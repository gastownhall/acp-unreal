package runtime

// Contract tests pinned to github.com/unreallabsai/unreal-agent v0.1.1
// (b7c9bf1). Each pins one library behaviour acp-unreal's design relies on;
// if an upgrade breaks one, revisit the component named in the comment.

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
)

type scriptedLLM struct {
	mu      sync.Mutex
	calls   int
	ctxs    []context.Context
	started chan int
	respond func(ctx context.Context, n int) (llm.Response, error)
}

func (s *scriptedLLM) Respond(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.ctxs = append(s.ctxs, ctx)
	s.mu.Unlock()
	if s.started != nil {
		s.started <- n
	}
	return s.respond(ctx, n)
}

func (s *scriptedLLM) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type harness struct {
	store *localfile.Store
	id    session.ID
	in    *inbox.Inbox
	done  chan error
}

func startCoordinator(t *testing.T, adapter llm.Adapter) *harness {
	t.Helper()
	dir := t.TempDir()
	store, err := localfile.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID("contract-1")
	if _, err := store.Create(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Resume(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	in, err := inbox.New(ctx, restored.ExternalInputIDs)
	if err != nil {
		t.Fatal(err)
	}
	registry := tool.NewRegistry(tool.StaticTranslators{Bash: bash.New(bash.Config{Shell: "/bin/sh", Directory: dir, BaseDirectory: dir})}, tool.BashName)
	builder := contextbuilder.NewBuilder()
	builder.SetModel(llm.Model{ID: "m"})
	opsCtx, opsCancel := context.WithCancel(context.Background())
	t.Cleanup(opsCancel)
	c := coordinator.New(coordinator.Dependencies{
		SessionID: id, Inbox: in, Restored: restored, Sessions: store, ContextBuilder: builder,
		LLM: adapter, Tools: registry, Operations: operation.NewLocalOperationManager(opsCtx),
	})
	h := &harness{store: store, id: id, in: in, done: make(chan error, 1)}
	go func() { h.done <- c.Run(ctx) }()
	return h
}

func (h *harness) submit(t *testing.T, id, text string) {
	t.Helper()
	payload, _ := json.Marshal(text)
	if err := h.in.Submit(context.Background(), inbox.Input{ID: inbox.ID(id), Kind: inbox.InputExternal, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) control(t *testing.T, mode inbox.ControlMode) {
	t.Helper()
	payload, _ := json.Marshal(inbox.ControlMessage{Mode: mode})
	if err := h.in.Submit(context.Background(), inbox.Input{ID: inbox.ID("ctl-" + string(mode)), Kind: inbox.InputControl, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Gate relies on: an empty COMPLETED Response is persisted and leaves the
// coordinator idle (Run keeps running, no further model call).
func TestContractEmptyCompletedResponseLeavesCoordinatorIdle(t *testing.T) {
	adapter := &scriptedLLM{respond: func(context.Context, int) (llm.Response, error) {
		return llm.Response{ID: "empty", Stop: llm.StopComplete}, nil
	}}
	h := startCoordinator(t, adapter)
	h.submit(t, "in-1", "hello")
	eventually(t, "first model call", func() bool { return adapter.count() == 1 })
	select {
	case err := <-h.done:
		t.Fatalf("Run returned after an empty response: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if adapter.count() != 1 {
		t.Fatalf("model called %d times", adapter.count())
	}
	h.control(t, inbox.StopWhenIdle)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopWhenIdle did not end Run: coordinator was not idle")
	}
}

// Gate relies on: a model error with a live ctx is FATAL to Run
// (loop.go:225-232) -- the reason the Gate converts errors.
func TestContractModelErrorIsFatalToRun(t *testing.T) {
	adapter := &scriptedLLM{respond: func(context.Context, int) (llm.Response, error) {
		return llm.Response{}, errors.New("provider 503")
	}}
	h := startCoordinator(t, adapter)
	h.submit(t, "in-1", "hello")
	select {
	case err := <-h.done:
		if err == nil || !strings.Contains(err.Error(), "provider 503") {
			t.Fatalf("Run err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run survived a model error; the Gate's conversion may be unnecessary now")
	}
}

// Mirror/Gate rely on: a second input while a request is in flight cancels
// the superseded request's ctx (native steer, loop.go:350, 306-311).
func TestContractSupersededRequestCtxIsCancelled(t *testing.T) {
	adapter := &scriptedLLM{started: make(chan int, 4), respond: func(ctx context.Context, n int) (llm.Response, error) {
		if n == 1 {
			<-ctx.Done()
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{ID: "r2", Stop: llm.StopComplete}, nil
	}}
	h := startCoordinator(t, adapter)
	h.submit(t, "in-1", "first")
	<-adapter.started
	h.submit(t, "in-2", "second")
	<-adapter.started
	adapter.mu.Lock()
	first := adapter.ctxs[0]
	adapter.mu.Unlock()
	eventually(t, "superseded ctx cancelled", func() bool { return first.Err() != nil })
	select {
	case err := <-h.done:
		t.Fatalf("Run ended: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// The runtime's observer relies on: observers run synchronously (before the
// Append returns) and in append order.
func TestContractObserversAreSynchronousAndOrdered(t *testing.T) {
	store, err := localfile.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := session.ID("obs-1")
	if _, err := store.Create(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var seen []string
	store.AddObserver(func(_ session.ID, item sessionstore.Item) {
		if input, ok := item.Data.(inbox.Input); ok {
			seen = append(seen, string(input.ID))
		}
	})
	for i := range 5 {
		payload, _ := json.Marshal("x")
		inputID := "in-" + strconv.Itoa(i)
		if err := store.AppendInput(context.Background(), id, inbox.Input{ID: inbox.ID(inputID), Kind: inbox.InputExternal, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		if len(seen) != i+1 || seen[i] != inputID {
			t.Fatalf("after append %d observer saw %v", i, seen)
		}
	}
}

func shellOperation(t *testing.T, id, command string) operation.Operation {
	t.Helper()
	spec, err := operation.NewShellSpec(operation.ShellInput{Command: command, Shell: "/bin/sh", Directory: t.TempDir()}, t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	return operation.Operation{ID: operation.ID(id), Type: spec.Type, Version: spec.Version, Status: operation.StatusReady, State: spec.State, MaxOutputLength: spec.MaxOutputLength}
}

// ops.Manager relies on: LocalOperationManager silently drops a duplicate
// Add of a known id (local_manager.go:133-137) -- hence the re-emit.
func TestContractDuplicateAddIsSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := operation.NewLocalOperationManager(ctx)
	op := shellOperation(t, "dup-1", "true")
	if err := manager.Add(op); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for done := false; !done; {
		select {
		case update := <-manager.Updates():
			done = update.Status == operation.StatusCompleted
		case <-deadline:
			t.Fatal("op did not complete")
		}
	}
	if err := manager.Add(op); err != nil {
		t.Fatalf("duplicate Add: %v", err)
	}
	select {
	case update := <-manager.Updates():
		t.Fatalf("duplicate Add produced an update: %s", update.Status)
	case <-time.After(300 * time.Millisecond):
	}
}

// Close relies on: cancelling the manager ctx and waiting for Updates() to
// close kills even a SIGTERM-ignoring child (SIGTERM -> 5s -> SIGKILL,
// primitives/process.go:31, 764-833). It also pins the ~5s escalation that
// makes acp-unreal's own earlier pgid SIGKILL necessary.
func TestContractManagerDrainKillsTermIgnoringChild(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := operation.NewLocalOperationManager(ctx)
	if err := manager.Add(shellOperation(t, "term-1", `trap "" TERM; sleep 60 & wait`)); err != nil {
		t.Fatal(err)
	}
	pgid := 0
	deadline := time.After(5 * time.Second)
	for pgid == 0 {
		select {
		case update := <-manager.Updates():
			if state, err := operation.DecodeShellState(update); err == nil && state.ProcessGroupID > 1 {
				pgid = state.ProcessGroupID
			}
		case <-deadline:
			t.Fatal("no update carried ProcessGroupID")
		}
	}
	eventually(t, "sleep child in the tool group", func() bool { return groupAlive(pgid, "sleep") })
	start := time.Now()
	cancel()
	for range manager.Updates() {
	}
	took := time.Since(start)
	t.Logf("manager drained %s after cancel", took.Round(time.Millisecond))
	if groupAlive(pgid, "") {
		t.Fatalf("tool group %d survived the drain", pgid)
	}
	if took < 4*time.Second {
		t.Fatalf("drain took %s; the 5s SIGTERM grace changed -- revisit --cancel-grace/--shutdown-budget", took)
	}
}

// groupAlive reports a live (non-zombie) member of pgid, optionally by comm.
func groupAlive(pgid int, comm string) bool {
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		s := string(data)
		closeIdx := strings.LastIndexByte(s, ')')
		fields := strings.Fields(s[closeIdx+2:])
		if len(fields) < 3 || fields[0] == "Z" {
			continue
		}
		if g, _ := strconv.Atoi(fields[2]); g != pgid {
			continue
		}
		if comm == "" || s[strings.IndexByte(s, '(')+1:closeIdx] == comm {
			return true
		}
	}
	return false
}

// AcquireLock relies on: localfile.Store.Create on an existing id silently
// REPLACES the session log via rename (localfile/store.go:71-84, 386-408), so
// creation must be serialized by a lock that is not on the log's inode, and
// callers must check existence first.
func TestContractCreateOverwritesExistingSession(t *testing.T) {
	store, err := localfile.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := session.ID("over-1")
	if _, err := store.Create(ctx, id); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal("x")
	if err := store.AppendInput(ctx, id, inbox.Input{ID: "in-1", Kind: inbox.InputExternal, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, id); err != nil {
		t.Fatalf("second Create errored (library now guards; simplify open()): %v", err)
	}
	page, err := store.Items(ctx, id, sessionstore.BeforeFirst, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("second Create kept %d items; overwrite behaviour changed", len(page.Items))
	}
}
