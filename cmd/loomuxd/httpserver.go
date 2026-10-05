package main

import (
	"net/http"
	"time"
)

// newHTTPServer is an http.Server for addr with the timeouts a server
// facing the internet needs (LOOM-115): request headers must arrive
// within ReadHeaderTimeout (no slowloris), and an idle keep-alive
// connection is closed after IdleTimeout. There is deliberately no
// ReadTimeout or WriteTimeout: the conversation stream (SSE) and a
// blocking dispatch stay open for as long as the turn runs.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
}
