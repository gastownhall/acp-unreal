package e2e

import (
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
