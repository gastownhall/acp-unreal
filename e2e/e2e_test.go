package e2e

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// E1: initialize, new, prompt "hello".
func TestE1PromptStreamsAndReportsUsageAndServedModel(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	resp := a.prompt(sid, "hello")

	updates := a.client.snapshot()
	text, ids, chunks := messageText(updates)
	if chunks < 2 || len(ids) != 1 {
		t.Fatalf("want >=2 agent_message_chunk with one messageId; chunks=%d ids=%v", chunks, ids)
	}
	if text != "Echo: hello" {
		t.Fatalf("message = %q", text)
	}
	if thoughtCount(updates) < 1 {
		t.Fatal("no agent_thought_chunk")
	}
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stopReason = %s", resp.StopReason)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens <= 0 || resp.Usage.InputTokens <= 0 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
	if got, _ := resp.Meta["model"].(string); got != "fake-1-served" {
		t.Fatalf("_meta.model = %v", resp.Meta)
	}
	if llm.Count() != 1 {
		t.Fatalf("provider requests = %d", llm.Count())
	}
	a.stop()
}

// E2: ask mode, allowed Bash.
func TestE2AskModeAllowRunsToolAfterPermission(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--permission-mode", "ask"}})
	a.initialize()
	sid := a.newSession()
	resp := a.prompt(sid, "RUN[echo ok-$((6*7))]")
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stopReason = %s", resp.StopReason)
	}
	updates := a.client.snapshot()
	trace := toolTrace(updates)
	want := []string{"tool_call:pending:execute", "tool_call_update:in_progress:", "tool_call_update:completed:"}
	if len(trace) != 3 || trace[0] != want[0] || trace[1] != want[1] || !strings.HasPrefix(trace[2], want[2]) || !strings.Contains(trace[2], "ok-42") {
		t.Fatalf("tool trace = %q", trace)
	}
	if text, _, _ := messageText(updates); text != "Tool result: ok-42\n" && !strings.HasPrefix(text, "Tool result: ok-42") {
		t.Fatalf("final text = %q", text)
	}
	// Wire order: request_permission strictly before the in_progress update.
	lines := a.tee.all()
	ask := lineIndex(lines, 0, contains(`"session/request_permission"`))
	running := lineIndex(lines, 0, contains(`"tool_call_update"`, `"in_progress"`))
	call := lineIndex(lines, 0, contains(`"sessionUpdate":"tool_call"`))
	if ask < 0 || running < 0 || !(call < ask && ask < running) {
		t.Fatalf("order tool_call=%d request_permission=%d in_progress=%d", call, ask, running)
	}
	if len(a.client.asked) != 1 || len(a.client.asked[0].Options) != 4 {
		t.Fatalf("permission requests = %+v", a.client.asked)
	}
	a.stop()
}

// E3: ask mode, rejected Bash never runs; the model sees the rejection.
func TestE3AskModeRejectNeverRuns(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	marker := filepath.Join(t.TempDir(), "marker")
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--permission-mode", "ask"}})
	a.client.permission = func(acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("reject_once")}
	}
	a.initialize()
	sid := a.newSession()
	resp := a.prompt(sid, "RUN[touch "+marker+"]")
	if resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("stopReason = %s", resp.StopReason)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected command ran (marker stat err=%v)", err)
	}
	reqs := llm.Requests()
	last := reqs[len(reqs)-1]
	if last.LastKind != "function_call_output" || !strings.Contains(last.LastText, "rejected") {
		t.Fatalf("model did not receive the rejection: %+v", last)
	}
	trace := toolTrace(a.client.snapshot())
	if len(trace) != 2 || !strings.HasPrefix(trace[1], "tool_call_update:failed:") {
		t.Fatalf("tool trace = %q", trace)
	}
	a.stop()
}

// startTermIgnoringTool prompts a TERM-ignoring command and returns its pgid
// once the sleep is running (so the trap is installed).
func startTermIgnoringTool(t *testing.T, a *agentProc, sid acp.SessionId) (<-chan promptResult, int) {
	t.Helper()
	done := a.promptAsync(sid, `RUN[sh -c 'trap "" TERM; sleep 60']`)
	waitFor(t, 10*time.Second, "tool in_progress", func() bool {
		return slices.Contains(toolTrace(a.client.snapshot()), "tool_call_update:in_progress:")
	})
	var pgid int
	waitFor(t, 10*time.Second, "sleep child", func() bool {
		for _, p := range markedProcs(a.opts.marker, a.cmd.Process.Pid) {
			if p.comm == "sleep" {
				pgid = p.pgid
				return true
			}
		}
		return false
	})
	if pgid == a.cmd.Process.Pid {
		t.Fatalf("tool runs in the agent's process group")
	}
	return done, pgid
}

// E4: session/cancel during a TERM-ignoring tool.
func TestE4CancelKillsToolFlushesFailedAndMutesModel(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	done, pgid := startTermIgnoringTool(t, a, sid)
	before := llm.Count()
	cancelAt := time.Now()
	if err := a.conn.Cancel(a.ctx(), acp.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	var res promptResult
	select {
	case res = <-done:
	case <-time.After(3 * time.Second): // cancel-grace 1s + 2s
		t.Fatal("prompt not answered within cancel-grace + 2s")
	}
	if res.err != nil || res.resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("prompt = %+v err=%v", res.resp, res.err)
	}
	t.Logf("cancelled answered after %s", res.at.Sub(cancelAt).Round(time.Millisecond))
	lines := a.tee.all()
	failed := lineIndex(lines, 0, contains(`"tool_call_update"`, `"status":"failed"`))
	answer := lineIndex(lines, 0, contains(`"stopReason":"cancelled"`))
	if failed < 0 || answer < 0 || failed > answer {
		t.Fatalf("tool_call_update(failed) line %d must precede the cancelled response line %d", failed, answer)
	}
	if live := liveInGroup(pgid); len(live) > 0 {
		t.Fatalf("tool process group %d still has live members: %+v", pgid, live)
	}
	time.Sleep(1500 * time.Millisecond)
	if after := llm.Count(); after != before {
		t.Fatalf("provider requests changed after cancel: %d -> %d", before, after)
	}
	if !a.alive() {
		t.Fatal("agent died")
	}
	next := a.prompt(sid, "are you there")
	if next.StopReason != acp.StopReasonEndTurn || llm.Count() != before+1 {
		t.Fatalf("next prompt: %s requests=%d", next.StopReason, llm.Count())
	}
	a.stop()
}

// E5: SIGINT to the agent's process group mid-turn (gc Interrupt).
func TestE5SigintCancelsTurnAndAgentStaysAlive(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	done := a.promptAsync(sid, "SLOW please")
	waitFor(t, 10*time.Second, "streaming", func() bool { _, _, n := messageText(a.client.snapshot()); return n >= 2 })
	if err := syscall.Kill(-a.cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	var res promptResult
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt not answered after SIGINT")
	}
	if res.err != nil || res.resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("prompt = %+v err=%v", res.resp, res.err)
	}
	time.Sleep(300 * time.Millisecond)
	if !a.alive() {
		t.Fatal("agent exited on SIGINT")
	}
	if !strings.Contains(a.stderr.String(), "SIGINT: cancelling active turns") {
		t.Fatal("the cancel did not come from the SIGINT handler")
	}
	mark := a.client.mark()
	next := a.prompt(sid, "hello again")
	if next.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("next stopReason = %s", next.StopReason)
	}
	if text, _, _ := messageText(a.client.since(mark)); text != "Echo: hello again" {
		t.Fatalf("next text = %q", text)
	}
	a.stop()
}

// E6: persistence across processes; session/load replays with zero requests.
func TestE6LoadReplaysHistoryWithoutProviderCalls(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	a.prompt(sid, "remember zebra")
	a.prompt(sid, "RUN[echo tool-in-history]")
	a.terminate()

	before := llm.Count()
	b := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	b.initialize()
	if _, err := b.conn.LoadSession(b.ctx(), acp.LoadSessionRequest{SessionId: sid, Cwd: ws, McpServers: []acp.McpServer{}}); err != nil {
		t.Fatalf("session/load: %v", err)
	}
	replay := b.client.snapshot()
	if llm.Count() != before {
		t.Fatalf("session/load made provider requests: %d -> %d", before, llm.Count())
	}
	var users []string
	for _, u := range replay {
		if u.UserMessageChunk != nil && u.UserMessageChunk.Content.Text != nil {
			users = append(users, u.UserMessageChunk.Content.Text.Text)
		}
	}
	if len(users) != 2 || users[0] != "remember zebra" {
		t.Fatalf("replayed user messages = %q", users)
	}
	text, _, _ := messageText(replay)
	if !strings.Contains(text, "Echo: remember zebra") || !strings.Contains(text, "Tool result: tool-in-history") {
		t.Fatalf("replayed agent text = %q", text)
	}
	trace := toolTrace(replay)
	if len(trace) != 2 || !strings.HasPrefix(trace[1], "tool_call_update:completed:") {
		t.Fatalf("replayed tool trace = %q", trace)
	}
	b.prompt(sid, "what did I ask you to remember")
	reqs := llm.Requests()
	last := reqs[len(reqs)-1]
	if !slices.Contains(last.UserTexts, "remember zebra") {
		t.Fatalf("next prompt's LLM input lacks history: %q", last.UserTexts)
	}
	b.stop()
}

// E7a: gc bound mode via GC_SESSION_ID + GC_CONTINUATION_EPOCH.
func TestE7aGCBoundModeResumesPerEpoch(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	env1 := []string{"GC_SESSION_ID=ga-t/1", "GC_CONTINUATION_EPOCH=1"}
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, env: env1})
	a.initialize()
	sid := a.newSession()
	if ok, _ := filepath.Match("gc-ga-t-1-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-e1", string(sid)); !ok {
		t.Fatalf("bound id = %q", sid)
	}
	a.prompt(sid, "first context")
	a.terminate()

	b := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, env: env1})
	b.initialize()
	if again := b.newSession(); again != sid {
		t.Fatalf("restart id = %q, want %q", again, sid)
	}
	if n := len(b.client.snapshot()); n != 0 {
		t.Fatalf("bound open replayed %d updates", n)
	}
	b.prompt(sid, "second")
	if reqs := llm.Requests(); !slices.Contains(reqs[len(reqs)-1].UserTexts, "first context") {
		t.Fatalf("context not retained: %q", reqs[len(reqs)-1].UserTexts)
	}
	b.terminate()

	c := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, env: []string{"GC_SESSION_ID=ga-t/1", "GC_CONTINUATION_EPOCH=2"}})
	c.initialize()
	fresh := c.newSession()
	if fresh == sid || !strings.HasSuffix(string(fresh), "-e2") {
		t.Fatalf("epoch 2 id = %q", fresh)
	}
	c.prompt(fresh, "third")
	if reqs := llm.Requests(); slices.Contains(reqs[len(reqs)-1].UserTexts, "first context") {
		t.Fatal("epoch bump did not start a fresh conversation")
	}
	c.stop()
}

// E7b: --session-id / --resume, and --resume of an unknown id.
func TestE7bExplicitBoundModes(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--session-id", "kilo-1"}})
	a.initialize()
	if sid := a.newSession(); sid != "kilo-1" {
		t.Fatalf("--session-id returned %q", sid)
	}
	a.prompt("kilo-1", "kilo context")
	a.terminate()

	dup := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--session-id", "kilo-1"}})
	dup.initialize()
	if _, err := dup.conn.NewSession(dup.ctx(), acp.NewSessionRequest{Cwd: ws, McpServers: []acp.McpServer{}}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("--session-id on an existing id: err=%v", err)
	}
	dup.stop()

	b := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--resume", "kilo-1"}})
	b.initialize()
	if sid := b.newSession(); sid != "kilo-1" {
		t.Fatalf("--resume returned %q", sid)
	}
	b.prompt("kilo-1", "and now")
	if reqs := llm.Requests(); !slices.Contains(reqs[len(reqs)-1].UserTexts, "kilo context") {
		t.Fatalf("--resume lost context: %q", reqs[len(reqs)-1].UserTexts)
	}
	b.stop()

	u := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--resume", "no-such-session"}})
	_, err := u.conn.Initialize(u.ctx(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err == nil {
		t.Fatal("initialize answered for --resume unknown")
	}
	u.waitExit(5 * time.Second)
	if code := u.cmd.ProcessState.ExitCode(); code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !strings.Contains(u.stderr.String(), "unknown session") {
		t.Fatalf("stderr = %q", u.stderr.String())
	}
}

// E7c: a second concurrent process on the same session fails "session busy".
func TestE7cConcurrentOpenIsBusy(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--session-id", "busy-1"}})
	a.initialize()
	a.newSession()
	b := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--resume", "busy-1"}})
	b.initialize()
	_, err := b.conn.NewSession(b.ctx(), acp.NewSessionRequest{Cwd: ws, McpServers: []acp.McpServer{}})
	if err == nil || !strings.Contains(err.Error(), "session busy") {
		t.Fatalf("second opener: err=%v", err)
	}
	// session/load of the same id from a third, unbound process is busy too.
	c := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	c.initialize()
	if _, err := c.conn.LoadSession(c.ctx(), acp.LoadSessionRequest{SessionId: "busy-1", Cwd: ws, McpServers: []acp.McpServer{}}); err == nil || !strings.Contains(err.Error(), "session busy") {
		t.Fatalf("load while busy: err=%v", err)
	}
	a.stop()
	// Lock released on exit: now b can open it.
	if _, err := b.conn.LoadSession(b.ctx(), acp.LoadSessionRequest{SessionId: "busy-1", Cwd: ws, McpServers: []acp.McpServer{}}); err != nil {
		t.Fatalf("load after release: %v", err)
	}
	b.stop()
	c.stop()
}

// E8: stdin EOF during a TERM-ignoring tool: exit within budget, no orphans.
func TestE8StdinEOFShutdownLeavesNoOrphans(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	_, pgid := startTermIgnoringTool(t, a, sid)
	a.stdin.Close()
	took := a.waitExit(4*time.Second + time.Second)
	t.Logf("agent exited %s after stdin EOF (exit %v)", took.Round(time.Millisecond), a.exitErr)
	if a.exitErr != nil {
		t.Fatalf("exit status: %v", a.exitErr)
	}
	time.Sleep(100 * time.Millisecond)
	if live := liveInGroup(pgid); len(live) > 0 {
		t.Fatalf("orphans in tool group %d: %+v", pgid, live)
	}
	if live := markedProcs(a.opts.marker, -1); len(live) > 0 {
		t.Fatalf("processes carrying the marker env survive: %+v", live)
	}
}

// E8b: the client dies (stdin EOF plus broken stdout AND stderr, which is
// what every ACP agent sees when gc dies) during a TERM-ignoring tool: the
// agent must not die of SIGPIPE before its graceful shutdown kills the tool.
func TestE8bClientDeathLeavesNoOrphans(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	_, pgid := startTermIgnoringTool(t, a, sid)
	a.clientDeath()
	took := a.waitExit(4*time.Second + time.Second)
	t.Logf("agent exited %s after client death (exit %v)", took.Round(time.Millisecond), a.exitErr)
	if a.exitErr != nil {
		t.Fatalf("exit status: %v", a.exitErr)
	}
	waitFor(t, 2*time.Second, "tool group gone", func() bool { return len(liveInGroup(pgid)) == 0 })
	if live := markedProcs(a.opts.marker, -1); len(live) > 0 {
		t.Fatalf("processes carrying the marker env survive: %+v", live)
	}
}

// E9: credential scrub. Tools can read their own environment AND their
// parent's /proc/<pid>/environ (the exec-time block, which os.Unsetenv does
// not change), so the agent re-execs itself with a scrubbed environment and
// receives the key over an inherited pipe. GC_* is kept.
func TestE9CredentialScrubCoversOwnAndParentEnviron(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, env: []string{
		"ACP_UNREAL_API_KEY=dummy-key-A", "OPENAI_API_KEY=dummy-key-B", "OLLAMA_API_KEY2=dummy-key-C",
		"GC_SESSION_ID=gc-test", "GC_INSTANCE_TOKEN=gc-tok-kept",
	}})
	a.initialize()
	sid := a.newSession()
	a.prompt(sid, `RUN[echo own=$(env | grep -c dummy-key) parent=$(tr '\0' '\n' </proc/$PPID/environ | grep -c dummy-key) $GC_INSTANCE_TOKEN pp=$ACP_UNREAL_PARENT_PID]`)
	trace := toolTrace(a.client.snapshot())
	if len(trace) != 3 {
		t.Fatalf("trace = %q", trace)
	}
	if out := trace[2]; !strings.Contains(out, fmt.Sprintf("own=0 parent=0 gc-tok-kept pp=%d", a.cmd.Process.Pid)) {
		t.Fatalf("tool output = %q", out)
	}
	if n := environHits(t, a.cmd.Process.Pid, "dummy-key"); n != 0 {
		t.Fatalf("/proc/<agent>/environ still holds %d credential values", n)
	}
	if got := llm.Requests()[0].Authorization; got != "Bearer dummy-key-A" {
		t.Fatal("the provider did not receive the key passed across the re-exec")
	}
	a.stop()
}

// E9b: --api-key-file keeps the key out of every environment; a key file
// readable by group or others is refused.
func TestE9bAPIKeyFile(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	keyFile := filepath.Join(filepath.Dir(state), "key")
	if err := os.WriteFile(keyFile, []byte("dummy-file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--api-key-file", keyFile}})
	a.initialize()
	sid := a.newSession()
	a.prompt(sid, "hello")
	if got := llm.Requests()[0].Authorization; got != "Bearer dummy-file-key" {
		t.Fatal("the provider did not receive the key from --api-key-file")
	}
	if n := environHits(t, a.cmd.Process.Pid, "dummy-file-key"); n != 0 {
		t.Fatalf("key from file leaked into the environment (%d hits)", n)
	}
	a.stop()

	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	b := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--api-key-file", keyFile}})
	b.waitExit(5 * time.Second)
	if code := b.cmd.ProcessState.ExitCode(); code != 2 || !strings.Contains(b.stderr.String(), "--api-key-file") {
		t.Fatalf("world-readable key file: exit %d stderr %q", code, b.stderr.String())
	}
}

// E10: huge tool output and a huge prompt still yield only JSON-RPC lines
// under 1 MiB (checkStreams runs for every agent in every test).
func TestE10StdoutLinesBounded(t *testing.T) {
	llm := newFakeLLM(t)
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	a.initialize()
	sid := a.newSession()
	a.prompt(sid, "RUN[head -c 3000000 /dev/zero | tr '\\0' x]")
	a.prompt(sid, strings.Repeat("y", 2<<20)+" end")
	a.terminate()
	b := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws})
	b.initialize()
	if _, err := b.conn.LoadSession(b.ctx(), acp.LoadSessionRequest{SessionId: sid, Cwd: ws, McpServers: []acp.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	longest := 0
	for _, agent := range []*agentProc{a, b} {
		for _, line := range agent.tee.all() {
			longest = max(longest, len(line))
		}
	}
	t.Logf("longest stdout line: %d bytes", longest)
	if longest >= 1<<20 {
		t.Fatalf("stdout line of %d bytes", longest)
	}
	b.stop()
}
