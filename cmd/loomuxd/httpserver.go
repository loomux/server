package main

import (
	"context"
	"errors"
	"net"
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

// serveHTTP serves srv on ln until ctx is done, then shuts down: drain
// runs first (dispatch jobs get their drain time, LOOM-80), then srv and
// others stop, with grace for in-flight requests to finish. It returns
// only once all of that is over (LOOM-147): Serve itself returns as soon
// as Shutdown begins, and the caller closes the store after us.
func serveHTTP(ctx context.Context, srv *http.Server, ln net.Listener, drain func(), grace time.Duration, others ...*http.Server) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		if drain != nil {
			drain()
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		for _, o := range others {
			_ = o.Shutdown(shutdownCtx)
		}
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}
