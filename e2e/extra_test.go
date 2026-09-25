package e2e

import (
	"syscall"
	"uuid"

	"slices"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// session/close cancels the turn, drains tools and releases the lock
// in-process (the agent stays up).
func TestCloseSessionDrainsAndReleasesLock(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	done, pgid := startTermIgnoringTool(t, a, sid)
	start := time.Now()
	if _, err := a.conn.CloseSession(a.ctx(), acp.CloseSessionRequest{SessionId: sid}); err != nil {
		t.Fatalf("session/close: %v", err)
	}
	t.Logf("session/close took %s", time.Since(start).Round(time.Millisecond))
	if res := <-done; res.err != nil || res.resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("open prompt after close: %+v err=%v", res.resp, res.err)
	}
	if live := liveInGroup(pgid); len(live) > 0 {
		t.Fatalf("tool group survived session/close: %+v", live)
	}
	b := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	b.initialize()
	if _, err := b.conn.ResumeSession(b.ctx(), acp.ResumeSessionRequest{SessionId: sid, Cwd: ws}); err != nil {
		t.Fatalf("lock not released by session/close: %v", err)
	}
	if !a.alive() {
		t.Fatal("agent exited on session/close")
	}
	b.stop()
	a.stop()
}

// _acp-unreal/steer (queue mode) lands at the next response boundary and
// the ACP turn still ends once.
func TestSteerQueueMode(t *testing.T) {
	llm := newFakeLLM(t)
	llm.SlowDelay = 20 * time.Millisecond
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	done := a.promptAsync(sid, "SLOW story")
	waitFor(t, 10*time.Second, "streaming", func() bool { _, _, n := messageText(a.client.snapshot()); return n >= 2 })
	if _, err := a.conn.CallExtension(a.ctx(), "_acp-unreal/steer", map[string]any{"sessionId": sid, "text": "steered input"}); err != nil {
		t.Fatalf("steer: %v", err)
	}
	res := <-done
	if res.err != nil || res.resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("prompt: %+v err=%v", res.resp, res.err)
	}
	reqs := llm.Requests()
	if len(reqs) != 2 || reqs[1].LastText != "steered input" {
		t.Fatalf("requests = %+v", reqs)
	}
	if text, _, _ := messageText(a.client.snapshot()); !strings.HasSuffix(text, "Echo: steered input") || !strings.Contains(text, "slow words keep coming slow words keep coming") {
		t.Fatalf("queue mode must keep the first reply whole and append the steer reply: %q", text)
	}
	if !slices.Contains(reqs[1].UserTexts, "SLOW story") {
		t.Fatal("steer request lost the first message")
	}
	a.stop()
}

// allowlist mode: listed prefixes run unasked; others ask, and an
// unanswered ask expires as reject_once after --permission-timeout.
func TestAllowlistAndPermissionTimeout(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--permission-mode", "allowlist", "--allow", "echo,printf", "--permission-timeout", "300ms"}})
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	a.client.permission = func(acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		<-block // never answers in time
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}
	}
	a.initialize()
	sid := a.newSession()
	a.prompt(sid, "RUN[echo listed-ok]")
	if len(a.client.asked) != 0 {
		t.Fatalf("allowlisted command asked: %d", len(a.client.asked))
	}
	if trace := toolTrace(a.client.snapshot()); len(trace) != 3 || !strings.Contains(trace[2], "listed-ok") {
		t.Fatalf("trace = %q", trace)
	}
	marker := ws + "/not-listed"
	mark := a.client.mark()
	resp := a.prompt(sid, "RUN[touch "+marker+"]")
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stop = %s", resp.StopReason)
	}
	a.client.mu.Lock()
	asked := len(a.client.asked)
	a.client.mu.Unlock()
	if asked != 1 {
		t.Fatalf("unlisted command asks = %d", asked)
	}
	if trace := toolTrace(a.client.since(mark)); len(trace) != 2 || !strings.HasPrefix(trace[1], "tool_call_update:failed:") || !strings.Contains(trace[1], "timed out") {
		t.Fatalf("trace = %q", trace)
	}
	if exists(marker) {
		t.Fatal("timed-out command ran")
	}
	a.stop()
}

// messageIDs returns the agent message ids of updates in order of first use.
func agentMessageIDs(updates []acp.SessionUpdate) []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range updates {
		if c := u.AgentMessageChunk; c != nil && c.MessageId != nil && !seen[*c.MessageId] {
			seen[*c.MessageId] = true
			out = append(out, *c.MessageId)
		}
	}
	return out
}

func allMessageIDs(updates []acp.SessionUpdate) []string {
	var out []string
	for _, u := range updates {
		switch {
		case u.AgentMessageChunk != nil && u.AgentMessageChunk.MessageId != nil:
			out = append(out, *u.AgentMessageChunk.MessageId)
		case u.AgentThoughtChunk != nil && u.AgentThoughtChunk.MessageId != nil:
			out = append(out, *u.AgentThoughtChunk.MessageId)
		case u.UserMessageChunk != nil && u.UserMessageChunk.MessageId != nil:
			out = append(out, *u.UserMessageChunk.MessageId)
		}
	}
	return out
}

// Unstable ACP: message ids MUST be UUIDs and identify one message. gc
// reassembles transcript chunks by messageId, so ids must never repeat
// across prompts, session/close + session/load, or a restart, and live and
// replayed ids of the same message must match.
func TestMessageIDsAreUniqueUUIDsAndStableAcrossReplay(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	r1 := a.prompt(sid, "first")
	live1 := agentMessageIDs(a.client.snapshot())
	mark := a.client.mark()
	a.prompt(sid, "second")
	live2 := agentMessageIDs(a.client.since(mark))
	if len(live1) != 1 || len(live2) != 1 || live1[0] == live2[0] {
		t.Fatalf("live ids: first %q second %q", live1, live2)
	}
	for _, id := range allMessageIDs(a.client.snapshot()) {
		if _, err := uuid.Parse(id); err != nil {
			t.Fatalf("messageId %q is not a UUID", id)
		}
	}
	if r1.UserMessageId == nil {
		t.Fatal("no userMessageId")
	}
	if _, err := uuid.Parse(*r1.UserMessageId); err != nil {
		t.Fatalf("userMessageId %q is not a UUID", *r1.UserMessageId)
	}

	if _, err := a.conn.CloseSession(a.ctx(), acp.CloseSessionRequest{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	mark = a.client.mark()
	if _, err := a.conn.LoadSession(a.ctx(), acp.LoadSessionRequest{SessionId: sid, Cwd: ws, McpServers: []acp.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	replay := a.client.since(mark)
	if got := agentMessageIDs(replay); !slices.Equal(got, append(slices.Clone(live1), live2...)) {
		t.Fatalf("replayed ids %q, live ids %q + %q", got, live1, live2)
	}
	var users []string
	for _, u := range replay {
		if u.UserMessageChunk != nil && u.UserMessageChunk.MessageId != nil {
			users = append(users, *u.UserMessageChunk.MessageId)
		}
	}
	if len(users) != 2 || users[0] != *r1.UserMessageId {
		t.Fatalf("replayed user ids %q, first userMessageId %q", users, *r1.UserMessageId)
	}
	mark = a.client.mark()
	a.prompt(sid, "third")
	live3 := agentMessageIDs(a.client.since(mark))
	if len(live3) != 1 || slices.Contains(live1, live3[0]) || slices.Contains(live2, live3[0]) {
		t.Fatalf("after close+load the id repeats: %q", live3)
	}
	a.terminate()

	b := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	b.initialize()
	if _, err := b.conn.ResumeSession(b.ctx(), acp.ResumeSessionRequest{SessionId: sid, Cwd: ws}); err != nil {
		t.Fatal(err)
	}
	b.prompt(sid, "fourth")
	live4 := agentMessageIDs(b.client.snapshot())
	if len(live4) != 1 || slices.Contains(append(append(slices.Clone(live1), live2...), live3...), live4[0]) {
		t.Fatalf("after restart the id repeats: %q", live4)
	}
	b.stop()
}

// A cancelled streaming reply keeps its partial text; it is persisted under
// the provider's response id, so it replays under the messageId it streamed
// with.
func TestCancelledPartialReplaysUnderLiveMessageID(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	done := a.promptAsync(sid, "SLOW please")
	waitFor(t, 10*time.Second, "streaming", func() bool { _, _, n := messageText(a.client.snapshot()); return n >= 2 })
	if err := a.conn.Cancel(a.ctx(), acp.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.err != nil || res.resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("prompt = %+v err=%v", res.resp, res.err)
	}
	live := agentMessageIDs(a.client.snapshot())
	if _, err := a.conn.CloseSession(a.ctx(), acp.CloseSessionRequest{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	mark := a.client.mark()
	if _, err := a.conn.LoadSession(a.ctx(), acp.LoadSessionRequest{SessionId: sid, Cwd: ws, McpServers: []acp.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	if replay := agentMessageIDs(a.client.since(mark)); len(live) != 1 || !slices.Equal(replay, live) {
		t.Fatalf("live ids %q, replayed ids %q", live, replay)
	}
	a.stop()
}

func configOption(t *testing.T, options []acp.SessionConfigOption, id string) *acp.SessionConfigOptionSelect {
	t.Helper()
	for _, o := range options {
		if o.Select != nil && string(o.Select.Id) == id {
			return o.Select
		}
	}
	t.Fatalf("config option %q missing: %+v", id, options)
	return nil
}

func setConfig(a *agentProc, sid acp.SessionId, id, value string) (acp.SetSessionConfigOptionResponse, error) {
	return a.conn.SetSessionConfigOption(a.ctx(), acp.SetSessionConfigOptionRequest{ValueId: &acp.SetSessionConfigOptionValueId{
		SessionId: sid, ConfigId: acp.SessionConfigId(id), Value: acp.SessionConfigValueId(value),
	}})
}

// The launch --model is operator configuration: a restart with a new
// --model reaches an existing bound session. Only an explicit client
// set_config_option model persists, and it must be one of --models.
func TestModelFollowsLaunchFlagUnlessClientOverrides(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	env := []string{"GC_SESSION_ID=model-s", "GC_CONTINUATION_EPOCH=1"}
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, env: env})
	a.initialize()
	resp, err := a.conn.NewSession(a.ctx(), acp.NewSessionRequest{Cwd: ws, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	sid := resp.SessionId
	model := configOption(t, resp.ConfigOptions, "model")
	if model.Category == nil || *model.Category != acp.SessionConfigOptionCategoryModel || model.CurrentValue != "fake-1" {
		t.Fatalf("model option = %+v", model)
	}
	thought := configOption(t, resp.ConfigOptions, "thought_level")
	if thought.Category == nil || *thought.Category != acp.SessionConfigOptionCategoryThoughtLevel || thought.CurrentValue != "default" {
		t.Fatalf("thought_level option = %+v", thought)
	}
	a.prompt(sid, "one")
	if got := llm.Requests()[0]; got.Model != "fake-1" || got.Effort != "" {
		t.Fatalf("request 1 model %q effort %q", got.Model, got.Effort)
	}
	a.terminate()

	// Operator changes the launch model: the existing session follows it.
	b := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, env: env, args: []string{"--model", "fake-3", "--models", "fake-2,fake-3"}})
	b.initialize()
	resp, err = b.conn.NewSession(b.ctx(), acp.NewSessionRequest{Cwd: ws, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := configOption(t, resp.ConfigOptions, "model").CurrentValue; got != "fake-3" {
		t.Fatalf("after relaunch with --model fake-3 currentValue = %q", got)
	}
	b.prompt(sid, "two")
	reqs := llm.Requests()
	if got := reqs[len(reqs)-1].Model; got != "fake-3" {
		t.Fatalf("request after relaunch used model %q", got)
	}
	if _, err := setConfig(b, sid, "model", "not-offered"); err == nil || !strings.Contains(err.Error(), "Invalid params") {
		t.Fatalf("unoffered model: err=%v", err)
	}
	set, err := setConfig(b, sid, "model", "fake-2")
	if err != nil {
		t.Fatal(err)
	}
	if got := configOption(t, set.ConfigOptions, "model").CurrentValue; got != "fake-2" {
		t.Fatalf("after set currentValue = %q", got)
	}
	set, err = setConfig(b, sid, "thought_level", "low")
	if err != nil {
		t.Fatal(err)
	}
	if got := configOption(t, set.ConfigOptions, "thought_level").CurrentValue; got != "low" {
		t.Fatalf("thought_level currentValue = %q", got)
	}
	b.prompt(sid, "three")
	reqs = llm.Requests()
	if got := reqs[len(reqs)-1]; got.Model != "fake-2" || got.Effort != "low" {
		t.Fatalf("request after set: model %q effort %q", got.Model, got.Effort)
	}
	b.terminate()

	// The explicit client choice survives a relaunch with another --model.
	c := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, env: env, args: []string{"--model", "fake-4", "--models", "fake-2,fake-4"}})
	c.initialize()
	resp, err = c.conn.NewSession(c.ctx(), acp.NewSessionRequest{Cwd: ws, McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := configOption(t, resp.ConfigOptions, "model").CurrentValue; got != "fake-2" {
		t.Fatalf("explicit override lost: currentValue = %q", got)
	}
	if _, err := setConfig(c, sid, "thought_level", "default"); err != nil {
		t.Fatal(err)
	}
	c.prompt(sid, "four")
	reqs = llm.Requests()
	if got := reqs[len(reqs)-1]; got.Model != "fake-2" || got.Effort != "" {
		t.Fatalf("request after relaunch: model %q effort %q", got.Model, got.Effort)
	}
	c.stop()
}

// An idle agent shuts down on SIGTERM promptly: nothing to cancel, so
// shutdown must not wait for --cancel-grace (1s in these tests).
func TestIdleSigtermExitsWithoutWaitingForCancelGrace(t *testing.T) {
	for name, prompts := range map[string][]string{
		"after-text":     {"hello"},
		"after-tool":     {"RUN[echo hi]"},
		"after-both":     {"RUN[echo hi]", "hello"},
		"never-prompted": nil,
	} {
		t.Run(name, func(t *testing.T) { idleSigterm(t, prompts) })
	}
}

func idleSigterm(t *testing.T, prompts []string) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	for _, p := range prompts {
		a.prompt(sid, p)
	}
	start := time.Now()
	if err := a.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	a.waitExit(10 * time.Second)
	took := time.Since(start)
	t.Logf("idle SIGTERM exit took %s", took.Round(time.Millisecond))
	if a.exitErr != nil {
		t.Fatalf("exit: %v", a.exitErr)
	}
	if took >= 900*time.Millisecond {
		t.Fatalf("idle shutdown took %s; it waited for cancel-grace", took)
	}
}

// --op-timeout ends even a TERM-ignoring command within op-timeout +
// cancel-grace instead of the library's fixed 5s escalation.
func TestOpTimeoutKillsTermIgnoringTool(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--op-timeout", "500ms"}})
	a.initialize()
	sid := a.newSession()
	start := time.Now()
	resp := a.prompt(sid, `RUN[sh -c 'trap "" TERM; sleep 30']`)
	took := time.Since(start)
	t.Logf("prompt with a timed-out tool took %s", took.Round(time.Millisecond))
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stopReason = %s", resp.StopReason)
	}
	if trace := toolTrace(a.client.snapshot()); len(trace) != 3 || !strings.HasPrefix(trace[2], "tool_call_update:failed:") {
		t.Fatalf("trace = %q", trace)
	}
	if took > 3500*time.Millisecond {
		t.Fatalf("the timed-out tool ended after %s (op-timeout 500ms + cancel-grace 1s)", took)
	}
	a.stop()
}
