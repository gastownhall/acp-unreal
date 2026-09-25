package project

import (
	stdjson "encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/llm"
	"github.com/unreallabsai/unreal-agent/harness/tool"
)

func TestSplitTextKeepsRunesAndLimit(t *testing.T) {
	text := strings.Repeat("aé☃", 1000)
	parts := SplitText(text, 100)
	if strings.Join(parts, "") != text {
		t.Fatal("split lost bytes")
	}
	for _, p := range parts {
		if len(p) > 100 || !utf8.ValidString(p) {
			t.Fatalf("bad part len=%d valid=%v", len(p), utf8.ValidString(p))
		}
	}
}

func TestBoundTextKeepsHeadAndTail(t *testing.T) {
	text := "HEAD" + strings.Repeat("☃", 10000) + "TAIL"
	got := BoundText(text, 64)
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") || !strings.Contains(got, "bytes omitted") || !utf8.ValidString(got) {
		t.Fatalf("got %q", got)
	}
	if BoundText("short", 64) != "short" {
		t.Fatal("short text changed")
	}
}

// Model-generated tool arguments can nest arbitrarily; the rawInput sent in
// tool_call must stay bounded however the size is distributed.
func TestDescribeCallBoundsNestedArguments(t *testing.T) {
	nested := map[string]any{"command": "echo hi", "deep": map[string]any{"list": []any{strings.Repeat("z", 5000)}}}
	var many []any
	for range 5000 {
		many = append(many, "short")
	}
	nested["many"] = many
	args, _ := stdjson.Marshal(nested)
	call := llm.ToolCall{CallID: "c1", Name: tool.BashName, Arguments: string(args)}
	title, _, raw := DescribeCall(call, 1024)
	if title != "echo hi" {
		t.Fatalf("title = %q", title)
	}
	encoded, _ := stdjson.Marshal(raw)
	if len(encoded) > 2*1024 {
		t.Fatalf("rawInput is %d bytes for limit 1024", len(encoded))
	}
	small := llm.ToolCall{CallID: "c2", Name: tool.BashName, Arguments: `{"command":"ls","opts":{"a":[1,2]}}`}
	if _, _, raw := DescribeCall(small, 1024); raw["command"] != "ls" || raw["opts"] == nil {
		t.Fatalf("small rawInput changed: %+v", raw)
	}
}
