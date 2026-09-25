// Package runtime hosts one Unreal Agent harness session inside the
// long-lived acp-unreal process: the localfile store, a session-lifetime
// operation manager, lazily built coordinator "generations", the ACP
// turn-close rule, cancel, replay and close.
//
// Library facts this package depends on (unreal-agent v0.1.1, b7c9bf1):
//   - coordinator.Run idles until stopped; the stock runner is one-shot only
//     because it queues StopWhenIdle (cmd/internal/agentrunner/run.go:397-405).
//   - The coordinator is single-use (coordinator.go:30-33); a generation is
//     Resume -> inbox(seen ids) -> builder -> Run, rebuilt after Run ends.
//   - An Unreal turn is ONE model request and there is no idle callback, so
//     the end of an ACP turn is derived from a mirror of coordinator
//     scheduling fed by store observers (see package mirror).
//   - localfile observers fire on Append* but not SaveOperation, so
//     in_progress comes from the ops decorator's tap.
//   - Tools run in their own process groups (primitives/process.go:609) with
//     SIGTERM -> 5s -> SIGKILL escalation; the process must wait for the
//     manager to drain or tool children are orphaned.
package runtime

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
	"uuid"

	acp "github.com/coder/acp-go-sdk"

	"github.com/unreallabsai/unreal-agent/harness/contextbuilder"
	"github.com/unreallabsai/unreal-agent/harness/coordinator"
	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"
	"github.com/unreallabsai/unreal-agent/harness/tool"
	"github.com/unreallabsai/unreal-agent/harness/tool/bash"
	"github.com/unreallabsai/unreal-agent/harness/tool/viewimage"

	"github.com/gastownhall/acp-unreal/internal/fifo"
	"github.com/gastownhall/acp-unreal/internal/gate"
	"github.com/gastownhall/acp-unreal/internal/mirror"
	"github.com/gastownhall/acp-unreal/internal/ops"
	"github.com/gastownhall/acp-unreal/internal/project"
	"github.com/gastownhall/acp-unreal/internal/tap"
)

// Client is the ACP client side the runtime talks to
// (*acp.AgentSideConnection satisfies it).
type Client interface {
	SessionUpdate(ctx context.Context, n acp.SessionNotification) error
	RequestPermission(ctx context.Context, r acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error)
}

// PermissionMode selects when Bash asks the client first.
type PermissionMode string

// Permission modes.
const (
	PermissionAuto      PermissionMode = "auto"
	PermissionAsk       PermissionMode = "ask"
	PermissionAllowlist PermissionMode = "allowlist"
)

// Permission option ids offered in session/request_permission.
const (
	OptionAllowOnce    = "allow_once"
	OptionAllowAlways  = "allow_always"
	OptionRejectOnce   = "reject_once"
	OptionRejectAlways = "reject_always"
)

// Config is the per-process runtime configuration.
type Config struct {
	Layout            Layout
	Shell             string
	SystemPrompt      string
	PermissionMode    PermissionMode
	Allow             []string
	PermissionTimeout time.Duration
	CancelGrace       time.Duration
	ContextWindow     int
	MaxUpdateText     int
	OpTimeout         time.Duration
	// NewAdapter builds the provider adapter with the delta tap installed.
	NewAdapter func(sink func(tap.Delta)) (llm.Adapter, error)
	Log        *slog.Logger
}

// cancelFlushWait bounds how long, after the SIGKILL at cancel-grace, the
// turn waits for real terminal statuses before synthesizing failed updates.
const cancelFlushWait = 500 * time.Millisecond

type eventKind int

const (
	evItem eventKind = iota + 1
	evDelta
	evOp
	evPermission
	evModelError
	evRunExit
	evWake
	evCancelGrace
	evCancelFlush
)

type event struct {
	kind    eventKind
	item    sessionstore.Item
	delta   tap.Delta
	op      operation.Operation
	heldCtx context.Context
	err     error
	gen     *generation
	turn    *turnState
}

type turnResult struct {
	stop  acp.StopReason
	err   error
	usage acp.Usage
	model string
}

type queuedInput struct{ id, text string }

// turnState is one ACP prompt turn (session/prompt -> PromptResponse); it
// spans any number of harness turns.
type turnState struct {
	inputID    string
	unobserved map[string]bool // submitted to the inbox, not yet persisted
	steers     []queuedInput
	cancelling bool
	flushed    bool
	waitCalls  map[string]bool
	stop       acp.StopReason
	err        error
	usage      acp.Usage
	model      string
	done       chan turnResult
	finished   bool
}

// generation is one coordinator.Run with its inbox.
type generation struct {
	inbox  *inbox.Inbox
	cancel context.CancelFunc
	done   chan struct{}
}

// Runtime hosts one harness session.
type Runtime struct {
	cfg      Config
	client   Client
	id       session.ID
	cwd      string
	lock     *Lock
	store    *localfile.Store
	registry tool.Registry
	ctx      context.Context
	cancel   context.CancelFunc
	log      *slog.Logger

	opsCancel context.CancelFunc
	ops       *ops.Manager
	gate      *gate.Gate
	sync      *mirror.Sync
	events    *fifo.Queue[event]
	closeOnce sync.Once

	mu             sync.Mutex
	turn           *turnState
	gen            *generation
	model          string
	effort         llm.ReasoningEffort
	settingsDirty  bool
	opCall         map[operation.ID]string
	calls          map[string]llm.ToolCall
	inputs         map[string]bool
	pgids          map[operation.ID]int
	cancelledCalls map[string]bool
	refuseNew      bool
	alwaysAllow    bool
	alwaysReject   bool

	// actor-only
	am              *mirror.Mirror
	streamedText    map[uint64]bool
	streamedThought map[uint64]bool
	textAttempt     map[uint64]int
	started         map[string]bool
	reported        map[string]bool
}

// Open attaches to an existing session log (the caller created it if new
// and holds lock, which Close releases). No coordinator starts, so opening
// never spends tokens.
func Open(parent context.Context, cfg Config, client Client, id session.ID, meta Meta, lock *Lock) (*Runtime, error) {
	store, err := localfile.New(cfg.Layout.SessionsDir())
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	r := &Runtime{
		cfg: cfg, client: client, id: id, cwd: meta.Cwd, lock: lock, store: store,
		model: meta.Model, effort: llm.ReasoningEffort(meta.Effort),
		sync: mirror.NewSync(), events: fifo.New[event](), log: cfg.Log.With("session", string(id)),
		opCall: map[operation.ID]string{}, calls: map[string]llm.ToolCall{}, inputs: map[string]bool{},
		pgids: map[operation.ID]int{}, cancelledCalls: map[string]bool{},
		am: mirror.New(), streamedText: map[uint64]bool{}, streamedThought: map[uint64]bool{},
		textAttempt: map[uint64]int{}, started: map[string]bool{}, reported: map[string]bool{},
	}
	r.ctx, r.cancel = context.WithCancel(parent)
	fail := func(err error) (*Runtime, error) {
		r.cancel()
		return nil, err
	}
	if err := r.seed(); err != nil {
		return fail(err)
	}
	operationsDir := cfg.Layout.OperationsDir(id)
	if err := os.MkdirAll(operationsDir, 0o700); err != nil {
		return fail(fmt.Errorf("create operation directory: %w", err))
	}
	r.registry = tool.NewRegistry(tool.StaticTranslators{
		Bash:      bash.New(bash.Config{Shell: cfg.Shell, Directory: r.cwd, BaseDirectory: operationsDir}),
		ViewImage: viewimage.New(viewimage.Config{Directory: r.cwd}),
	}, tool.BashName, tool.ViewImageName)
	// The operation manager is session-lifetime and NOT derived from r.ctx:
	// Close stops the coordinator first, then cancels and drains the manager.
	opsCtx, opsCancel := context.WithCancel(context.Background())
	r.opsCancel = opsCancel
	r.ops = ops.New(opsCtx, operation.NewLocalOperationManager(opsCtx), ops.Options{
		Policy: r.policy, Ask: r.enqueuePermission, Tap: r.tapOperation, OpTimeout: cfg.OpTimeout,
	})
	adapter, err := cfg.NewAdapter(r.onDelta)
	if err != nil {
		opsCancel()
		return fail(err)
	}
	r.gate = gate.New(adapter, gate.Options{Reasons: r.sync.TurnReasons, Model: r.currentModel, OnError: r.onModelError})
	// Anything already pending (an interrupted turn) is parked until the next
	// prompt adds a new input: restore re-wakes the model immediately at Run
	// start (loop.go:110-116) and must not re-answer a cancelled prompt.
	if len(r.sync.PendingReasons()) > 0 || len(r.sync.Outstanding()) > 0 {
		r.gate.Park(r.sync.Calls(), r.inputList())
	}
	store.AddObserver(r.observe) // before any Run; not safe concurrently with appends
	go r.actor()
	return r, nil
}

// ID is the session id.
func (r *Runtime) ID() session.ID { return r.id }

// Cwd is the session's working directory.
func (r *Runtime) Cwd() string { return r.cwd }

// ModelAndEffort returns the current settings.
func (r *Runtime) ModelAndEffort() (string, llm.ReasoningEffort) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.model, r.effort
}

// ProviderRequests counts provider requests made by this runtime.
func (r *Runtime) ProviderRequests() int { return r.gate.ProviderRequests() }

// seed folds existing history into both mirrors: the store observer does not
// fire during the coordinator's own restore (loop.go:486-535).
func (r *Runtime) seed() error {
	after := sessionstore.BeforeFirst
	for {
		page, err := r.store.Items(r.ctx, r.id, after, 256)
		if err != nil {
			return fmt.Errorf("read session %q: %w", r.id, err)
		}
		for _, item := range page.Items {
			r.sync.Apply(item)
			r.am.Apply(item)
			r.index(item)
		}
		if !page.More {
			return nil
		}
		after = page.NextAfter
	}
}

// index records lookups for the projector and the permission policy.
// Caller holds r.mu, or runs before any goroutine starts.
func (r *Runtime) index(item sessionstore.Item) {
	switch item.Kind {
	case sessionstore.ItemInput:
		if input := item.Data.(inbox.Input); input.Kind == inbox.InputExternal {
			r.inputs[string(input.ID)] = true
		}
	case sessionstore.ItemModelResponse:
		response := item.Data.(sessionstore.ModelResponse)
		for _, output := range response.Response.Output {
			if call, ok := output.Data.(llm.ToolCall); ok && output.Type == llm.ItemToolCall {
				r.calls[call.CallID] = call
				if r.refuseNew {
					// A response that raced a cancel: its calls must not start.
					r.cancelledCalls[call.CallID] = true
					r.gate.Park([]string{call.CallID}, nil)
				}
			}
		}
	case sessionstore.ItemToolCallStatus:
		status := item.Data.(sessionstore.ToolCallStatus)
		for _, value := range status.Operations {
			r.opCall[value.ID] = status.CallID
		}
	}
}

// observe runs synchronously on the coordinator goroutine after each append
// (sessionstore.go:42-44). It stays O(1) and never blocks.
func (r *Runtime) observe(id session.ID, item sessionstore.Item) {
	if id != r.id {
		return
	}
	r.sync.Apply(item)
	r.mu.Lock()
	r.index(item)
	r.mu.Unlock()
	r.events.Push(event{kind: evItem, item: item})
}

func (r *Runtime) inputList() []string {
	out := make([]string, 0, len(r.inputs))
	for id := range r.inputs {
		out = append(out, id)
	}
	return out
}

func (r *Runtime) currentModel() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.model
}

// policy runs inside ops.Manager.Add on the coordinator goroutine: it must
// decide synchronously and never block.
func (r *Runtime) policy(op operation.Operation) (ops.Verdict, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancelledCalls[r.opCall[op.ID]] {
		return ops.Deny, "canceled before start: cancelled by the user"
	}
	if op.Type != operation.TypeShell || r.cfg.PermissionMode == PermissionAuto || r.alwaysAllow {
		return ops.Allow, ""
	}
	if r.alwaysReject {
		return ops.Deny, "The user rejected all commands in this session. Do not run commands; ask the user how to proceed."
	}
	if r.cfg.PermissionMode == PermissionAllowlist {
		if state, err := operation.DecodeShellState(op); err == nil && allowlisted(state.Input.Command, r.cfg.Allow) {
			return ops.Allow, ""
		}
	}
	return ops.Ask, ""
}

// allowlisted matches a command against prefixes. Commands containing shell
// control characters never match: "echo x; rm -rf ~" must still ask.
func allowlisted(command string, prefixes []string) bool {
	command = strings.TrimSpace(command)
	if strings.ContainsAny(command, ";&|`$<>()\n\\") {
		return false
	}
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		if command == prefix || strings.HasPrefix(command, prefix+" ") {
			return true
		}
	}
	return false
}

func (r *Runtime) enqueuePermission(ctx context.Context, op operation.Operation) {
	r.events.Push(event{kind: evPermission, op: op, heldCtx: ctx})
}

func (r *Runtime) tapOperation(op operation.Operation) {
	if op.Type == operation.TypeShell {
		if state, err := operation.DecodeShellState(op); err == nil {
			r.mu.Lock()
			if state.ProcessGroupID > 1 && !mirror.OperationTerminal(op.Status) {
				r.pgids[op.ID] = state.ProcessGroupID
			} else {
				delete(r.pgids, op.ID)
			}
			r.mu.Unlock()
		}
	}
	r.events.Push(event{kind: evOp, op: op})
}

func (r *Runtime) onDelta(d tap.Delta) {
	r.gate.ObserveDelta(d)
	r.events.Push(event{kind: evDelta, delta: d})
}

func (r *Runtime) onModelError(_ uint64, err error) {
	r.events.Push(event{kind: evModelError, err: err})
}

func (r *Runtime) systemPrompt() string {
	prompt := r.cfg.SystemPrompt
	if prompt == "" {
		prompt = "You are a coding agent working on the user's project. Use the Bash tool to inspect and change files."
	}
	return prompt + "\n\nWorking directory: " + r.cwd
}

// ensureGeneration builds the coordinator lazily, at the first prompt after
// open or after Run ended.
func (r *Runtime) ensureGeneration() (*generation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gen != nil {
		return r.gen, nil
	}
	restored, err := r.store.Resume(r.ctx, r.id)
	if err != nil {
		return nil, fmt.Errorf("resume session %q: %w", r.id, err)
	}
	genCtx, cancel := context.WithCancel(r.ctx)
	in, err := inbox.New(genCtx, restored.ExternalInputIDs)
	if err != nil {
		cancel()
		return nil, err
	}
	builder := contextbuilder.NewBuilder(r.registry.Skills()...)
	builder.SetModel(llm.Model{ID: r.model, ReasoningEffort: r.effort})
	builder.SetSystemPrompt(r.systemPrompt())
	for _, definition := range r.registry.StaticDefinitions() {
		builder.AddTool(definition.Tool)
	}
	c := coordinator.New(coordinator.Dependencies{
		ToolHeartbeatInterval: 0, // an approval wait must never trigger a paid request
		SessionID:             r.id, Inbox: in, Restored: restored, Sessions: r.store,
		ContextBuilder: builder, LLM: r.gate, Tools: r.registry, Operations: r.ops,
	})
	gen := &generation{inbox: in, cancel: cancel, done: make(chan struct{})}
	r.gen = gen
	go func() {
		err := c.Run(genCtx)
		close(gen.done)
		r.events.Push(event{kind: evRunExit, gen: gen, err: err})
	}()
	if r.settingsDirty && r.effort != "" {
		r.settingsDirty = false
		if err := submitControl(r.ctx, in, inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: inbox.Settings{ReasoningEffort: r.effort}}); err != nil {
			r.log.Warn("submit settings", "err", err)
		}
	}
	return gen, nil
}

// submitExternal sends user text; the builder decodes external payloads as a
// JSON string (contextbuilder/builder.go:43-61).
func submitExternal(ctx context.Context, in *inbox.Inbox, id, text string) error {
	payload, err := json.Marshal(text)
	if err != nil {
		return err
	}
	return in.Submit(ctx, inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: payload})
}

func submitControl(ctx context.Context, in *inbox.Inbox, message inbox.ControlMessage) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return in.Submit(ctx, inbox.Input{ID: inbox.ID(uuid.New().String()), Kind: inbox.InputControl, Payload: payload})
}

// ValidInputID reports whether id can be used as an inbox input id.
func ValidInputID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Prompt runs one ACP prompt turn with already-flattened text.
func (r *Runtime) Prompt(ctx context.Context, text string, messageID *string) (acp.PromptResponse, error) {
	id := uuid.New().String()
	if messageID != nil && ValidInputID(*messageID) {
		id = *messageID
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		if r.turn == nil {
			break
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			return acp.PromptResponse{}, acp.NewInvalidRequest(map[string]any{"error": "a prompt turn is already running"})
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.inputs[id] {
		// The inbox deduplicates by input id (inbox/local.go:59-87): a
		// redelivered prompt was already recorded, so acknowledge it.
		r.mu.Unlock()
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn, UserMessageId: &id}, nil
	}
	t := &turnState{inputID: id, unobserved: map[string]bool{id: true}, waitCalls: map[string]bool{}, done: make(chan turnResult, 1)}
	r.turn = t
	r.refuseNew = false
	clear(r.cancelledCalls)
	r.mu.Unlock()

	gen, err := r.ensureGeneration()
	if err == nil {
		err = submitExternal(r.ctx, gen.inbox, id, text)
	}
	if err != nil {
		r.mu.Lock()
		r.finishLocked(t, turnResult{err: err})
		r.mu.Unlock()
	}
	select {
	case result := <-t.done:
		return result.response(id)
	case <-ctx.Done():
		// The SDK cancels this ctx on session/cancel and on a newer prompt
		// for the same session (acp-go-sdk agent_gen.go:405-418).
		r.CancelTurn()
		result := <-t.done
		return result.response(id)
	}
}

func (result turnResult) response(id string) (acp.PromptResponse, error) {
	if result.err != nil {
		return acp.PromptResponse{}, acp.NewInternalError(map[string]any{"error": result.err.Error()})
	}
	usage := result.usage
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	return acp.PromptResponse{
		StopReason: result.stop, Usage: &usage, UserMessageId: &id,
		Meta: map[string]any{"model": result.model},
	}, nil
}

func (r *Runtime) finishLocked(t *turnState, result turnResult) {
	if t.finished {
		return
	}
	t.finished = true
	if r.turn == t {
		r.turn = nil
	}
	t.done <- result
}

// CancelTurn stops the model and the turn's tool calls without ending
// coordinator.Run: Gate cancel, ops cancel, SIGKILL of recorded tool groups
// after cancel-grace, synthesized failed updates, then the cancelled answer.
// It is idempotent.
func (r *Runtime) CancelTurn() {
	r.mu.Lock()
	t := r.turn
	if t == nil || t.finished || t.cancelling {
		r.mu.Unlock()
		return
	}
	t.cancelling = true
	t.stop = acp.StopReasonCancelled
	r.refuseNew = true
	outstanding := r.sync.Outstanding()
	for id := range outstanding {
		t.waitCalls[id] = true
		r.cancelledCalls[id] = true
	}
	inputs := r.inputList()
	for id := range t.unobserved {
		inputs = append(inputs, id)
	}
	t.steers = nil
	r.mu.Unlock()

	r.gate.Park(r.sync.Calls(), inputs)
	r.gate.CancelInflight()
	for _, ids := range outstanding {
		for _, id := range ids {
			if err := r.ops.Cancel(id, "cancelled by the user"); err != nil {
				r.log.Warn("cancel operation", "op", id, "err", err)
			}
		}
	}
	r.events.Push(event{kind: evWake})
	time.AfterFunc(r.cfg.CancelGrace, func() { r.events.Push(event{kind: evCancelGrace, turn: t}) })
}

// killToolGroups SIGKILLs every recorded tool process group. Unreal's own
// escalation waits a fixed 5s (primitives/process.go:31), which is longer
// than gc's stop grace.
func (r *Runtime) killToolGroups(why string) {
	r.mu.Lock()
	groups := make([]int, 0, len(r.pgids))
	for _, pgid := range r.pgids {
		groups = append(groups, pgid)
	}
	r.mu.Unlock()
	for _, pgid := range groups {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
			r.log.Info("killed tool process group", "pgid", pgid, "why", why)
		}
	}
}

// Steer injects a user message into the running turn. Queue mode lands it
// at the next response boundary (streamed output is never discarded);
// interrupt mode submits now, so the coordinator supersedes the in-flight
// request natively (loop.go:350, 306-311).
func (r *Runtime) Steer(text string, interrupt bool) (string, error) {
	r.mu.Lock()
	t := r.turn
	if t == nil || t.finished || t.cancelling {
		r.mu.Unlock()
		return "", errors.New("no active prompt turn to steer")
	}
	id := uuid.New().String()
	t.unobserved[id] = true
	if !interrupt {
		t.steers = append(t.steers, queuedInput{id: id, text: text})
		r.mu.Unlock()
		r.events.Push(event{kind: evWake})
		return id, nil
	}
	gen := r.gen
	r.mu.Unlock()
	if gen == nil {
		return "", errors.New("no running generation")
	}
	return id, submitExternal(r.ctx, gen.inbox, id, text)
}

func (r *Runtime) actor() {
	for {
		ev, ok := r.events.Pop(r.ctx)
		if !ok {
			return
		}
		r.handle(ev)
		r.evaluate()
	}
}

func (r *Runtime) handle(ev event) {
	switch ev.kind {
	case evItem:
		r.am.Apply(ev.item)
		r.projectLive(ev.item)
		r.bookkeep(ev.item)
	case evDelta:
		r.projectDelta(ev.delta)
	case evOp:
		r.projectOperation(ev.op)
	case evPermission:
		go r.askPermission(ev.heldCtx, ev.op)
	case evModelError:
		r.log.Warn("model request failed", "err", ev.err)
		r.mu.Lock()
		t := r.turn
		if t != nil && !t.finished {
			t.err = ev.err
		}
		r.mu.Unlock()
		if t != nil && len(r.sync.Outstanding()) > 0 {
			r.CancelTurn()
		}
	case evRunExit:
		r.mu.Lock()
		if r.gen == ev.gen {
			r.gen = nil
		}
		t := r.turn
		if ev.err != nil && !errors.Is(ev.err, context.Canceled) && t != nil {
			r.finishLocked(t, turnResult{err: fmt.Errorf("coordinator stopped: %w", ev.err)})
		}
		r.mu.Unlock()
		if ev.err != nil && !errors.Is(ev.err, context.Canceled) {
			r.log.Error("coordinator exited", "err", ev.err)
		}
	case evCancelGrace:
		r.mu.Lock()
		live := r.turn == ev.turn && !ev.turn.finished
		tools := live && len(ev.turn.waitCalls) > 0
		r.mu.Unlock()
		if tools {
			r.killToolGroups("cancel grace elapsed")
		}
		if live {
			time.AfterFunc(cancelFlushWait, func() { r.events.Push(event{kind: evCancelFlush, turn: ev.turn}) })
		}
	case evCancelFlush:
		r.mu.Lock()
		var calls []string
		if r.turn == ev.turn {
			for id := range ev.turn.waitCalls {
				calls = append(calls, id)
			}
			clear(ev.turn.waitCalls)
			ev.turn.flushed = true
		}
		r.mu.Unlock()
		for _, id := range calls {
			if !r.reported[id] {
				r.reported[id] = true
				r.send(project.Failed(id, "Cancelled by the user."))
			}
		}
	}
}

func (r *Runtime) bookkeep(item sessionstore.Item) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.turn
	if t == nil {
		return
	}
	switch item.Kind {
	case sessionstore.ItemInput:
		delete(t.unobserved, string(item.Data.(inbox.Input).ID))
	case sessionstore.ItemModelResponse:
		response := item.Data.(sessionstore.ModelResponse).Response
		usage := response.Usage
		t.usage.InputTokens += int(usage.InputTokens)
		t.usage.OutputTokens += int(usage.OutputTokens)
		t.usage.ThoughtTokens = addInt(t.usage.ThoughtTokens, int(usage.ReasoningTokens))
		t.usage.CachedReadTokens = addInt(t.usage.CachedReadTokens, int(usage.CachedInputTokens))
		t.usage.CachedWriteTokens = addInt(t.usage.CachedWriteTokens, int(usage.CacheWriteInputTokens))
		if record, ok := r.gate.Lookup(response.ID); ok && record.Model != "" && record.Outcome != gate.Muted {
			t.model = record.Model
		}
		switch response.Stop {
		case llm.StopMaxOutputTokens:
			t.stop = acp.StopReasonMaxTokens
		case llm.StopRefused:
			t.stop = acp.StopReasonRefusal
		}
	case sessionstore.ItemToolCallStatus:
		status := item.Data.(sessionstore.ToolCallStatus)
		if project.Terminal(status) {
			delete(t.waitCalls, status.CallID)
		}
	}
}

func addInt(p *int, v int) *int {
	if p == nil {
		return &v
	}
	sum := *p + v
	return &sum
}

// evaluate applies the close rule after every event: the turn ends when the
// coordinator is quiescent and nothing we submitted is unobserved.
func (r *Runtime) evaluate() {
	r.mu.Lock()
	t := r.turn
	if t == nil || t.finished {
		r.mu.Unlock()
		return
	}
	if t.cancelling {
		if len(t.waitCalls) == 0 && (!r.am.Inflight || t.flushed) {
			r.finishLocked(t, turnResult{stop: acp.StopReasonCancelled, err: t.err, usage: t.usage, model: t.model})
		}
		r.mu.Unlock()
		return
	}
	if len(t.steers) > 0 && !r.am.Inflight && r.gen != nil {
		steers, gen := t.steers, r.gen
		t.steers = nil
		r.mu.Unlock()
		for _, steer := range steers {
			if err := submitExternal(r.ctx, gen.inbox, steer.id, steer.text); err != nil {
				r.log.Warn("submit steer", "err", err)
			}
		}
		return
	}
	if r.am.Quiescent() && len(t.unobserved) == 0 && len(t.steers) == 0 {
		stop := t.stop
		if stop == "" {
			stop = acp.StopReasonEndTurn
		}
		r.finishLocked(t, turnResult{stop: stop, err: t.err, usage: t.usage, model: t.model})
	}
	r.mu.Unlock()
}

func (r *Runtime) send(updates ...acp.SessionUpdate) {
	for _, update := range updates {
		if err := r.client.SessionUpdate(r.ctx, acp.SessionNotification{SessionId: acp.SessionId(r.id), Update: update}); err != nil {
			r.log.Warn("session/update", "err", err)
		}
	}
}

func (r *Runtime) projectDelta(d tap.Delta) {
	if d.Seq != r.gate.CurrentSeq() {
		return // a superseded request
	}
	limit := r.cfg.MaxUpdateText
	switch d.Kind {
	case tap.Text:
		id := fmt.Sprintf("m-%d", d.Seq)
		if previous, seen := r.textAttempt[d.Seq]; seen && previous != d.Attempt {
			r.send(project.AgentText("\n[provider retry]\n", id, limit)...)
		}
		r.textAttempt[d.Seq] = d.Attempt
		r.streamedText[d.Seq] = true
		r.send(project.AgentText(d.Text, id, limit)...)
	case tap.Thinking:
		r.streamedThought[d.Seq] = true
		r.send(project.AgentThought(d.Text, fmt.Sprintf("t-%d", d.Seq), limit)...)
	}
}

func (r *Runtime) projectLive(item sessionstore.Item) {
	limit := r.cfg.MaxUpdateText
	switch item.Kind {
	case sessionstore.ItemModelResponse:
		response := item.Data.(sessionstore.ModelResponse).Response
		record, _ := r.gate.Lookup(response.ID)
		for _, output := range response.Output {
			switch data := output.Data.(type) {
			case llm.Reasoning:
				if !r.streamedThought[record.Seq] && len(data.Summary) > 0 {
					r.send(project.AgentThought(strings.Join(data.Summary, "\n\n"), fmt.Sprintf("t-%d", record.Seq), limit)...)
				}
			case llm.Message:
				if data.Role == llm.RoleAssistant && data.Text != "" && !r.streamedText[record.Seq] {
					r.send(project.AgentText(data.Text, fmt.Sprintf("m-%d", record.Seq), limit)...)
				}
			case llm.ToolCall:
				r.send(project.ToolCallStart(data, limit))
			}
		}
		delete(r.streamedText, record.Seq)
		delete(r.streamedThought, record.Seq)
		delete(r.textAttempt, record.Seq)
		if used := response.Usage.InputTokens + response.Usage.OutputTokens; used > 0 {
			r.send(acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Used: int(used), Size: r.cfg.ContextWindow}})
		}
	case sessionstore.ItemToolCallStatus:
		status := item.Data.(sessionstore.ToolCallStatus)
		if !project.Terminal(status) || r.reported[status.CallID] {
			return
		}
		r.reported[status.CallID] = true
		r.mu.Lock()
		call := r.calls[status.CallID]
		r.mu.Unlock()
		r.send(project.ToolResult(r.registry, call, status, limit))
	}
}

func (r *Runtime) projectOperation(op operation.Operation) {
	if op.Status != operation.StatusAwaiting {
		return
	}
	r.mu.Lock()
	callID := r.opCall[op.ID]
	r.mu.Unlock()
	if callID == "" || r.started[callID] || r.reported[callID] {
		return
	}
	r.started[callID] = true
	r.send(project.InProgress(callID))
}

func (r *Runtime) askPermission(ctx context.Context, op operation.Operation) {
	r.mu.Lock()
	callID := r.opCall[op.ID]
	call := r.calls[callID]
	r.mu.Unlock()
	if r.cfg.PermissionTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.PermissionTimeout)
		defer cancel()
	}
	title, kind, args := project.DescribeCall(call, r.cfg.MaxUpdateText)
	status := acp.ToolCallStatusPending
	response, err := r.client.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: acp.SessionId(r.id),
		ToolCall:  acp.ToolCallUpdate{ToolCallId: acp.ToolCallId(callID), Title: &title, Kind: &kind, Status: &status, RawInput: args},
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: OptionAllowOnce},
			{Kind: acp.PermissionOptionKindAllowAlways, Name: "Allow all commands in this session", OptionId: OptionAllowAlways},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Reject", OptionId: OptionRejectOnce},
			{Kind: acp.PermissionOptionKindRejectAlways, Name: "Reject all commands in this session", OptionId: OptionRejectAlways},
		},
	})
	const rejected = "The user rejected this command. Do not run it again; ask the user how to proceed."
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		r.ops.Decide(op.ID, false, "The permission request timed out; the command was not run.")
	case err != nil:
		r.ops.Decide(op.ID, false, "permission request failed: "+err.Error())
	case response.Outcome.Cancelled != nil:
		r.ops.Decide(op.ID, false, "The user cancelled this command.")
	case response.Outcome.Selected != nil:
		switch response.Outcome.Selected.OptionId {
		case OptionAllowOnce:
			r.ops.Decide(op.ID, true, "")
		case OptionAllowAlways:
			r.mu.Lock()
			r.alwaysAllow = true
			r.mu.Unlock()
			r.ops.Decide(op.ID, true, "")
		case OptionRejectAlways:
			r.mu.Lock()
			r.alwaysReject = true
			r.mu.Unlock()
			r.ops.Decide(op.ID, false, rejected)
		default:
			r.ops.Decide(op.ID, false, rejected)
		}
	default:
		r.ops.Decide(op.ID, false, "invalid permission outcome")
	}
}

// Replay streams the recorded conversation for session/load. No coordinator
// is started, so loading never triggers a model call.
func (r *Runtime) Replay(ctx context.Context) error {
	calls := map[string]llm.ToolCall{}
	after := sessionstore.BeforeFirst
	for {
		page, err := r.store.Items(ctx, r.id, after, 256)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			r.send(project.Replay(item, r.registry, calls, r.cfg.MaxUpdateText)...)
		}
		if !page.More {
			return nil
		}
		after = page.NextAfter
	}
}

// SetEffort records a reasoning-effort change durably through the inbox
// (UpdateSettings is persisted and replayed).
func (r *Runtime) SetEffort(effort llm.ReasoningEffort) {
	r.mu.Lock()
	r.effort = effort
	gen := r.gen
	if gen == nil {
		r.settingsDirty = true
	}
	r.mu.Unlock()
	if gen != nil {
		if err := submitControl(r.ctx, gen.inbox, inbox.ControlMessage{Mode: inbox.UpdateSettings, Parameters: inbox.Settings{ReasoningEffort: effort}}); err != nil {
			r.log.Warn("submit settings", "err", err)
		}
	}
}

// SetModel switches the model for the next request; the Gate rewrites
// Request.Model.ID, so no coordinator rebuild is needed.
func (r *Runtime) SetModel(model string) {
	r.mu.Lock()
	r.model = model
	r.mu.Unlock()
}

// Close stops the session within budget: cancel the turn, StopHard the
// generation, SIGKILL recorded tool groups after cancel-grace, cancel and
// DRAIN the operation manager (every primitive finished, Updates closed),
// SIGKILL anything still recorded, then release the lock.
func (r *Runtime) Close(budget time.Duration) {
	r.closeOnce.Do(func() { r.close(budget) })
}

func (r *Runtime) close(budget time.Duration) {
	deadline := time.Now().Add(budget)
	remaining := func() time.Duration { return max(time.Until(deadline), 50*time.Millisecond) }
	r.CancelTurn()
	r.mu.Lock()
	gen := r.gen
	r.mu.Unlock()
	if gen != nil {
		if err := submitControl(r.ctx, gen.inbox, inbox.ControlMessage{Mode: inbox.StopHard, Reason: "session closing"}); err != nil {
			r.log.Warn("submit stop", "err", err)
		}
		select {
		case <-gen.done:
		case <-time.After(min(r.cfg.CancelGrace, remaining())):
		}
	}
	r.killToolGroups("session closing")
	if gen != nil {
		select {
		case <-gen.done:
		case <-time.After(remaining() / 2):
			r.log.Warn("coordinator did not stop before the close deadline")
		}
		gen.cancel()
	}
	r.opsCancel()
	select {
	case <-r.ops.Drained():
	case <-time.After(remaining()):
		r.log.Warn("operation manager did not drain before the close deadline")
	}
	r.killToolGroups("after drain")
	r.mu.Lock()
	if t := r.turn; t != nil {
		r.finishLocked(t, turnResult{stop: acp.StopReasonCancelled, usage: t.usage, model: t.model})
	}
	r.mu.Unlock()
	r.cancel()
	r.lock.Release()
}
