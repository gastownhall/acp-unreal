package acpagent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/unreallabsai/unreal-agent/harness/session"
	"github.com/unreallabsai/unreal-agent/harness/sessionstore/localfile"

	"github.com/gastownhall/acp-unreal/internal/runtime"
)

// session/list pages its answer: an unpaginated list of 6500 sessions was a
// single 1.2 MB stdout line, over gc's 1 MiB line limit.
func TestListSessionsPaginates(t *testing.T) {
	layout := runtime.Layout{Root: t.TempDir()}
	store, err := localfile.New(layout.SessionsDir())
	if err != nil {
		t.Fatal(err)
	}
	const total = 2*listPageSize + 7
	for i := range total {
		id := session.ID(fmt.Sprintf("s-%04d", i))
		if _, err := store.Create(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		cwd := "/ws/a"
		if i%2 == 1 {
			cwd = "/ws/b"
		}
		if err := layout.WriteMeta(id, runtime.Meta{Cwd: cwd}); err != nil {
			t.Fatal(err)
		}
	}
	a := New(Config{Runtime: runtime.Config{Layout: layout, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, DefaultModel: "m"})
	seen := map[acp.SessionId]bool{}
	pages := 0
	var cursor *string
	for {
		resp, err := a.ListSessions(context.Background(), acp.ListSessionsRequest{Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(resp.Sessions) > listPageSize {
			t.Fatalf("page of %d sessions", len(resp.Sessions))
		}
		for _, s := range resp.Sessions {
			if seen[s.SessionId] {
				t.Fatalf("session %s listed twice", s.SessionId)
			}
			seen[s.SessionId] = true
		}
		if resp.NextCursor == nil {
			break
		}
		cursor = resp.NextCursor
	}
	if len(seen) != total || pages != 3 {
		t.Fatalf("listed %d sessions in %d pages, want %d in 3", len(seen), pages, total)
	}
	cwd := "/ws/b"
	resp, err := a.ListSessions(context.Background(), acp.ListSessionsRequest{Cwd: &cwd})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.Sessions {
		if s.Cwd != cwd {
			t.Fatalf("cwd filter leaked %+v", s)
		}
	}
	bad := "%%%"
	if _, err := a.ListSessions(context.Background(), acp.ListSessionsRequest{Cursor: &bad}); err == nil {
		t.Fatal("an invalid cursor must be rejected")
	}
}
