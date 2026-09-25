package gate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/unreallabsai/unreal-agent/harness/llm"

	"github.com/gastownhall/acp-unreal/internal/tap"
)

type adapterFunc func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error)

func (f adapterFunc) Respond(ctx context.Context, r llm.Request, o llm.RequestOptions) (llm.Response, error) {
	return f(ctx, r, o)
}

func TestCancelBecomesCompletedResponseWithPartialText(t *testing.T) {
	started := make(chan struct{})
	var g *Gate
	g = New(adapterFunc(func(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		g.ObserveDelta(tap.Delta{Seq: 1, Attempt: 1, Kind: tap.Text, Text: "half an "})
		g.ObserveDelta(tap.Delta{Seq: 1, Attempt: 1, Kind: tap.Text, Text: "answer"})
		close(started)
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}), Options{Model: func() string { return "m1" }})

	go func() { <-started; g.CancelInflight() }()
	resp, err := g.Respond(context.Background(), llm.Request{}, llm.RequestOptions{})
	if err != nil {
		t.Fatalf("cancel must not surface an error (fatal to coordinator.Run): %v", err)
	}
	if resp.Stop != llm.StopComplete {
		t.Fatalf("stop = %q, want complete", resp.Stop)
	}
	if len(resp.Output) != 1 || resp.Output[0].Data.(llm.Message).Text != "half an answer" {
		t.Fatalf("partial text not kept: %+v", resp.Output)
	}
	rec, ok := g.Lookup(resp.ID)
	if !ok || rec.Outcome != Cancelled || rec.Model != "m1" {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}
}

func TestProviderErrorBecomesEmptyCompletedResponse(t *testing.T) {
	var reported error
	g := New(adapterFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		return llm.Response{}, errors.New("503")
	}), Options{OnError: func(_ uint64, err error) { reported = err }})
	resp, err := g.Respond(context.Background(), llm.Request{}, llm.RequestOptions{})
	if err != nil || resp.Stop != llm.StopComplete || len(resp.Output) != 0 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	if reported == nil {
		t.Fatal("OnError not called")
	}
}

func TestSupersededRequestReturnsCtxError(t *testing.T) {
	g := New(adapterFunc(func(ctx context.Context, _ llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		<-ctx.Done()
		return llm.Response{}, ctx.Err()
	}), Options{})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := g.Respond(ctx, llm.Request{}, llm.RequestOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (coordinator drops superseded results)", err)
	}
}

func TestParkMutesOnlyParkedReasons(t *testing.T) {
	calls := 0
	reasons := []string{"call:c1", "input:i1"}
	g := New(adapterFunc(func(context.Context, llm.Request, llm.RequestOptions) (llm.Response, error) {
		calls++
		return llm.Response{ID: "r", Stop: llm.StopComplete}, nil
	}), Options{Reasons: func() []string { return reasons }, Model: func() string { return "m" }})
	g.Park([]string{"c1"}, []string{"i1"})
	resp, _ := g.Respond(context.Background(), llm.Request{}, llm.RequestOptions{})
	if calls != 0 {
		t.Fatalf("parked request reached provider")
	}
	if rec, _ := g.Lookup(resp.ID); rec.Outcome != Muted {
		t.Fatalf("outcome = %v, want Muted", rec.Outcome)
	}
	reasons = []string{"input:new"}
	if _, err := g.Respond(context.Background(), llm.Request{}, llm.RequestOptions{}); err != nil || calls != 1 {
		t.Fatalf("new input must unpark: calls=%d err=%v", calls, err)
	}
	reasons = []string{"call:c1"}
	if g.Respond(context.Background(), llm.Request{}, llm.RequestOptions{}); calls != 2 {
		t.Fatalf("park must be cleared after an unparked request; calls=%d", calls)
	}
	if g.ProviderRequests() != 2 {
		t.Fatalf("ProviderRequests=%d", g.ProviderRequests())
	}
}

func TestModelOverrideAndServedModel(t *testing.T) {
	var sent string
	var g *Gate
	g = New(adapterFunc(func(_ context.Context, r llm.Request, _ llm.RequestOptions) (llm.Response, error) {
		sent = r.Model.ID
		g.ObserveDelta(tap.Delta{Seq: 1, Kind: tap.Model, Text: "m2-served"})
		return llm.Response{ID: "x", Stop: llm.StopComplete}, nil
	}), Options{Model: func() string { return "m2" }})
	resp, _ := g.Respond(context.Background(), llm.Request{Model: llm.Model{ID: "m1"}}, llm.RequestOptions{})
	rec, _ := g.Lookup(resp.ID)
	if sent != "m2" || rec.Model != "m2-served" {
		t.Fatalf("sent=%q served=%q", sent, rec.Model)
	}
}
