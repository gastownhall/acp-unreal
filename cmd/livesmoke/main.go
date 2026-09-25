// Command livesmoke drives acp-unreal against a real Responses API provider
// with two prompts and prints COUNTS ONLY (no model text, no secrets).
//
//	livesmoke -agent <abs>/acp-unreal -state <dir> -cwd <dir> -- <agent flags...>
//
// The agent reads its key from the env var named by its --api-key-env flag,
// inherited from this process's environment; nothing here reads or prints it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

type client struct {
	mu                               sync.Mutex
	messages, thoughts, usageUpdates int
	toolCalls                        int
	statuses                         []string
	text, toolOutput                 strings.Builder
}

func (c *client) SessionUpdate(_ context.Context, n acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil:
		c.messages++
		c.text.WriteString(u.AgentMessageChunk.Content.Text.Text)
	case u.AgentThoughtChunk != nil:
		c.thoughts++
	case u.ToolCall != nil:
		c.toolCalls++
		c.statuses = append(c.statuses, "tool_call:"+string(u.ToolCall.Status))
	case u.ToolCallUpdate != nil && u.ToolCallUpdate.Status != nil:
		c.statuses = append(c.statuses, "update:"+string(*u.ToolCallUpdate.Status))
		for _, part := range u.ToolCallUpdate.Content {
			if part.Content != nil && part.Content.Content.Text != nil {
				c.toolOutput.WriteString(part.Content.Content.Text.Text)
			}
		}
	case u.UsageUpdate != nil:
		c.usageUpdates++
	}
	return nil
}

func (c *client) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messages, c.thoughts, c.usageUpdates, c.toolCalls = 0, 0, 0, 0
	c.statuses = nil
	c.text.Reset()
	c.toolOutput.Reset()
}

func (c *client) RequestPermission(context.Context, acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected("allow_once")}, nil
}
func (c *client) ReadTextFile(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound("fs/read_text_file")
}
func (c *client) WriteTextFile(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound("fs/write_text_file")
}
func (c *client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound("terminal/create")
}
func (c *client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound("terminal/kill")
}
func (c *client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound("terminal/output")
}
func (c *client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound("terminal/release")
}
func (c *client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound("terminal/wait_for_exit")
}

func main() {
	agentBin := flag.String("agent", "", "absolute path of acp-unreal")
	cwd := flag.String("cwd", "", "session working directory")
	stderrPath := flag.String("agent-stderr", "", "file for the agent's stderr")
	flag.Parse()
	cmd := exec.Command(*agentBin, flag.Args()...)
	cmd.Dir = *cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if *stderrPath != "" {
		f, err := os.Create(*stderrPath)
		if err != nil {
			fmt.Println("FAIL create stderr file:", err)
			os.Exit(1)
		}
		defer f.Close()
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		fmt.Println("FAIL start:", err)
		os.Exit(1)
	}
	fmt.Printf("agent pid=%d\n", cmd.Process.Pid)
	c := &client{}
	conn := acp.NewClientSideConnection(c, stdin, stdout)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	failures := 0
	defer func() {
		stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			fmt.Printf("agent exit err=%v\n", err)
		case <-time.After(8 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			fmt.Println("agent did not exit on stdin EOF; killed")
			failures++
		}
		fmt.Printf("RESULT failures=%d\n", failures)
	}()
	if _, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}); err != nil {
		fmt.Println("FAIL initialize:", err)
		failures++
		return
	}
	created, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: *cwd, McpServers: []acp.McpServer{}})
	if err != nil {
		fmt.Println("FAIL session/new:", err)
		failures++
		return
	}
	prompts := []struct{ text, want string }{
		{"Reply with exactly five words greeting me.", ""},
		{"Use the Bash tool to run `echo live-ok-$((6*7))` and then tell me the exact output.", "live-ok-42"},
	}
	for i, p := range prompts {
		c.reset()
		start := time.Now()
		resp, err := conn.Prompt(ctx, acp.PromptRequest{SessionId: created.SessionId, Prompt: []acp.ContentBlock{acp.TextBlock(p.text)}})
		c.mu.Lock()
		in, out := 0, 0
		if resp.Usage != nil {
			in, out = resp.Usage.InputTokens, resp.Usage.OutputTokens
		}
		model, _ := resp.Meta["model"].(string)
		sawTool := p.want != "" && strings.Contains(c.toolOutput.String(), p.want)
		sawText := p.want != "" && strings.Contains(c.text.String(), p.want)
		fmt.Printf("PROMPT %d: err=%v stop=%s elapsed=%s message_chunks=%d thought_chunks=%d usage_updates=%d tool_calls=%d statuses=%v usage_in=%d usage_out=%d served_model=%q tool_output_has_want=%v text_has_want=%v\n",
			i+1, err != nil, resp.StopReason, time.Since(start).Round(time.Millisecond), c.messages, c.thoughts, c.usageUpdates, c.toolCalls, c.statuses, in, out, model, sawTool, sawText)
		ok := err == nil && resp.StopReason == acp.StopReasonEndTurn && c.messages > 0 && in > 0
		if p.want != "" {
			ok = ok && sawTool
		}
		c.mu.Unlock()
		if !ok {
			failures++
		}
	}
}
