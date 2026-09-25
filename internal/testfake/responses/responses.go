// Package responses is a scripted OpenAI Responses API SSE fake for
// deterministic tests. The reply depends on the last input item:
//
//   - a user message containing RUN[<cmd>]: one Bash function_call
//   - a user message containing SLOW: a slowly streamed text reply
//   - a function_call_output: "Tool result: <first 60 chars>"
//   - anything else: "Echo: <text>"
//
// Text replies stream a reasoning delta and one output_text delta per word.
// The completed response reports model "<model>-served".
package responses

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Request is what the fake recorded about one model request.
type Request struct {
	N         int      `json:"n"`
	Model     string   `json:"model"`
	Effort    string   `json:"effort"`
	Items     int      `json:"items"`
	LastKind  string   `json:"last_kind"`
	LastText  string   `json:"last_text"`
	UserTexts []string `json:"user_texts"`
	// Authorization is the request header; never logged.
	Authorization string `json:"-"`
}

// Fake is the handler plus its request log. Safe for concurrent use.
type Fake struct {
	// LogPath, if set, receives one JSON line per request.
	LogPath string
	// SlowDelay is the per-word delay of SLOW replies.
	SlowDelay time.Duration

	mu       sync.Mutex
	requests []Request
}

// New returns a Fake with default settings.
func New() *Fake { return &Fake{SlowDelay: 150 * time.Millisecond} }

// Count is the number of requests received.
func (f *Fake) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// Requests returns a copy of the request log.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

type inputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	CallID  string          `json:"call_id"`
	Output  json.RawMessage `json:"output"`
}

type requestBody struct {
	Model     string      `json:"model"`
	Input     []inputItem `json:"input"`
	Reasoning *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func sse(w http.ResponseWriter, flusher http.Flusher, event map[string]any) {
	data, _ := json.Marshal(event)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	flusher.Flush()
}

func usage() map[string]any {
	return map[string]any{
		"input_tokens": 11, "output_tokens": 7, "total_tokens": 18,
		"input_tokens_details":  map[string]any{"cached_tokens": 3},
		"output_tokens_details": map[string]any{"reasoning_tokens": 2},
	}
}

func (f *Fake) record(req Request) int {
	f.mu.Lock()
	req.N = len(f.requests) + 1
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.LogPath != "" {
		if file, err := os.OpenFile(f.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			data, _ := json.Marshal(req)
			fmt.Fprintf(file, "%s\n", data)
			file.Close()
		}
	}
	return req.N
}

// ServeHTTP implements http.Handler for POST <base>/responses.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body requestBody
	_ = json.Unmarshal(raw, &body)
	var last inputItem
	if len(body.Input) > 0 {
		last = body.Input[len(body.Input)-1]
	}
	lastKind := last.Type
	if lastKind == "" {
		lastKind = "message:" + last.Role
	}
	lastText := contentText(last.Content)
	if last.Type == "function_call_output" {
		lastText = contentText(last.Output)
	}
	var userTexts []string
	for _, item := range body.Input {
		if item.Role == "user" {
			userTexts = append(userTexts, contentText(item.Content))
		}
	}
	effort := ""
	if body.Reasoning != nil {
		effort = body.Reasoning.Effort
	}
	n := f.record(Request{Model: body.Model, Effort: effort, Items: len(body.Input), LastKind: lastKind, LastText: lastText, UserTexts: userTexts, Authorization: r.Header.Get("Authorization")})

	flusher := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	id := fmt.Sprintf("resp_%d", n)
	sse(w, flusher, map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": body.Model}})

	command := ""
	if last.Role == "user" && strings.Contains(lastText, "RUN[") {
		start := strings.Index(lastText, "RUN[") + len("RUN[")
		if end := strings.LastIndex(lastText, "]"); end > start {
			command = lastText[start:end]
		}
	}
	var item map[string]any
	if command != "" {
		args, _ := json.Marshal(map[string]string{"command": command})
		callID := fmt.Sprintf("call_%d", n)
		sse(w, flusher, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": callID, "name": "Bash", "arguments": ""}})
		item = map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": callID, "name": "Bash", "arguments": string(args), "status": "completed"}
	} else {
		var text string
		delay := 30 * time.Millisecond
		switch {
		case last.Type == "function_call_output":
			short := lastText
			if len(short) > 60 {
				short = short[:60]
			}
			text = "Tool result: " + short
		case strings.Contains(lastText, "SLOW"):
			text = strings.Repeat("slow words keep coming ", 40)
			delay = f.SlowDelay
		default:
			text = "Echo: " + lastText
		}
		sse(w, flusher, map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "delta": "thinking about it"})
		sse(w, flusher, map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": "msg_" + id, "role": "assistant", "status": "in_progress", "content": []any{}}})
		for _, word := range strings.SplitAfter(text, " ") {
			if r.Context().Err() != nil {
				return
			}
			sse(w, flusher, map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_" + id, "delta": word})
			time.Sleep(delay)
		}
		item = map[string]any{"type": "message", "id": "msg_" + id, "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
	}
	sse(w, flusher, map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	sse(w, flusher, map[string]any{"type": "response.completed", "response": map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "model": body.Model + "-served",
		"output": []any{item}, "usage": usage(),
	}})
}
