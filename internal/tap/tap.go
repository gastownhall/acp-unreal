// Package tap observes provider SSE streams for UI deltas.
//
// The responsesapi adapter reads provider SSE but keeps only
// response.output_item.done and terminal events (harness/llm/responsesapi/
// stream.go:145-208); text deltas are discarded and decodeResponse drops the
// provider's model id (responsesapi/response.go:13-40). The provider clients
// hardcode primitives.NewRemoteClient(), but responsesapi.NewAdapter and
// primitives.NewRemoteClientWithHTTPClient are exported
// (primitives/remote.go:125-127), so this tee transport observes the same
// bytes the adapter parses. It is best effort and UI-only.
package tap

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Kind classifies a Delta.
type Kind int

// Delta kinds.
const (
	Text Kind = iota + 1
	Thinking
	ToolStart
	Model
)

// Delta is one observed stream event, tagged with the Gate request sequence
// and the HTTP attempt number (the adapter retries internally).
type Delta struct {
	Seq     uint64
	Attempt int
	Kind    Kind
	Text    string
	CallID  string
	Name    string
}

type requestTag struct {
	seq      uint64
	attempts *atomic.Int32
}

type requestTagKey struct{}

// WithRequestTag marks ctx so deltas of requests made with it carry seq.
func WithRequestTag(ctx context.Context, seq uint64) context.Context {
	return context.WithValue(ctx, requestTagKey{}, &requestTag{seq: seq, attempts: &atomic.Int32{}})
}

// Transport tees text/event-stream response bodies into an SSE parser.
type Transport struct {
	Base http.RoundTripper
	Sink func(Delta)
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	tag, _ := request.Context().Value(requestTagKey{}).(*requestTag)
	attempt := 1
	if tag != nil {
		attempt = int(tag.attempts.Add(1))
	}
	response, err := t.Base.RoundTrip(request)
	if err != nil || tag == nil || response == nil || response.Body == nil {
		return response, err
	}
	if mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type")); mediaType != "text/event-stream" {
		return response, nil
	}
	response.Body = &sseTee{ReadCloser: response.Body, emit: func(d Delta) {
		d.Seq, d.Attempt = tag.seq, attempt
		t.Sink(d)
	}}
	return response, nil
}

type sseTee struct {
	io.ReadCloser
	emit func(Delta)
	buf  []byte
}

const maxTeeBuffer = 1 << 20

func (t *sseTee) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if n > 0 {
		t.buf = append(t.buf, p[:n]...)
		for {
			end, width := frameEnd(t.buf)
			if end < 0 {
				break
			}
			ParseFrame(t.buf[:end], t.emit)
			t.buf = t.buf[end+width:]
		}
		if len(t.buf) > maxTeeBuffer {
			t.buf = t.buf[:0]
		}
	}
	return n, err
}

func frameEnd(b []byte) (int, int) {
	lf := bytes.Index(b, []byte("\n\n"))
	crlf := bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case lf < 0 && crlf < 0:
		return -1, 0
	case lf < 0 || (crlf >= 0 && crlf < lf):
		return crlf, 4
	default:
		return lf, 2
	}
}

// ParseFrame decodes one SSE frame (without its terminating blank line) and
// emits the deltas it carries.
func ParseFrame(frame []byte, emit func(Delta)) {
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
		}
	}
	if len(data) == 0 {
		return
	}
	var event struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
		Item  struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		} `json:"item"`
		Response struct {
			Model string `json:"model"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta != "" {
			emit(Delta{Kind: Text, Text: event.Delta})
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if event.Delta != "" {
			emit(Delta{Kind: Thinking, Text: event.Delta})
		}
	case "response.output_item.added":
		if event.Item.Type == "function_call" {
			emit(Delta{Kind: ToolStart, CallID: event.Item.CallID, Name: event.Item.Name})
		}
	case "response.completed", "response.incomplete":
		if event.Response.Model != "" {
			emit(Delta{Kind: Model, Text: event.Response.Model})
		}
	}
}

// NewHTTPClient mirrors primitives.newRemoteHTTPClient (remote.go:142-157)
// with the tee in front.
func NewHTTPClient(sink func(Delta)) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	base := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: &Transport{Base: base, Sink: sink}}
}
