package acpagent

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestGCSessionIDDerivation(t *testing.T) {
	sum := sha256.Sum256([]byte("ga-t/1"))
	want := "gc-ga-t-1-" + hex.EncodeToString(sum[:])[:8] + "-e1"
	if got := string(GCSessionID("ga-t/1", "1")); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := string(GCSessionID("ga-t/1", "")); got != want {
		t.Fatalf("default epoch: got %q want %q", got, want)
	}
	if got := string(GCSessionID("abc-DEF-9", "7")); got != "gc-abc-DEF-9-e7" {
		t.Fatalf("clean id must not get a hash suffix: %q", got)
	}
	if GCSessionID("a/b", "1") == GCSessionID("a_b", "1") {
		t.Fatal("distinct raw ids collapsed to one sanitized id")
	}
	if got := string(GCSessionID("x", "e2x")); got != "gc-x-e2" {
		t.Fatalf("epoch digits: %q", got)
	}
	if !ValidSessionID(string(GCSessionID("ga-t/1 ☃", "3"))) {
		t.Fatal("derived id must satisfy the store's id rule")
	}
}

func TestResolveBoundPriority(t *testing.T) {
	b, err := ResolveBound(env(map[string]string{"GC_SESSION_ID": "s1", "GC_CONTINUATION_EPOCH": "2"}), "K", "R")
	if err != nil || b.Mode != BoundGC || b.ID != "gc-s1-e2" {
		t.Fatalf("gc wins: %+v %v", b, err)
	}
	b, _ = ResolveBound(env(nil), "K", "R")
	if b.Mode != BoundCreate || b.ID != "K" {
		t.Fatalf("--session-id next: %+v", b)
	}
	b, _ = ResolveBound(env(nil), "", "R")
	if b.Mode != BoundResume || b.ID != "R" {
		t.Fatalf("--resume last: %+v", b)
	}
	b, _ = ResolveBound(env(nil), "", "")
	if b.Mode != Unbound {
		t.Fatalf("unbound: %+v", b)
	}
	if _, err := ResolveBound(env(nil), "../etc", ""); err == nil {
		t.Fatal("path-like session id must be rejected")
	}
}

func TestFlattenPrompt(t *testing.T) {
	text, err := FlattenPrompt([]acp.ContentBlock{
		acp.TextBlock("look at this"),
		acp.ResourceBlock(acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{Uri: "file:///a.go", Text: "package a"}}),
		acp.ResourceLinkBlock("b.go", "file:///b.go"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"look at this", "--- file:///a.go ---\npackage a", "[Referenced file: file:///b.go]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	if _, err := FlattenPrompt([]acp.ContentBlock{acp.ImageBlock("AAAA", "image/png")}); !errors.Is(err, ErrUnsupportedContent) {
		t.Fatalf("image: %v", err)
	}
	if _, err := FlattenPrompt([]acp.ContentBlock{acp.AudioBlock("AAAA", "audio/wav")}); !errors.Is(err, ErrUnsupportedContent) {
		t.Fatalf("audio: %v", err)
	}
	if _, err := FlattenPrompt(nil); err == nil {
		t.Fatal("empty prompt accepted")
	}
}

func TestInsideDir(t *testing.T) {
	cases := []struct {
		path, dir string
		want      bool
	}{
		{"/w/state", "/w", true}, {"/w", "/w", true}, {"/w2/state", "/w", false}, {"/state", "/w", false}, {"/w/../x", "/w", false},
	}
	for _, c := range cases {
		if got := insideDir(c.path, c.dir); got != c.want {
			t.Errorf("insideDir(%q,%q)=%v", c.path, c.dir, got)
		}
	}
}

// A nested acp-unreal started by one of this agent's tools inherits
// GC_SESSION_ID; binding to it would collide with the parent ("session
// busy"). The parent exports its own GC_SESSION_ID as ParentGCSessionEnv,
// and a process whose GC_SESSION_ID equals it ignores that ambient identity.
func TestResolveBoundIgnoresAmbientGCIdentityUnderAParentAgent(t *testing.T) {
	nested := env(map[string]string{"GC_SESSION_ID": "s1", "GC_CONTINUATION_EPOCH": "2", ParentGCSessionEnv: "s1"})
	b, err := ResolveBound(nested, "", "")
	if err != nil || b.Mode != Unbound {
		t.Fatalf("nested agent bound to the parent's gc session: %+v %v", b, err)
	}
	b, _ = ResolveBound(nested, "K", "")
	if b.Mode != BoundCreate || b.ID != "K" {
		t.Fatalf("explicit --session-id must still apply: %+v", b)
	}
}

// The parent marker leaks through anything a tool starts, including a gc
// controller, which passes its whole environment to its agents. Those agents
// have their own GC_SESSION_ID and must bind to it.
func TestResolveBoundBindsOwnGCIdentityDespiteALeakedParentMarker(t *testing.T) {
	agent := env(map[string]string{"GC_SESSION_ID": "nested-city-s9", "GC_CONTINUATION_EPOCH": "1", ParentGCSessionEnv: "s1"})
	b, err := ResolveBound(agent, "", "")
	if err != nil || b.Mode != BoundGC || b.ID != GCSessionID("nested-city-s9", "1") {
		t.Fatalf("agent of a nested city did not bind to its own gc session: %+v %v", b, err)
	}
}
