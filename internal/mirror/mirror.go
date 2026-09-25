// Package mirror re-derives coordinator scheduling state from persisted
// session items, because the coordinator exposes no idle callback.
//
// The rules mirror harness/coordinator/loop.go at unreal-agent v0.1.1
// (b7c9bf1): the coordinator calls the model iff inputs are pending and no
// request is in flight; a finished tool call counts as a pending input
// (finishToolCall); an ItemTurn marks the pending inputs as the turn's inputs
// (loop.go:597-600); a response for the current turn delivers them
// (loop.go:615-617). isIdle = no model call, no pending inputs, no tool calls,
// no nonterminal ops (loop.go:272-275). The same approach is used by
// andreylukin/bough (Apache-2.0) go/internal/unreal/session/mirror.go.
package mirror

import (
	"strings"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
)

// Mirror is coordinator scheduling state rebuilt purely from store items.
// It is not safe for concurrent use; see Sync.
type Mirror struct {
	Pending     int
	Reasons     []string // pending: "input:<id>", "call:<id>", "heartbeat:<id>"
	TurnReasons []string // what the newest request answers
	LastTurn    session.TurnID
	Inflight    bool
	Outstanding map[string]*Call // call id -> call whose result the model has not had
}

// Call is a tool call whose operations have not all ended.
type Call struct {
	ID  string
	Ops []operation.ID
}

// New returns an empty mirror.
func New() *Mirror { return &Mirror{Outstanding: map[string]*Call{}} }

// Quiescent reports the coordinator has nothing left to do: no request in
// flight, nothing pending, no running tool call. This is the ACP turn-close
// rule (the caller additionally requires that everything it submitted has
// been observed).
func (m *Mirror) Quiescent() bool {
	return !m.Inflight && m.Pending == 0 && len(m.Outstanding) == 0
}

// Apply folds one persisted item into the mirror.
func (m *Mirror) Apply(item sessionstore.Item) {
	switch item.Kind {
	case sessionstore.ItemFork:
		clear(m.Outstanding)
	case sessionstore.ItemInput:
		input, _ := item.Data.(inbox.Input)
		switch input.Kind {
		case inbox.InputExternal:
			m.Pending++
			m.Reasons = append(m.Reasons, "input:"+string(input.ID))
		case inbox.InputControl:
			if message, err := input.DecodeControlMessage(); err == nil && message.Mode == inbox.Heartbeat {
				m.Pending++
				m.Reasons = append(m.Reasons, "heartbeat:"+string(input.ID))
			}
		}
	case sessionstore.ItemTurn:
		turn, _ := item.Data.(session.Turn)
		m.LastTurn = turn.ID
		m.Inflight = true
		m.TurnReasons = m.Reasons
		m.Reasons = nil
		m.Pending = 0
	case sessionstore.ItemModelResponse:
		response, _ := item.Data.(sessionstore.ModelResponse)
		if response.TurnID != m.LastTurn {
			return
		}
		m.Inflight = false
		for _, output := range response.Response.Output {
			call, ok := output.Data.(llm.ToolCall)
			if output.Type != llm.ItemToolCall || !ok {
				continue
			}
			m.Outstanding[call.CallID] = &Call{ID: call.CallID}
		}
	case sessionstore.ItemToolCallStatus:
		status, _ := item.Data.(sessionstore.ToolCallStatus)
		call, ok := m.Outstanding[status.CallID]
		if !ok {
			return
		}
		if status.Status.Error != "" || len(status.Status.WaitingFor) == 0 || StatusTerminal(status) {
			delete(m.Outstanding, status.CallID)
			m.Pending++
			m.Reasons = append(m.Reasons, "call:"+status.CallID)
			return
		}
		call.Ops = append([]operation.ID(nil), status.Status.WaitingFor...)
	}
}

// StatusTerminal reports whether every operation the status waits for ended.
func StatusTerminal(status sessionstore.ToolCallStatus) bool {
	byID := map[operation.ID]operation.Status{}
	for _, value := range status.Operations {
		byID[value.ID] = value.Status
	}
	for _, id := range status.Status.WaitingFor {
		if !OperationTerminal(byID[id]) {
			return false
		}
	}
	return true
}

// OperationTerminal reports whether status is final.
func OperationTerminal(status operation.Status) bool {
	switch status {
	case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
		return true
	}
	return false
}

// Sync is a locked replica applied inside the store observer, on the
// coordinator goroutine, so the Gate reads state never behind the
// coordinator's own next step.
type Sync struct {
	mu sync.Mutex
	m  *Mirror
}

// NewSync returns an empty replica.
func NewSync() *Sync { return &Sync{m: New()} }

// Apply folds an item.
func (s *Sync) Apply(item sessionstore.Item) {
	s.mu.Lock()
	s.m.Apply(item)
	s.mu.Unlock()
}

// TurnReasons is what the newest request answers.
func (s *Sync) TurnReasons() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.m.TurnReasons...)
}

// PendingReasons is what the next request would answer.
func (s *Sync) PendingReasons() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.m.Reasons...)
}

// Calls lists every call whose result the model has not had yet: still
// running, or finished and waiting for the next request.
func (s *Sync) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id := range s.m.Outstanding {
		ids = append(ids, id)
	}
	for _, reason := range s.m.Reasons {
		if kind, id, _ := strings.Cut(reason, ":"); kind == "call" {
			ids = append(ids, id)
		}
	}
	return ids
}

// Outstanding returns running calls and the operations each waits for.
func (s *Sync) Outstanding() map[string][]operation.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]operation.ID, len(s.m.Outstanding))
	for id, call := range s.m.Outstanding {
		out[id] = append([]operation.ID(nil), call.Ops...)
	}
	return out
}
