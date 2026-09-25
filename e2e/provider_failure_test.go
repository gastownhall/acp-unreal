package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/gastownhall/acp-unreal/internal/testfake/responses"
)

func writeSSE(w http.ResponseWriter, event map[string]any) {
	data, _ := json.Marshal(event)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	w.(http.Flusher).Flush()
}

// failingProvider answers request 1 with one Bash function_call per command
// and every later request with HTTP 500.
func failingProvider(t *testing.T, commands []string) (*fakeLLM, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if n.Add(1) >= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"provider exploded","type":"server_error"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		id := "resp_fail_1"
		writeSSE(w, map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": "fake-1"}})
		var items []any
		for i, command := range commands {
			args, _ := json.Marshal(map[string]string{"command": command})
			item := map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", i), "call_id": fmt.Sprintf("call_%d", i), "name": "Bash", "arguments": string(args), "status": "completed"}
			writeSSE(w, map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
			items = append(items, item)
		}
		writeSSE(w, map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "status": "completed", "model": "fake-1", "output": items, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}})
	})
	mux := http.NewServeMux()
	mux.Handle("POST /v1/responses", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fakeLLM{Fake: responses.New(), srv: srv}, &n
}

func requireProviderError(t *testing.T, resp acp.PromptResponse, err error) {
	t.Helper()
	var rpc *acp.RequestError
	if !errors.As(err, &rpc) || rpc.Code != -32603 {
		t.Fatalf("prompt = stopReason %q err %v, want JSON-RPC -32603", resp.StopReason, err)
	}
	if !strings.Contains(fmt.Sprint(rpc.Data), "provider exploded") {
		t.Fatalf("error data %v does not carry the provider error", rpc.Data)
	}
}

// A provider failure with nothing outstanding is a JSON-RPC error.
func TestProviderFailureAnswersError(t *testing.T) {
	llm, _ := failingProvider(t, []string{"echo only"})
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--max-attempts", "1"}})
	a.initialize()
	sid := a.newSession()
	resp, err := a.conn.Prompt(a.ctx(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("go")}})
	requireProviderError(t, resp, err)
	a.stop()
}

// Two parallel calls: the fast one finishes, the model is called while the
// slow one still runs, and that request fails. The agent aborts the slow tool
// itself; the client never cancelled, so ACP's "cancelled" would be a lie.
func TestProviderFailureWithOutstandingToolAnswersError(t *testing.T) {
	llm, n := failingProvider(t, []string{"echo fast", "sleep 20; echo slow"})
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--max-attempts", "1"}})
	a.initialize()
	sid := a.newSession()
	start := time.Now()
	resp, err := a.conn.Prompt(a.ctx(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock("go")}})
	requireProviderError(t, resp, err)
	if n.Load() != 2 {
		t.Fatalf("provider requests = %d, want 2 (the failure must happen while a tool runs)", n.Load())
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("turn took %s: the outstanding tool was not aborted", took)
	}
	a.stop()
}

// A provider failure followed by the client's own session/cancel answers
// cancelled: the client asked for it (ACP: MUST answer cancelled).
func TestProviderFailureThenClientCancelAnswersCancelled(t *testing.T) {
	// The slow tool ignores SIGTERM, so the agent's own abort lasts the
	// whole 5s cancel grace and the client's cancel lands inside it.
	llm, n := failingProvider(t, []string{"echo fast", "trap '' TERM; sleep 20; echo slow"})
	state, ws := newDirs(t)
	a := startAgent(t, llm, agentOpts{stateDir: state, cwd: ws, args: []string{"--max-attempts", "1", "--cancel-grace", "5s"}})
	a.initialize()
	sid := a.newSession()
	ch := a.promptAsync(sid, "go")
	deadline := time.Now().Add(15 * time.Second)
	for n.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("provider never saw the failing request")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Let the agent start its own abort, then cancel inside the 5s grace.
	time.Sleep(300 * time.Millisecond)
	if err := a.conn.Cancel(a.ctx(), acp.CancelNotification{SessionId: sid}); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-ch:
		if r.err != nil || r.resp.StopReason != acp.StopReasonCancelled {
			t.Fatalf("prompt = stopReason %q err %v, want cancelled", r.resp.StopReason, r.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("prompt never answered")
	}
	a.stop()
}
