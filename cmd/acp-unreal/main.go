// Command acp-unreal is an ACP v1 agent over stdio that hosts Unreal Agent
// (github.com/unreallabsai/unreal-agent v0.1.1) sessions in one long-lived
// process. stdout carries only JSON-RPC; logs go to stderr.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/llm/responsesapi"
	"github.com/unreallabsai/unreal-agent/harness/primitives"

	"github.com/gastownhall/acp-unreal/internal/acpagent"
	"github.com/gastownhall/acp-unreal/internal/runtime"
	"github.com/gastownhall/acp-unreal/internal/sweep"
	"github.com/gastownhall/acp-unreal/internal/tap"
)

// version is the release; `go install ...@vX.Y.Z` builds report the module
// version instead (see buildVersion).
const version = "0.1.0"

// releaseVersion matches a tagged module version (not a VCS pseudo-version).
var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// buildVersion prefers the release version stamped by go install @vX.Y.Z.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && releaseVersion.MatchString(info.Main.Version) {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return version
}

// Exit codes.
const (
	exitUsage          = 2
	exitUnknownSession = 3
)

// sweepGrace is how long escaped tool descendants get between SIGTERM and
// SIGKILL at shutdown; it runs after the shutdown budget, inside gc's 5s.
const sweepGrace = 500 * time.Millisecond

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func defaultStateDir() string {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "acp-unreal")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "acp-unreal")
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

type options struct {
	baseURL, model, models, apiKeyEnv, apiKeyFile, scrubEnv, keepEnv, stateDir, permissionMode, allow, shell, systemPrompt string
	sessionID, resume                                                                                                      string
	permissionTimeout, cancelGrace, shutdownBudget, opTimeout                                                              time.Duration
	contextWindow, maxUpdateText, maxAttempts                                                                              int
	promptCacheKey, showVersion                                                                                            bool
}

func parseFlags() options {
	var o options
	flag.StringVar(&o.baseURL, "base-url", os.Getenv("ACP_UNREAL_BASE_URL"), "OpenAI Responses-compatible base URL (env ACP_UNREAL_BASE_URL)")
	flag.StringVar(&o.model, "model", os.Getenv("ACP_UNREAL_MODEL"), "default model id (env ACP_UNREAL_MODEL)")
	flag.StringVar(&o.models, "models", "", "comma-separated models offered as the model config option")
	flag.StringVar(&o.apiKeyEnv, "api-key-env", "ACP_UNREAL_API_KEY", "NAME of the env var holding the provider API key (read, then scrubbed)")
	flag.StringVar(&o.apiKeyFile, "api-key-file", "", "read the provider API key from this file (mode 0600); the key never enters any environment")
	flag.StringVar(&o.scrubEnv, "scrub-env", "", "comma-separated extra variable names removed from the environment tools inherit")
	flag.StringVar(&o.keepEnv, "keep-env", "", "comma-separated variable names kept even though their names look like credentials")
	flag.StringVar(&o.stateDir, "state-dir", defaultStateDir(), "session store, meta, locks and tool output; must be outside the workspace")
	flag.StringVar(&o.permissionMode, "permission-mode", "auto", "auto | ask | allowlist")
	flag.StringVar(&o.allow, "allow", "", "comma-separated command prefixes auto-allowed in allowlist mode")
	flag.DurationVar(&o.permissionTimeout, "permission-timeout", 0, "max wait for session/request_permission (0 = forever; expiry = reject_once)")
	flag.StringVar(&o.shell, "shell", "/bin/bash", "absolute shell for the Bash tool (never $SHELL)")
	flag.StringVar(&o.systemPrompt, "system-prompt", "", "system prompt (default: a short coding-agent preamble)")
	flag.DurationVar(&o.cancelGrace, "cancel-grace", time.Second, "wait after cancel before SIGKILLing tool process groups")
	flag.DurationVar(&o.shutdownBudget, "shutdown-budget", 4*time.Second, "graceful shutdown budget; keep below the owner's SIGKILL grace (gc: 5s)")
	flag.IntVar(&o.contextWindow, "context-window", 131072, "context window reported in usage_update")
	flag.IntVar(&o.maxUpdateText, "max-update-text", 16384, "max bytes of text per session/update chunk or tool result")
	flag.DurationVar(&o.opTimeout, "op-timeout", 0, "per-Bash wall clock (0 = unbounded)")
	flag.IntVar(&o.maxAttempts, "max-attempts", 3, "provider attempts per model request")
	flag.BoolVar(&o.promptCacheKey, "prompt-cache-key", false, "send the session id as prompt_cache_key")
	flag.StringVar(&o.sessionID, "session-id", "", "bound mode: create this session id on the first session/new (error if it exists)")
	flag.StringVar(&o.resume, "resume", "", "bound mode: open this existing session id on the first session/new, without replay")
	flag.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	flag.Parse()
	return o
}

func main() {
	os.Exit(run())
}

func run() int {
	// When the client dies, stdin reaches EOF and stdout AND stderr break at
	// the same moment. Go's default SIGPIPE disposition for fd 1/2 would kill
	// the process on its next log line, before shutdown drains and kills the
	// tools. Catching SIGPIPE makes those writes fail with EPIPE instead.
	// Notify (a caught handler) rather than signal.Ignore: SIG_IGN would be
	// inherited by every tool across exec.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	o := parseFlags()
	if o.showVersion {
		fmt.Println("acp-unreal", buildVersion())
		return 0
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	usage := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "acp-unreal: "+format+"\n", args...)
		return exitUsage
	}
	// Tools inherit this process's environment (operation/shell.go:519-532,
	// primitives/process.go:608) and can read /proc/$PPID/environ.
	// Prompts arrive only over ACP. gc appends a session's initial message
	// to the command when the provider's prompt_mode is "arg" (the pack sets
	// "none"); refusing it beats dropping the message silently.
	if flag.NArg() > 0 {
		return usage("unexpected positional arguments (%d); prompts are sent over ACP session/prompt. Under gc, set prompt_mode = \"none\" on the provider", flag.NArg())
	}
	apiKey, err := loadAPIKey(o)
	if err != nil {
		return usage("%v", err)
	}
	if o.baseURL == "" || o.model == "" {
		return usage("--base-url and --model (or ACP_UNREAL_BASE_URL / ACP_UNREAL_MODEL) are required")
	}
	mode := runtime.PermissionMode(o.permissionMode)
	switch mode {
	case runtime.PermissionAuto, runtime.PermissionAsk, runtime.PermissionAllowlist:
	default:
		return usage("--permission-mode must be auto, ask or allowlist")
	}
	if !filepath.IsAbs(o.shell) {
		return usage("--shell must be an absolute path")
	}
	stateDir, err := filepath.Abs(o.stateDir)
	if err != nil {
		return usage("--state-dir: %v", err)
	}
	if cwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(cwd, stateDir); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
			return usage("--state-dir %s is inside the working directory %s; keep agent state out of the workspace", stateDir, cwd)
		}
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		logger.Error("create state dir", "err", err)
		return 1
	}
	layout := runtime.Layout{Root: stateDir}
	bound, err := acpagent.ResolveBound(os.Getenv, o.sessionID, o.resume)
	if err != nil {
		return usage("%v", err)
	}
	// Mark every tool this process starts, so a nested acp-unreal does not
	// bind to this process's inherited gc session.
	if err := os.Setenv(acpagent.ParentPIDEnv, strconv.Itoa(os.Getpid())); err != nil {
		logger.Error("export "+acpagent.ParentPIDEnv, "err", err)
		return 1
	}
	if bound.Mode == acpagent.BoundResume && !layout.Exists(bound.ID) {
		fmt.Fprintf(os.Stderr, "acp-unreal: unknown session %s\n", bound.ID)
		return exitUnknownSession
	}
	if bound.Mode != acpagent.Unbound {
		logger.Info("bound mode", "session", string(bound.ID), "mode", int(bound.Mode))
	}

	baseURL, attempts, cacheKey := strings.TrimRight(o.baseURL, "/"), o.maxAttempts, o.promptCacheKey
	newAdapter := func(sink func(tap.Delta)) (llm.Adapter, error) {
		headers := map[string][]string{"Content-Type": {"application/json"}}
		if apiKey != "" {
			headers["Authorization"] = []string{"Bearer " + apiKey}
		}
		return responsesapi.NewAdapter(primitives.NewRemoteClientWithHTTPClient(tap.NewHTTPClient(sink)), responsesapi.Config{
			Endpoint:          baseURL + "/responses",
			Headers:           headers,
			MaxAttempts:       &attempts,
			CacheKeyPlacement: responsesapi.CacheKeyPlacement{UsePromptCacheKeyField: cacheKey},
		})
	}
	agent := acpagent.New(acpagent.Config{
		Runtime: runtime.Config{
			Layout: layout, Shell: o.shell, SystemPrompt: o.systemPrompt,
			PermissionMode: mode, Allow: splitCSV(o.allow), PermissionTimeout: o.permissionTimeout,
			CancelGrace: o.cancelGrace, ContextWindow: o.contextWindow, MaxUpdateText: o.maxUpdateText,
			OpTimeout: o.opTimeout, NewAdapter: newAdapter, Log: logger,
		},
		DefaultModel: o.model, Models: splitCSV(o.models), Bound: bound,
		ShutdownBudget: o.shutdownBudget, Version: buildVersion(),
	})
	// Descendants that escape their tool's process group (setsid daemons)
	// are found by an inherited environment marker and stopped at shutdown.
	marker, err := sweep.Mark()
	if err != nil {
		logger.Error("mark tool descendants", "err", err)
		return 1
	}
	shutdown := func() {
		agent.Shutdown()
		marker.Stop(logger, sweepGrace)
	}
	// The SDK logs via slog.Default() when no logger is set. Connection.
	// SetLogger is unsynchronized with the reader goroutine the constructor
	// starts (acp-go-sdk connection.go:125 vs :128), so set the default
	// instead of calling it.
	slog.SetDefault(logger)
	conn := acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin)
	agent.SetClient(conn)

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case sig := <-signals:
			if sig == syscall.SIGINT {
				// gc's ACP Interrupt sends SIGINT to the agent's process group:
				// cancel in place and stay alive (gc interrupt_now relies on it).
				logger.Info("SIGINT: cancelling active turns")
				agent.CancelAllTurns()
				continue
			}
			logger.Info("shutting down", "signal", sig.String())
			shutdown()
			return 0
		case <-conn.Done():
			logger.Info("client disconnected; shutting down")
			shutdown()
			return 0
		}
	}
}
