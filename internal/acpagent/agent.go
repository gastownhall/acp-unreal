// Package acpagent maps ACP v1 agent methods onto per-session runtimes.
package acpagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"uuid"

	acp "github.com/coder/acp-go-sdk"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"

	"github.com/gastownhall/acp-unreal/internal/runtime"
)

// SteerMethod is the extension method for mid-turn user input.
const SteerMethod = "_acp-unreal/steer"

// Harness identifies the pinned library.
const Harness = "unreal-agent@v0.1.1"

// Config configures the Agent.
type Config struct {
	Runtime        runtime.Config
	DefaultModel   string
	Models         []string
	Bound          Bound
	ShutdownBudget time.Duration
	Version        string
}

// Agent implements the SDK's Agent, AgentLoader and ExtensionMethodHandler.
type Agent struct {
	cfg    Config
	ctx    context.Context
	cancel context.CancelFunc
	client *clientRef
	log    *slog.Logger

	mu       sync.Mutex
	bound    Bound
	sessions map[session.ID]*runtime.Runtime
}

var (
	_ acp.Agent                  = (*Agent)(nil)
	_ acp.AgentLoader            = (*Agent)(nil)
	_ acp.ExtensionMethodHandler = (*Agent)(nil)
)

// New returns an Agent; call SetClient before serving.
func New(cfg Config) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	return &Agent{
		cfg: cfg, ctx: ctx, cancel: cancel, log: cfg.Runtime.Log, bound: cfg.Bound,
		client: &clientRef{ready: make(chan struct{})}, sessions: map[session.ID]*runtime.Runtime{},
	}
}

// SetClient wires the connection used for agent->client traffic. The SDK
// starts reading before its constructor returns, so handlers may run first;
// they block on the reference until it is set. Call it exactly once.
func (a *Agent) SetClient(client runtime.Client) {
	a.client.c = client
	close(a.client.ready)
}

// clientRef breaks the Agent <-> connection construction cycle; closing
// ready publishes c to every goroutine (happens-before).
type clientRef struct {
	ready chan struct{}
	c     runtime.Client
}

func (r *clientRef) SessionUpdate(ctx context.Context, n acp.SessionNotification) error {
	<-r.ready
	return r.c.SessionUpdate(ctx, n)
}

func (r *clientRef) RequestPermission(ctx context.Context, req acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	<-r.ready
	return r.c.RequestPermission(ctx, req)
}

func (a *Agent) layout() runtime.Layout { return a.cfg.Runtime.Layout }

// Initialize implements acp.Agent.
func (a *Agent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo:       &acp.Implementation{Name: "acp-unreal", Version: a.cfg.Version},
		AuthMethods:     []acp.AuthMethod{},
		AgentCapabilities: acp.AgentCapabilities{
			LoadSession:        true,
			PromptCapabilities: acp.PromptCapabilities{EmbeddedContext: true, Image: false, Audio: false},
			SessionCapabilities: acp.SessionCapabilities{
				Resume: &acp.SessionResumeCapabilities{},
				List:   &acp.SessionListCapabilities{},
				Close:  &acp.SessionCloseCapabilities{},
			},
			Meta: map[string]any{"acpUnreal": map[string]any{
				"harness":        Harness,
				"permissionMode": string(a.cfg.Runtime.PermissionMode),
				"steerMethod":    SteerMethod,
			}},
		},
	}, nil
}

// Authenticate implements acp.Agent; credentials come from the owner's env.
func (a *Agent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

// Logout implements acp.Agent.
func (a *Agent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}

// SetSessionMode implements acp.Agent.
func (a *Agent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

func invalid(format string, args ...any) error {
	return acp.NewInvalidParams(map[string]any{"error": fmt.Sprintf(format, args...)})
}

func internal(err error) error {
	var rpc *acp.RequestError
	if errors.As(err, &rpc) {
		return err
	}
	return acp.NewInternalError(map[string]any{"error": err.Error()})
}

// insideDir reports whether path is dir or below it.
func insideDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, "../")))
}

// NewSession implements acp.Agent. The first successful session/new of a
// bound process uses the bound id; others mint a uuid.
func (a *Agent) NewSession(_ context.Context, params acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	if !filepath.IsAbs(params.Cwd) {
		return acp.NewSessionResponse{}, invalid("cwd must be absolute")
	}
	if insideDir(a.layout().Root, filepath.Clean(params.Cwd)) {
		return acp.NewSessionResponse{}, invalid("state dir %s is inside the workspace %s; move --state-dir outside it", a.layout().Root, params.Cwd)
	}
	if len(params.McpServers) > 0 {
		a.log.Warn("MCP servers are not supported by the unreal harness; ignoring", "count", len(params.McpServers))
	}
	a.mu.Lock()
	bound := a.bound
	a.mu.Unlock()
	id := session.ID(uuid.New().String())
	if bound.Mode != Unbound {
		id = bound.ID
	}
	r, err := a.open(id, params.Cwd, bound.Mode)
	if err != nil {
		return acp.NewSessionResponse{}, err
	}
	a.mu.Lock()
	a.bound = Bound{}
	a.mu.Unlock()
	return acp.NewSessionResponse{SessionId: acp.SessionId(id), ConfigOptions: a.configOptions(r)}, nil
}

// open locks, creates or opens, and registers a runtime.
func (a *Agent) open(id session.ID, cwd string, mode BoundMode) (*runtime.Runtime, error) {
	a.mu.Lock()
	if r := a.sessions[id]; r != nil {
		a.mu.Unlock()
		if mode == BoundCreate {
			return nil, invalid("session %s already exists", id)
		}
		return r, nil
	}
	a.mu.Unlock()
	layout := a.layout()
	lock, err := layout.AcquireLock(id)
	if err != nil {
		return nil, internal(err)
	}
	fail := func(err error) (*runtime.Runtime, error) {
		lock.Release()
		return nil, err
	}
	exists := layout.Exists(id)
	switch {
	case mode == BoundCreate && exists:
		return fail(invalid("session %s already exists", id))
	case (mode == BoundResume || mode == loadExisting) && !exists:
		return fail(invalid("unknown session %s", id))
	}
	meta, metaErr := layout.ReadMeta(id)
	if !exists {
		store, err := localfile.New(layout.SessionsDir())
		if err != nil {
			return fail(internal(err))
		}
		// Create publishes via rename and would overwrite an existing log;
		// the Exists check above runs under the session lock.
		if _, err := store.Create(a.ctx, id); err != nil {
			return fail(internal(fmt.Errorf("create session %s: %w", id, err)))
		}
		meta = runtime.Meta{CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	} else if metaErr != nil {
		meta = runtime.Meta{CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	if cwd != "" {
		meta.Cwd = cwd
	}
	if meta.Cwd == "" {
		return fail(invalid("cwd is required for session %s", id))
	}
	if err := layout.WriteMeta(id, meta); err != nil {
		return fail(internal(err))
	}
	r, err := runtime.Open(a.ctx, a.cfg.Runtime, a.client, id, meta, meta.EffectiveModel(a.cfg.DefaultModel), lock)
	if err != nil {
		return fail(internal(err))
	}
	a.mu.Lock()
	a.sessions[id] = r
	a.mu.Unlock()
	return r, nil
}

// loadExisting is an internal mode for load/resume of a client-named id.
const loadExisting BoundMode = -1

func (a *Agent) attach(id acp.SessionId, cwd string) (*runtime.Runtime, error) {
	if !ValidSessionID(string(id)) {
		return nil, invalid("invalid session id %q", id)
	}
	if cwd != "" && !filepath.IsAbs(cwd) {
		return nil, invalid("cwd must be absolute")
	}
	return a.open(session.ID(id), cwd, loadExisting)
}

// LoadSession implements acp.AgentLoader: replay everything, then respond.
func (a *Agent) LoadSession(ctx context.Context, params acp.LoadSessionRequest) (acp.LoadSessionResponse, error) {
	r, err := a.attach(params.SessionId, params.Cwd)
	if err != nil {
		return acp.LoadSessionResponse{}, err
	}
	if err := r.Replay(ctx); err != nil {
		return acp.LoadSessionResponse{}, internal(err)
	}
	return acp.LoadSessionResponse{ConfigOptions: a.configOptions(r)}, nil
}

// ResumeSession implements acp.Agent: attach without replay.
func (a *Agent) ResumeSession(_ context.Context, params acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	r, err := a.attach(params.SessionId, params.Cwd)
	if err != nil {
		return acp.ResumeSessionResponse{}, err
	}
	return acp.ResumeSessionResponse{ConfigOptions: a.configOptions(r)}, nil
}

// ListSessions implements acp.Agent from the store plus meta sidecars.
func (a *Agent) ListSessions(ctx context.Context, params acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	store, err := localfile.New(a.layout().SessionsDir())
	if err != nil {
		return acp.ListSessionsResponse{}, internal(err)
	}
	infos, err := store.ListSessions(ctx)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return acp.ListSessionsResponse{}, internal(err)
	}
	sessions := []acp.SessionInfo{}
	for _, info := range infos {
		meta, err := a.layout().ReadMeta(info.ID)
		if err != nil || (params.Cwd != nil && *params.Cwd != meta.Cwd) {
			continue
		}
		updated := info.LastUpdatedAt.UTC().Format(time.RFC3339)
		sessions = append(sessions, acp.SessionInfo{
			SessionId: acp.SessionId(info.ID), Cwd: meta.Cwd, UpdatedAt: &updated,
			Meta: map[string]any{"model": meta.EffectiveModel(a.cfg.DefaultModel), "createdAt": meta.CreatedAt},
		})
	}
	return acp.ListSessionsResponse{Sessions: sessions}, nil
}

func (a *Agent) lookup(id acp.SessionId) (*runtime.Runtime, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.sessions[session.ID(id)]
	if r == nil {
		return nil, invalid("session %q is not loaded", id)
	}
	return r, nil
}

// Prompt implements acp.Agent.
func (a *Agent) Prompt(ctx context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	r, err := a.lookup(params.SessionId)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	text, err := FlattenPrompt(params.Prompt)
	if err != nil {
		return acp.PromptResponse{}, invalid("%s", err.Error())
	}
	return r.Prompt(ctx, text, params.MessageId)
}

// Cancel implements acp.Agent (session/cancel notification).
func (a *Agent) Cancel(_ context.Context, params acp.CancelNotification) error {
	if r, err := a.lookup(params.SessionId); err == nil {
		r.CancelTurn()
	}
	return nil
}

// SetSessionConfigOption implements acp.Agent: model and thought_level.
func (a *Agent) SetSessionConfigOption(_ context.Context, params acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	if params.ValueId == nil {
		return acp.SetSessionConfigOptionResponse{}, invalid("only select options are supported")
	}
	r, err := a.lookup(params.ValueId.SessionId)
	if err != nil {
		return acp.SetSessionConfigOptionResponse{}, err
	}
	value := string(params.ValueId.Value)
	meta, err := a.layout().ReadMeta(r.ID())
	if err != nil {
		meta = runtime.Meta{Cwd: r.Cwd(), CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	switch params.ValueId.ConfigId {
	case configModel:
		if value == "" {
			return acp.SetSessionConfigOptionResponse{}, invalid("model must not be empty")
		}
		if len(a.cfg.Models) > 0 && value != a.cfg.DefaultModel && !slices.Contains(a.cfg.Models, value) {
			return acp.SetSessionConfigOptionResponse{}, invalid("model %q is not offered (--models)", value)
		}
		r.SetModel(value)
		meta.ModelOverride = value
	case configThoughtLevel:
		effort := llm.ReasoningEffort(value)
		if value == thoughtDefault {
			effort = ""
		} else if !effort.Valid() {
			return acp.SetSessionConfigOptionResponse{}, invalid("invalid thought_level %q", value)
		}
		r.SetEffort(effort)
		meta.Effort = string(effort)
	default:
		return acp.SetSessionConfigOptionResponse{}, invalid("unknown config option %q", params.ValueId.ConfigId)
	}
	if err := a.layout().WriteMeta(r.ID(), meta); err != nil {
		return acp.SetSessionConfigOptionResponse{}, internal(err)
	}
	return acp.SetSessionConfigOptionResponse{ConfigOptions: a.configOptions(r)}, nil
}

// CloseSession implements acp.Agent.
func (a *Agent) CloseSession(_ context.Context, params acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	sid := session.ID(params.SessionId)
	a.mu.Lock()
	r := a.sessions[sid]
	delete(a.sessions, sid)
	a.mu.Unlock()
	if r != nil {
		r.Close(a.cfg.ShutdownBudget)
	}
	return acp.CloseSessionResponse{}, nil
}

type steerRequest struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
	Mode      string `json:"mode,omitempty"`
}

// HandleExtensionMethod implements acp.ExtensionMethodHandler.
func (a *Agent) HandleExtensionMethod(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method != SteerMethod {
		return nil, acp.NewMethodNotFound(method)
	}
	var request steerRequest
	if err := json.Unmarshal(params, &request); err != nil || strings.TrimSpace(request.Text) == "" {
		return nil, invalid("sessionId and text are required")
	}
	if request.Mode != "" && request.Mode != "queue" && request.Mode != "interrupt" {
		return nil, invalid("mode must be queue or interrupt")
	}
	r, err := a.lookup(acp.SessionId(request.SessionID))
	if err != nil {
		return nil, err
	}
	id, err := r.Steer(request.Text, request.Mode == "interrupt")
	if err != nil {
		return nil, acp.NewInvalidRequest(map[string]any{"error": err.Error()})
	}
	return map[string]any{"inputId": id}, nil
}

// Config option ids and the thought_level value meaning "send no effort".
const (
	configModel        = "model"
	configThoughtLevel = "thought_level"
	thoughtDefault     = "default"
)

func (a *Agent) configOptions(r *runtime.Runtime) []acp.SessionConfigOption {
	model, effort := r.ModelAndEffort()
	current := acp.SessionConfigValueId(effort)
	if effort == "" {
		current = thoughtDefault
	}
	models := acp.SessionConfigSelectOptionsUngrouped{}
	seen := map[string]bool{}
	for _, name := range append([]string{model}, a.cfg.Models...) {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		models = append(models, acp.SessionConfigSelectOption{Name: name, Value: acp.SessionConfigValueId(name)})
	}
	efforts := acp.SessionConfigSelectOptionsUngrouped{{Name: "Provider default", Value: thoughtDefault}}
	for _, level := range []llm.ReasoningEffort{llm.ReasoningEffortLow, llm.ReasoningEffortMedium, llm.ReasoningEffortHigh, llm.ReasoningEffortXHigh, llm.ReasoningEffortMax} {
		efforts = append(efforts, acp.SessionConfigSelectOption{Name: string(level), Value: acp.SessionConfigValueId(level)})
	}
	modelCategory, thoughtCategory := acp.SessionConfigOptionCategoryModel, acp.SessionConfigOptionCategoryThoughtLevel
	return []acp.SessionConfigOption{
		{Select: &acp.SessionConfigOptionSelect{Id: configModel, Name: "Model", Type: "select", Category: &modelCategory, CurrentValue: acp.SessionConfigValueId(model), Options: acp.SessionConfigSelectOptions{Ungrouped: &models}}},
		{Select: &acp.SessionConfigOptionSelect{Id: configThoughtLevel, Name: "Reasoning effort", Type: "select", Category: &thoughtCategory, CurrentValue: current, Options: acp.SessionConfigSelectOptions{Ungrouped: &efforts}}},
	}
}

func (a *Agent) runtimes() []*runtime.Runtime {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*runtime.Runtime, 0, len(a.sessions))
	for _, r := range a.sessions {
		out = append(out, r)
	}
	return out
}

// CancelAllTurns cancels every active turn in place (gc's Interrupt sends
// SIGINT to the agent's process group, internal/runtime/acp/acp.go:495-505).
func (a *Agent) CancelAllTurns() {
	for _, r := range a.runtimes() {
		r.CancelTurn()
	}
}

// Shutdown closes every session in parallel within the shutdown budget.
func (a *Agent) Shutdown() {
	var wg sync.WaitGroup
	for _, r := range a.runtimes() {
		wg.Go(func() { r.Close(a.cfg.ShutdownBudget) })
	}
	wg.Wait()
	a.cancel()
}
