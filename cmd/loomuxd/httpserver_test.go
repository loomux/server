package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// LOOM-115: a client that never finishes its request headers (slowloris)
// is cut off, instead of holding a connection open for ever.
func TestHTTPServerClosesSlowHeaders(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.ReadHeaderTimeout = 200 * time.Millisecond
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v (want the server to close the connection)", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("closed after %v", time.Since(start))
	}
}

func TestHTTPServerDefaults(t *testing.T) {
	srv := newHTTPServer(":8080", http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 120*time.Second || srv.MaxHeaderBytes != 64<<10 {
		t.Errorf("timeouts = %v / %v / %d", srv.ReadHeaderTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}
	// No WriteTimeout or ReadTimeout: the conversation stream (SSE) and a
	// blocking dispatch stay open for as long as the turn runs.
	if srv.WriteTimeout != 0 || srv.ReadTimeout != 0 {
		t.Errorf("WriteTimeout %v, ReadTimeout %v; want none", srv.WriteTimeout, srv.ReadTimeout)
	}
}

// LOOM-147: serveHTTP returns only after shutdown has finished: a request
// in flight when the signal comes still gets its full answer, and the
// dispatch drain runs before the HTTP server stops.
func TestServeHTTP_WaitsForInFlightRequests(t *testing.T) {
	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	inHandler := make(chan struct{})
	srv := newHTTPServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inHandler)
		time.Sleep(300 * time.Millisecond)
		note("handler done")
		_, _ = io.WriteString(w, "whole answer")
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- serveHTTP(ctx, srv, ln, func() { note("drained") }, 5*time.Second)
		note("serveHTTP returned")
	}()

	body := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			body <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body <- string(b)
	}()
	<-inHandler
	stop()
	if err := <-served; err != nil {
		t.Fatalf("serveHTTP: %v", err)
	}
	if got := <-body; got != "whole answer" {
		t.Fatalf("in-flight request got %q", got)
	}
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	want := []string{"drained", "handler done", "serveHTTP returned"}
	if strings.Join(order, ", ") != strings.Join(want, ", ") {
		t.Fatalf("order = %v, want %v", order, want)
	}
}
