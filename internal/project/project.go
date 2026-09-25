// Package project maps persisted Unreal session items onto ACP session
// updates. Everything here is pure; the runtime decides when to send.
package project

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"

	"github.com/unreallabsai/unreal-agent/harness/inbox"
	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/operation"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

// Resolver finds a tool's result translator (tool.Registry satisfies it).
type Resolver interface {
	Resolve(name string) (tool.Translator, bool)
}

// BoundText keeps a head and tail of text within limit bytes (UTF-8 safe).
// gc's ACP reader drops lines over 1 MiB (internal/runtime/acp/conn.go:85).
func BoundText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	half := limit / 2
	head := text[:half]
	for !utf8.ValidString(head) && len(head) > 0 {
		head = head[:len(head)-1]
	}
	tail := text[len(text)-half:]
	for !utf8.ValidString(tail) && len(tail) > 0 {
		tail = tail[1:]
	}
	return head + fmt.Sprintf("\n...[%d bytes omitted]...\n", len(text)-len(head)-len(tail)) + tail
}

// SplitText cuts text into pieces of at most limit bytes on rune boundaries,
// so one long message becomes several chunks sharing a messageId.
func SplitText(text string, limit int) []string {
	if limit <= 0 || len(text) <= limit {
		return []string{text}
	}
	var parts []string
	for len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		if cut == 0 {
			cut = limit
		}
		parts = append(parts, text[:cut])
		text = text[cut:]
	}
	return append(parts, text)
}

// AgentText returns agent_message_chunk updates for text, split to limit.
func AgentText(text, messageID string, limit int) []acp.SessionUpdate {
	var out []acp.SessionUpdate
	for _, part := range SplitText(text, limit) {
		id := messageID
		out = append(out, acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(part), MessageId: &id}})
	}
	return out
}

// AgentThought returns agent_thought_chunk updates for text, split to limit.
func AgentThought(text, messageID string, limit int) []acp.SessionUpdate {
	var out []acp.SessionUpdate
	for _, part := range SplitText(text, limit) {
		id := messageID
		out = append(out, acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock(part), MessageId: &id}})
	}
	return out
}

// UserText returns user_message_chunk updates for text, split to limit.
func UserText(text, messageID string, limit int) []acp.SessionUpdate {
	var out []acp.SessionUpdate
	for _, part := range SplitText(text, limit) {
		id := messageID
		out = append(out, acp.SessionUpdate{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: acp.TextBlock(part), MessageId: &id}})
	}
	return out
}

// DescribeCall gives a call's ACP title, kind and raw input.
func DescribeCall(call llm.ToolCall, limit int) (string, acp.ToolKind, map[string]any) {
	var args map[string]any
	_ = json.Unmarshal([]byte(call.Arguments), &args)
	text := func(key string) string {
		value, _ := args[key].(string)
		return value
	}
	args = boundArgs(args, limit)
	switch call.Name {
	case tool.BashName:
		command := strings.TrimSpace(text("command"))
		if line, _, cut := strings.Cut(command, "\n"); cut {
			command = line + " ..."
		}
		if len(command) > 120 {
			command = BoundText(command, 117)
		}
		return command, acp.ToolKindExecute, args
	case tool.ViewImageName:
		return "View image " + text("path"), acp.ToolKindRead, args
	case tool.SkillUseName:
		return "Load skill " + text("name"), acp.ToolKindRead, args
	default:
		return call.Name, acp.ToolKindOther, args
	}
}

// boundArgs keeps rawInput within about limit bytes of JSON however the
// size is distributed: top-level strings are bounded individually, and if
// the whole object is still too large (deep nesting, huge arrays) it is
// replaced by a bounded JSON preview plus the command.
func boundArgs(args map[string]any, limit int) map[string]any {
	if limit <= 0 || args == nil {
		return args
	}
	for key, value := range args {
		if s, ok := value.(string); ok {
			args[key] = BoundText(s, limit)
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil || len(encoded) <= limit {
		return args
	}
	bounded := map[string]any{"truncated": true, "json": BoundText(string(encoded), limit/2)}
	if command, ok := args["command"].(string); ok {
		bounded["command"] = BoundText(command, limit/2)
	}
	return bounded
}

// ToolCallStart is the pending tool_call announcing call.
func ToolCallStart(call llm.ToolCall, limit int) acp.SessionUpdate {
	title, kind, args := DescribeCall(call, limit)
	return acp.StartToolCall(acp.ToolCallId(call.CallID), title, acp.WithStartKind(kind),
		acp.WithStartStatus(acp.ToolCallStatusPending), acp.WithStartRawInput(args))
}

// InProgress is the tool_call_update marking a call as running.
func InProgress(callID string) acp.SessionUpdate {
	status := acp.ToolCallStatusInProgress
	return acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: acp.ToolCallId(callID), Status: &status}}
}

// Failed is a synthesized terminal tool_call_update.
func Failed(callID, text string) acp.SessionUpdate {
	status := acp.ToolCallStatusFailed
	return acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: acp.ToolCallId(callID), Status: &status,
		Content: []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(text))},
	}}
}

// Terminal reports whether status is final (an error or all ops ended).
func Terminal(status sessionstore.ToolCallStatus) bool {
	if status.Status.Error != "" {
		return true
	}
	byID := map[operation.ID]operation.Status{}
	for _, value := range status.Operations {
		byID[value.ID] = value.Status
	}
	for _, id := range status.Status.WaitingFor {
		switch byID[id] {
		case operation.StatusCompleted, operation.StatusFailed, operation.StatusCanceled:
		default:
			return false
		}
	}
	return true
}

// ToolResult renders a terminal call status with the tool's own pure result
// translator -- the same text the model sees -- capped at limit.
func ToolResult(tools Resolver, call llm.ToolCall, status sessionstore.ToolCallStatus, limit int) acp.SessionUpdate {
	failed := status.Status.Error != ""
	for _, value := range status.Operations {
		if value.Status == operation.StatusFailed || value.Status == operation.StatusCanceled {
			failed = true
		}
	}
	raw := map[string]any{}
	text := status.Status.Error
	if translator, ok := tools.Resolve(call.Name); ok && call.Name != "" {
		if result, err := translator.TranslateResult(status.CallID, status.Status, status.Operations); err == nil {
			var parts []string
			for _, output := range result.Output {
				if output.Kind == llm.ToolResultText {
					parts = append(parts, output.Value)
				} else {
					parts = append(parts, "[image]")
				}
			}
			text = strings.Join(parts, "\n")
		}
	}
	for _, value := range status.Operations {
		if value.Type != operation.TypeShell {
			continue
		}
		if state, err := operation.DecodeShellState(value); err == nil {
			raw["status"] = string(value.Status)
			if state.Result != nil {
				raw["exitCode"] = state.Result.ExitCode
				raw["stdoutBytes"] = state.Result.OutSize
				raw["stderrBytes"] = state.Result.ErrSize
			}
		}
	}
	state := acp.ToolCallStatusCompleted
	if failed {
		state = acp.ToolCallStatusFailed
	}
	return acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{
		ToolCallId: acp.ToolCallId(status.CallID), Status: &state,
		Content:   []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(BoundText(text, limit)))},
		RawOutput: raw,
	}}
}

// Replay renders one persisted item for session/load. calls indexes every
// tool call seen so far (Replay adds to it).
func Replay(sessionID string, item sessionstore.Item, tools Resolver, calls map[string]llm.ToolCall, limit int) []acp.SessionUpdate {
	switch item.Kind {
	case sessionstore.ItemInput:
		input, _ := item.Data.(inbox.Input)
		if input.Kind != inbox.InputExternal {
			return nil
		}
		var text string
		if err := json.Unmarshal(input.Payload, &text); err != nil {
			return nil
		}
		return UserText(text, string(input.ID), limit)
	case sessionstore.ItemModelResponse:
		response := item.Data.(sessionstore.ModelResponse).Response
		var out []acp.SessionUpdate
		for _, output := range response.Output {
			switch data := output.Data.(type) {
			case llm.Reasoning:
				if len(data.Summary) > 0 {
					out = append(out, AgentThought(strings.Join(data.Summary, "\n\n"), MessageID(sessionID, response.ID, KindThought), limit)...)
				}
			case llm.Message:
				if data.Role == llm.RoleAssistant && data.Text != "" {
					out = append(out, AgentText(data.Text, MessageID(sessionID, response.ID, KindMessage), limit)...)
				}
			case llm.ToolCall:
				calls[data.CallID] = data
				out = append(out, ToolCallStart(data, limit))
			}
		}
		return out
	case sessionstore.ItemToolCallStatus:
		status := item.Data.(sessionstore.ToolCallStatus)
		if !Terminal(status) {
			return nil
		}
		return []acp.SessionUpdate{ToolResult(tools, calls[status.CallID], status, limit)}
	}
	return nil
}
