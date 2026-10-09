package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Handler serves one method call. A nil *Error with a result is a
// success; params is the raw JSON the caller sent (nil for none).
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, *Error)

// Serve reads requests from r and answers them on w until r ends or ctx
// is done (then r is closed if it can be, to unblock the read). Each
// request is handled on its own goroutine; notifications (no id) are
// handled without an answer. A frame that isn't JSON gets a parse error
// with a null id; anything that can't be read at all ends Serve with
// the error (nil for a clean end of stream).
func Serve(ctx context.Context, r io.Reader, w io.Writer, h Handler) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var wmu sync.Mutex
	write := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		wmu.Lock()
		defer wmu.Unlock()
		_ = WriteFrame(w, b)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			if cl, ok := r.(io.Closer); ok {
				_ = cl.Close()
			}
		case <-stop:
		}
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		payload, err := ReadFrame(br)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var req request
		if err := json.Unmarshal(payload, &req); err != nil || req.Method == "" {
			write(response{JSONRPC: "2.0", ID: nil, Error: &wireError{Code: numParseError, Message: "not a JSON-RPC request"}})
			continue
		}
		wg.Add(1)
		go func(req request) {
			defer wg.Done()
			result, rpcErr := h(ctx, req.Method, req.Params)
			if req.ID == nil {
				return
			}
			if rpcErr != nil {
				write(response{JSONRPC: "2.0", ID: req.ID, Error: toWire(rpcErr)})
				return
			}
			raw, err := json.Marshal(result)
			if err != nil {
				write(response{JSONRPC: "2.0", ID: req.ID, Error: toWire(&Error{Code: CodeInternal, Message: "result can't be encoded"})})
				return
			}
			write(response{JSONRPC: "2.0", ID: req.ID, Result: raw})
		}(req)
	}
}

// Notify sends a notification (no id, no answer) on w; a plugin uses it
// for targets.changed. Safe to call concurrently with Serve's answers
// only through the same writer lock, so plugins send notifications
// through the SDK, which holds it.
func Notify(w io.Writer, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	b, err := json.Marshal(request{JSONRPC: "2.0", Method: method, Params: raw})
	if err != nil {
		return err
	}
	return WriteFrame(w, b)
}
