package mirror

import (
	"encoding/json/v2"
	"testing"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func input(id string) sessionstore.Item {
	payload, _ := json.Marshal("hi")
	return sessionstore.Item{Kind: sessionstore.ItemInput, Data: inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: payload}}
}

func turn(id string) sessionstore.Item {
	return sessionstore.Item{Kind: sessionstore.ItemTurn, Data: session.Turn{ID: session.TurnID(id)}}
}

func response(turnID string, calls ...string) sessionstore.Item {
	var out []llm.Item
	for _, c := range calls {
		out = append(out, llm.Item{Type: llm.ItemToolCall, Data: llm.ToolCall{CallID: c, Name: "Bash"}})
	}
	return sessionstore.Item{Kind: sessionstore.ItemModelResponse, Data: sessionstore.ModelResponse{TurnID: session.TurnID(turnID), Response: llm.Response{Stop: llm.StopComplete, Output: out}}}
}

func status(call string, op string, st operation.Status) sessionstore.Item {
	return sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{
		CallID: call, Status: tool.CallStatus{WaitingFor: []operation.ID{operation.ID(op)}},
		Operations: []operation.Operation{{ID: operation.ID(op), Status: st}},
	}}
}

func TestCloseRuleAcrossToolRoundTrip(t *testing.T) {
	m := New()
	if !m.Quiescent() {
		t.Fatal("empty mirror must be quiescent")
	}
	steps := []struct {
		item      sessionstore.Item
		quiescent bool
		name      string
	}{
		{input("i1"), false, "input pending"},
		{turn("t1"), false, "request in flight"},
		{response("t1", "c1"), false, "tool call outstanding"},
		{status("c1", "op1", operation.StatusReady), false, "op ready"},
		{status("c1", "op1", operation.StatusAwaiting), false, "op running"},
		{status("c1", "op1", operation.StatusCompleted), false, "result pending for model"},
		{turn("t2"), false, "second request in flight"},
		{response("t2"), true, "final text response"},
	}
	for _, s := range steps {
		m.Apply(s.item)
		if m.Quiescent() != s.quiescent {
			t.Fatalf("%s: quiescent=%v want %v (mirror %+v)", s.name, m.Quiescent(), s.quiescent, m)
		}
	}
	if len(m.TurnReasons) != 1 || m.TurnReasons[0] != "call:c1" {
		t.Fatalf("turn reasons = %v", m.TurnReasons)
	}
}

func TestStaleResponseIgnored(t *testing.T) {
	m := New()
	m.Apply(input("i1"))
	m.Apply(turn("t1"))
	m.Apply(input("i2")) // native steer supersedes t1
	m.Apply(turn("t2"))
	m.Apply(response("t1")) // late, dropped by the coordinator (loop.go:153-156)
	if !m.Inflight {
		t.Fatal("a superseded turn's response must not clear Inflight")
	}
	m.Apply(response("t2"))
	if !m.Quiescent() {
		t.Fatal("should be quiescent")
	}
}

func TestToolCallErrorCountsAsPendingResult(t *testing.T) {
	m := New()
	m.Apply(input("i1"))
	m.Apply(turn("t1"))
	m.Apply(response("t1", "c1"))
	m.Apply(sessionstore.Item{Kind: sessionstore.ItemToolCallStatus, Data: sessionstore.ToolCallStatus{CallID: "c1", Status: tool.CallStatus{Error: "bad args"}}})
	if m.Quiescent() || m.Pending != 1 || len(m.Outstanding) != 0 {
		t.Fatalf("mirror %+v", m)
	}
}
