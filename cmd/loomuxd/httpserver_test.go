package main

import (
	"io"
	"net"
	"net/http"
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
