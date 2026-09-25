// Command responses serves the scripted fake Responses API on 127.0.0.1 for
// manual smoke tests. It prints "http://<addr>/v1" (use it as --base-url).
// FAKE_LOG, if set, receives one JSON line per request.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"

	"github.com/gastownhall/acp-unreal/internal/testfake/responses"
)

func main() {
	fake := responses.New()
	fake.LogPath = os.Getenv("FAKE_LOG")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("http://%s/v1\n", listener.Addr())
	mux := http.NewServeMux()
	mux.Handle("POST /v1/responses", fake)
	log.Fatal(http.Serve(listener, mux))
}
