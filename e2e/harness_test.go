package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/gastownhall/acp-unreal/internal/testfake/responses"
)

// agentBin is built once by TestMain.
var agentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "acp-unreal-e2e-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	agentBin = filepath.Join(dir, "acp-unreal")
	args := []string{"build", "-o", agentBin}
	if os.Getenv("ACP_UNREAL_E2E_RACE") != "" {
		args = append(args, "-race") // race-instrument the agent binary itself
	}
	build := exec.Command("go", append(args, "github.com/gastownhall/acp-unreal/cmd/acp-unreal")...)
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build acp-unreal:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeLLM is the in-process scripted Responses API.
type fakeLLM struct {
	*responses.Fake
	srv *httptest.Server
}

func newFakeLLM(t *testing.T) *fakeLLM {
	t.Helper()
	fake := responses.New()
	mux := http.NewServeMux()
	mux.Handle("POST /v1/responses", fake)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &fakeLLM{Fake: fake, srv: srv}
}

func (f *fakeLLM) baseURL() string { return f.srv.URL + "/v1" }

// recorder is the test ACP client.
type recorder struct {
	mu         sync.Mutex
	updates    []acp.SessionUpdate
	permission func(acp.RequestPermissionRequest) acp.RequestPermissionResponse
	asked      []acp.RequestPermissionRequest
}

func (c *recorder) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	c.updates = append(c.updates, n.Update)
	c.mu.Unlock()
	return nil
}

func (c *recorder) RequestPermission(_ context.Context, r acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.asked = append(c.asked, r)
	decide := c.permission
	c.mu.Unlock()
	if decide == nil {
		return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("allow_once")}, nil
	}
	return decide(r), nil
}

func (c *recorder) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound("fs/read_text_file")
}

func (c *recorder) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound("fs/write_text_file")
}

func (c *recorder) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound("terminal/create")
}

func (c *recorder) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound("terminal/kill")
}

func (c *recorder) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound("terminal/output")
}

func (c *recorder) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound("terminal/release")
}

func (c *recorder) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound("terminal/wait_for_exit")
}

func (c *recorder) snapshot() []acp.SessionUpdate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]acp.SessionUpdate(nil), c.updates...)
}

func (c *recorder) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.updates)
}

func (c *recorder) since(mark int) []acp.SessionUpdate { return c.snapshot()[mark:] }

// lineTee records every complete stdout line on its way to the SDK.
type lineTee struct {
	r       io.Reader
	mu      sync.Mutex
	partial []byte
	lines   []string
}

func (l *lineTee) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if n > 0 {
		l.mu.Lock()
		l.partial = append(l.partial, p[:n]...)
		for {
			i := bytes.IndexByte(l.partial, '\n')
			if i < 0 {
				break
			}
			l.lines = append(l.lines, string(l.partial[:i]))
			l.partial = l.partial[i+1:]
		}
		l.mu.Unlock()
	}
	return n, err
}

func (l *lineTee) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// syncBuffer is a goroutine-safe stderr sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type agentOpts struct {
	stateDir string
	cwd      string
	args     []string
	env      []string
	marker   string
}

type agentProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	conn    *acp.ClientSideConnection
	client  *recorder
	stdin   io.WriteCloser
	tee     *lineTee
	stderr  *syncBuffer
	exited  chan struct{}
	exitErr error
	opts    agentOpts
}

var secretPattern = regexp.MustCompile(`(?i)bearer|api_key|authorization`)

// startAgent launches acp-unreal in its own process group (as gc does) with a
// minimal environment.
func startAgent(t *testing.T, llm *fakeLLM, o agentOpts) *agentProc {
	t.Helper()
	if o.marker == "" {
		o.marker = fmt.Sprintf("m%d", time.Now().UnixNano())
	}
	args := append([]string{"--base-url", llm.baseURL(), "--model", "fake-1", "--state-dir", o.stateDir, "--cancel-grace", "1s"}, o.args...)
	cmd := exec.Command(agentBin, args...)
	cmd.Dir = o.cwd
	cmd.Env = append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + filepath.Join(o.stateDir, "..", "home"), "LANG=C",
		"ACP_UNREAL_E2E_MARKER=" + o.marker,
	}, o.env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("started acp-unreal pid=%d", cmd.Process.Pid)
	tee := &lineTee{r: stdout}
	client := &recorder{}
	a := &agentProc{t: t, cmd: cmd, client: client, stdin: stdin, tee: tee, stderr: stderr, exited: make(chan struct{}), opts: o}
	a.conn = acp.NewClientSideConnection(client, stdin, tee)
	go func() {
		a.exitErr = cmd.Wait()
		close(a.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-a.exited:
		default:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-a.exited
		}
		// Hygiene: never leak tool processes from a failing test.
		time.Sleep(50 * time.Millisecond)
		for _, p := range markedProcs(o.marker, -1) {
			t.Errorf("leaked tool process %+v; killing its group", p)
			_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
		}
		a.checkStreams()
		if t.Failed() {
			t.Logf("agent stderr:\n%s", a.stderr.String())
		}
	})
	return a
}

// checkStreams asserts E10 (stdout is JSON-RPC only, lines < 1 MiB) and the
// log secret rule, for every agent in every test.
func (a *agentProc) checkStreams() {
	t := a.t
	for i, line := range a.tee.all() {
		if len(line) >= 1<<20 {
			t.Errorf("stdout line %d is %d bytes (>= 1 MiB)", i, len(line))
		}
		var msg struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.JSONRPC != "2.0" {
			t.Errorf("stdout line %d is not JSON-RPC: %.200q", i, line)
		}
	}
	if strings.Contains(a.stderr.String(), "DATA RACE") {
		t.Errorf("race detected in the agent:\n%s", a.stderr.String())
	}
	if m := secretPattern.FindString(a.stderr.String()); m != "" {
		t.Errorf("agent stderr contains forbidden pattern %q", m)
	}
}

func (a *agentProc) ctx() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	a.t.Cleanup(cancel)
	return ctx
}

func (a *agentProc) initialize() {
	a.t.Helper()
	resp, err := a.conn.Initialize(a.ctx(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		a.t.Fatalf("initialize: %v\nstderr:\n%s", err, a.stderr.String())
	}
	if resp.ProtocolVersion != 1 || !resp.AgentCapabilities.LoadSession {
		a.t.Fatalf("initialize response: %+v", resp)
	}
}

func (a *agentProc) newSession() acp.SessionId {
	a.t.Helper()
	resp, err := a.conn.NewSession(a.ctx(), acp.NewSessionRequest{Cwd: a.opts.cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		a.t.Fatalf("session/new: %v\nstderr:\n%s", err, a.stderr.String())
	}
	return resp.SessionId
}

func (a *agentProc) prompt(sid acp.SessionId, text string) acp.PromptResponse {
	a.t.Helper()
	resp, err := a.conn.Prompt(a.ctx(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock(text)}})
	if err != nil {
		a.t.Fatalf("session/prompt %q: %v\nstderr:\n%s", text, err, a.stderr.String())
	}
	return resp
}

// promptAsync runs a prompt in the background.
func (a *agentProc) promptAsync(sid acp.SessionId, text string) <-chan promptResult {
	ch := make(chan promptResult, 1)
	go func() {
		resp, err := a.conn.Prompt(context.Background(), acp.PromptRequest{SessionId: sid, Prompt: []acp.ContentBlock{acp.TextBlock(text)}})
		ch <- promptResult{resp: resp, err: err, at: time.Now()}
	}()
	return ch
}

type promptResult struct {
	resp acp.PromptResponse
	err  error
	at   time.Time
}

// stop closes stdin and waits for a clean exit.
func (a *agentProc) stop() {
	a.t.Helper()
	a.stdin.Close()
	a.waitExit(10 * time.Second)
}

func (a *agentProc) terminate() {
	a.t.Helper()
	_ = syscall.Kill(a.cmd.Process.Pid, syscall.SIGTERM)
	a.waitExit(10 * time.Second)
}

func (a *agentProc) waitExit(limit time.Duration) time.Duration {
	a.t.Helper()
	start := time.Now()
	select {
	case <-a.exited:
	case <-time.After(limit):
		a.t.Fatalf("agent did not exit within %s", limit)
	}
	return time.Since(start)
}

func (a *agentProc) alive() bool {
	select {
	case <-a.exited:
		return false
	default:
		return a.cmd.Process.Signal(syscall.Signal(0)) == nil
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- update helpers

func messageText(updates []acp.SessionUpdate) (string, map[string]bool, int) {
	var b strings.Builder
	ids := map[string]bool{}
	n := 0
	for _, u := range updates {
		if c := u.AgentMessageChunk; c != nil && c.Content.Text != nil {
			b.WriteString(c.Content.Text.Text)
			n++
			if c.MessageId != nil {
				ids[*c.MessageId] = true
			}
		}
	}
	return b.String(), ids, n
}

func thoughtCount(updates []acp.SessionUpdate) int {
	n := 0
	for _, u := range updates {
		if u.AgentThoughtChunk != nil {
			n++
		}
	}
	return n
}

// toolTrace lists "tool_call:<status>" / "tool_call_update:<status>:<text>".
func toolTrace(updates []acp.SessionUpdate) []string {
	var out []string
	for _, u := range updates {
		switch {
		case u.ToolCall != nil:
			out = append(out, fmt.Sprintf("tool_call:%s:%s", u.ToolCall.Status, u.ToolCall.Kind))
		case u.ToolCallUpdate != nil && u.ToolCallUpdate.Status != nil:
			text := ""
			for _, part := range u.ToolCallUpdate.Content {
				if part.Content != nil && part.Content.Content.Text != nil {
					text += part.Content.Content.Text.Text
				}
			}
			out = append(out, fmt.Sprintf("tool_call_update:%s:%s", *u.ToolCallUpdate.Status, text))
		}
	}
	return out
}

// --- /proc helpers (Linux)

type procInfo struct {
	pid, pgid int
	state     string
	comm      string
}

func readStat(pid int) (procInfo, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procInfo{}, false
	}
	s := string(data)
	open, closeIdx := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || closeIdx < 0 {
		return procInfo{}, false
	}
	fields := strings.Fields(s[closeIdx+2:])
	if len(fields) < 3 {
		return procInfo{}, false
	}
	pgid, _ := strconv.Atoi(fields[2])
	return procInfo{pid: pid, pgid: pgid, state: fields[0], comm: s[open+1 : closeIdx]}, true
}

// markedProcs lists live (non-zombie) processes carrying the marker env var,
// excluding exclude (the agent itself).
func markedProcs(marker string, exclude int) []procInfo {
	entries, _ := os.ReadDir("/proc")
	want := []byte("ACP_UNREAL_E2E_MARKER=" + marker + "\x00")
	var out []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == exclude {
			continue
		}
		env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil || !bytes.Contains(env, want) {
			continue
		}
		if info, ok := readStat(pid); ok && info.state != "Z" {
			out = append(out, info)
		}
	}
	return out
}

// liveInGroup lists live (non-zombie) members of process group pgid.
func liveInGroup(pgid int) []procInfo {
	entries, _ := os.ReadDir("/proc")
	var out []procInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if info, ok := readStat(pid); ok && info.pgid == pgid && info.state != "Z" {
			out = append(out, info)
		}
	}
	return out
}

// newDirs returns a fresh state dir and workspace, siblings under one temp dir.
func newDirs(t *testing.T) (state, workspace string) {
	root := t.TempDir()
	state, workspace = filepath.Join(root, "state"), filepath.Join(root, "ws")
	for _, d := range []string{state, workspace, filepath.Join(root, "home")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return state, workspace
}

func lineIndex(lines []string, from int, pred func(string) bool) int {
	for i := from; i < len(lines); i++ {
		if pred(lines[i]) {
			return i
		}
	}
	return -1
}

func contains(subs ...string) func(string) bool {
	return func(s string) bool {
		for _, sub := range subs {
			if !strings.Contains(s, sub) {
				return false
			}
		}
		return true
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
