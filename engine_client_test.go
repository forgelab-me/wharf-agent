package main

import (
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Every poll builds a new client for the engine's socket. If it kept its
// connection alive, each one would leave a connection and two goroutines
// behind for good (found when the agent was killed for lack of memory).
func TestEngineClientsLeaveNoConnectionBehind(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no unix sockets here:", err)
	}
	var closed atomic.Int32
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("{}")) }),
		ConnState: func(_ net.Conn, s http.ConnState) {
			if s == http.StateClosed {
				closed.Add(1)
			}
		},
	}
	go srv.Serve(l)
	defer srv.Close()

	const calls = 30
	for i := 0; i < calls; i++ {
		resp, err := unixClient(sock).Get("http://docker/volumes")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for closed.Load() < calls && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := closed.Load(); got != calls {
		t.Errorf("%d of %d connections were closed: the others stay open for good", got, calls)
	}
}
