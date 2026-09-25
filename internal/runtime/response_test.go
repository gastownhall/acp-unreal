package runtime

import (
	"errors"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

// ACP prompt-turn: after session/cancel the agent MUST answer stopReason
// cancelled and must not surface the abort (or an earlier provider error)
// as a JSON-RPC error.
func TestCancelledTurnNeverAnswersAnError(t *testing.T) {
	resp, err := turnResult{stop: acp.StopReasonCancelled, err: errors.New("provider failed")}.response("u1")
	if err != nil || resp.StopReason != acp.StopReasonCancelled {
		t.Fatalf("response = %+v err = %v", resp, err)
	}
	if _, err := (turnResult{stop: acp.StopReasonEndTurn, err: errors.New("provider failed")}).response("u1"); err == nil {
		t.Fatal("an uncancelled failed turn must answer an error")
	}
}
