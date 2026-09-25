package runtime

import (
	"errors"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// ACP prompt-turn: after the client's session/cancel the agent MUST answer
// stopReason cancelled and must not surface the abort (or an earlier
// provider error) as a JSON-RPC error.
func TestClientCancelledTurnNeverAnswersAnError(t *testing.T) {
	for _, stop := range []acp.StopReason{acp.StopReasonCancelled, acp.StopReasonEndTurn, ""} {
		resp, err := turnResult{stop: stop, err: errors.New("provider failed"), clientCancelled: true}.response("u1")
		if err != nil || resp.StopReason != acp.StopReasonCancelled {
			t.Fatalf("stop %q: response = %+v err = %v", stop, resp, err)
		}
	}
}

// ACP reserves cancelled for the client: an abort the agent started itself
// answers the provider error.
func TestAgentAbortedTurnAnswersTheError(t *testing.T) {
	if _, err := (turnResult{stop: acp.StopReasonCancelled, err: errors.New("provider failed")}).response("u1"); err == nil {
		t.Fatal("an agent-aborted turn must answer an error, not cancelled")
	}
	if _, err := (turnResult{stop: acp.StopReasonCancelled}).response("u1"); err == nil {
		t.Fatal("a cancelled turn the client never cancelled must answer an error")
	}
	if _, err := (turnResult{stop: acp.StopReasonEndTurn, err: errors.New("provider failed")}).response("u1"); err == nil {
		t.Fatal("an uncancelled failed turn must answer an error")
	}
	resp, err := turnResult{stop: acp.StopReasonEndTurn}.response("u1")
	if err != nil || resp.StopReason != acp.StopReasonEndTurn {
		t.Fatalf("response = %+v err = %v", resp, err)
	}
}
