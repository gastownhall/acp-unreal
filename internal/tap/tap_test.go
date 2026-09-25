package tap

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTransportTeesDeltasAndModel(t *testing.T) {
	body := "event: x\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"hm\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\r\n\r\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"Bash\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"m-served\"}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
	defer srv.Close()
	var got []Delta
	client := &http.Client{Transport: &Transport{Base: http.DefaultTransport, Sink: func(d Delta) { got = append(got, d) }}}
	req, _ := http.NewRequestWithContext(WithRequestTag(t.Context(), 7), "POST", srv.URL, strings.NewReader("{}"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	passed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(passed) != body {
		t.Fatal("tee altered the body the adapter parses")
	}
	var text string
	kinds := map[Kind]int{}
	for _, d := range got {
		if d.Seq != 7 || d.Attempt != 1 {
			t.Fatalf("tag lost: %+v", d)
		}
		kinds[d.Kind]++
		if d.Kind == Text {
			text += d.Text
		}
	}
	if text != "Hello" || kinds[Thinking] != 1 || kinds[ToolStart] != 1 || kinds[Model] != 1 {
		t.Fatalf("deltas = %+v", got)
	}
}
