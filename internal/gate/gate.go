// Package gate provides the llm.Adapter decorator the coordinator sees.
//
// A model error with a live ctx is fatal to coordinator.Run
// (harness/coordinator/loop.go:225-232), and the harness has no "cancel this
// turn but keep running" control (inbox/inbox.go:48-55). So every outcome the
// harness must survive -- a user cancel, a provider failure -- is answered as
// an empty COMPLETED Response, which is persisted and leaves the coordinator
// idle. After a cancel the Gate parks the cancelled calls and inputs: a
// request that only answers parked reasons is muted (answered empty without
// calling the provider) until a request answers something new, so a later
// generation does not silently re-answer a cancelled prompt. Pattern from
// andreylukin/bough (Apache-2.0) go/docs/unreal-engine.md section 9.6.
package gate

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/unreallabsai/unreal-agent/harness/llm"

	"github.com/gastownhall/acp-unreal/internal/tap"
)

// Outcome says how the Gate answered a request.
type Outcome int

// Outcomes.
const (
	Provider Outcome = iota + 1
	Muted
	Cancelled
	Errored
)

// Record describes one answered request, keyed by the Response.ID the Gate
// returned (the harness never reads Response.ID itself).
type Record struct {
	Seq     uint64
	Outcome Outcome
	Model   string
}

// Options wires the Gate to its session.
type Options struct {
	// Reasons returns what the newest request answers ("input:<id>",
	// "call:<id>", "heartbeat:<id>"); it must be current when Respond runs.
	Reasons func() []string
	// Model returns the model id to send; it overrides Request.Model.ID.
	Model func() string
	// OnError is told about provider failures that were answered empty.
	OnError func(seq uint64, err error)
}

// Gate is an llm.Adapter decorator. Safe for concurrent use.
type Gate struct {
	inner llm.Adapter
	opts  Options

	mu             sync.Mutex
	seq            uint64
	parked         bool
	parkedCalls    map[string]bool
	parkedInputs   map[string]bool
	inflightCancel context.CancelFunc
	inflightSeq    uint64
	userStop       uint64
	partial        map[uint64]*strings.Builder
	partialAttempt map[uint64]int
	records        map[string]Record
	served         map[uint64]string
	providerCalls  int
}

var _ llm.Adapter = (*Gate)(nil)

// New wraps inner.
func New(inner llm.Adapter, opts Options) *Gate {
	if opts.Reasons == nil {
		opts.Reasons = func() []string { return nil }
	}
	if opts.OnError == nil {
		opts.OnError = func(uint64, error) {}
	}
	return &Gate{
		inner: inner, opts: opts,
		parkedCalls: map[string]bool{}, parkedInputs: map[string]bool{},
		partial: map[uint64]*strings.Builder{}, partialAttempt: map[uint64]int{},
		records: map[string]Record{}, served: map[uint64]string{},
	}
}

// Park mutes every request that only answers the given calls and inputs,
// until the first request that answers anything else.
func (g *Gate) Park(calls, inputs []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.parked = true
	for _, id := range calls {
		g.parkedCalls[id] = true
	}
	for _, id := range inputs {
		g.parkedInputs[id] = true
	}
}

// CancelInflight stops the provider request in flight; Respond then answers
// it as a completed Response carrying the text streamed so far.
func (g *Gate) CancelInflight() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflightCancel != nil {
		g.userStop = g.inflightSeq
		g.inflightCancel()
	}
}

// CurrentSeq is the sequence number of the newest request.
func (g *Gate) CurrentSeq() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inflightSeq
}

// Lookup returns the record for a Response.ID the Gate produced.
func (g *Gate) Lookup(responseID string) (Record, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	record, ok := g.records[responseID]
	return record, ok
}

// ProviderRequests counts requests forwarded to the provider.
func (g *Gate) ProviderRequests() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.providerCalls
}

// ObserveDelta keeps the partial assistant text and served model per request.
func (g *Gate) ObserveDelta(d tap.Delta) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch d.Kind {
	case tap.Text:
		if b := g.partial[d.Seq]; b == nil || g.partialAttempt[d.Seq] != d.Attempt {
			g.partialAttempt[d.Seq] = d.Attempt
			g.partial[d.Seq] = &strings.Builder{}
		}
		g.partial[d.Seq].WriteString(d.Text)
	case tap.Model:
		g.served[d.Seq] = d.Text
	}
}

func (g *Gate) allParkedLocked(reasons []string) bool {
	for _, reason := range reasons {
		kind, id, _ := strings.Cut(reason, ":")
		switch kind {
		case "call":
			if !g.parkedCalls[id] {
				return false
			}
		case "input":
			if !g.parkedInputs[id] {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Respond implements llm.Adapter.
func (g *Gate) Respond(ctx context.Context, request llm.Request, options llm.RequestOptions) (llm.Response, error) {
	model := ""
	if g.opts.Model != nil {
		model = g.opts.Model() // may take the runtime lock; never call it while holding g.mu
	}
	reasons := g.opts.Reasons()
	g.mu.Lock()
	g.seq++
	seq := g.seq
	if g.parked {
		if g.allParkedLocked(reasons) {
			id := fmt.Sprintf("acp-unreal-muted-%d", seq)
			g.records[id] = Record{Seq: seq, Outcome: Muted}
			g.mu.Unlock()
			return llm.Response{ID: id, Stop: llm.StopComplete}, nil
		}
		g.parked = false
		clear(g.parkedCalls)
		clear(g.parkedInputs)
	}
	child, cancel := context.WithCancel(ctx)
	g.inflightCancel, g.inflightSeq = cancel, seq
	g.providerCalls++
	g.mu.Unlock()

	if model != "" {
		request.Model.ID = model
	} else {
		model = request.Model.ID
	}
	response, err := g.inner.Respond(tap.WithRequestTag(child, seq), request, options)
	cancel()

	g.mu.Lock()
	g.inflightCancel = nil
	stopped := g.userStop == seq
	partial := ""
	if builder := g.partial[seq]; builder != nil {
		partial = builder.String()
	}
	delete(g.partial, seq)
	delete(g.partialAttempt, seq)
	served := g.served[seq]
	delete(g.served, seq)
	if served == "" {
		served = model
	}
	g.mu.Unlock()

	switch {
	case ctx.Err() != nil:
		// Superseded by the coordinator (a native steer); it drops this
		// result (loop.go:153-156).
		return llm.Response{}, ctx.Err()
	case stopped:
		id := fmt.Sprintf("acp-unreal-cancel-%d", seq)
		var output []llm.Item
		if partial != "" {
			output = []llm.Item{{Type: llm.ItemMessage, Data: llm.Message{Role: llm.RoleAssistant, Text: partial}}}
		}
		g.remember(id, Record{Seq: seq, Outcome: Cancelled, Model: served})
		return llm.Response{ID: id, Stop: llm.StopComplete, Output: output}, nil
	case err != nil:
		id := fmt.Sprintf("acp-unreal-error-%d", seq)
		g.remember(id, Record{Seq: seq, Outcome: Errored, Model: served})
		g.opts.OnError(seq, err)
		return llm.Response{ID: id, Stop: llm.StopComplete}, nil
	default:
		if response.ID == "" {
			response.ID = fmt.Sprintf("acp-unreal-%d", seq)
		}
		g.remember(response.ID, Record{Seq: seq, Outcome: Provider, Model: served})
		return response, nil
	}
}

func (g *Gate) remember(id string, record Record) {
	g.mu.Lock()
	g.records[id] = record
	g.mu.Unlock()
}
