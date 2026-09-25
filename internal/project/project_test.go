package project

import (
	"strings"
	"testing"
	"unicode/utf8"
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
