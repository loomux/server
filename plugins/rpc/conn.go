package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Error codes a plugin answers with (Error.Code). The host maps them to
// its own error classes.
const (
	CodeInvalidConfig  = "invalid_config"
	CodeUnauthorized   = "unauthorized"
	CodeNotFound       = "not_found"
	CodeQuota          = "quota"
	CodeUnavailable    = "unavailable"
	CodeInternal       = "internal"
	CodeMethodNotFound = "method_not_found"
	CodeInvalidParams  = "invalid_params"
)

// Error is a JSON-RPC error as the protocol defines it: a stable Code
// and a Message safe to show.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ErrClosed is a call on a closed connection, or one closed under it.
var ErrClosed = errors.New("rpc: connection closed")

// The wire shapes.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type wireErrorData struct {
	Code string `json:"code"`
}

// JSON-RPC's own numeric codes, used alongside data.code.
const (
	numParseError     = -32700
	numInvalidRequest = -32600
	numMethodNotFound = -32601
	numInvalidParams  = -32602
	numServer         = -32000
)

func toWire(e *Error) *wireError {
	num := numServer
	switch e.Code {
	case CodeMethodNotFound:
		num = numMethodNotFound
	case CodeInvalidParams:
		num = numInvalidParams
	}
	data, _ := json.Marshal(wireErrorData{Code: e.Code})
	return &wireError{Code: num, Message: e.Message, Data: data}
}

func fromWire(w *wireError) *Error {
	e := &Error{Code: CodeInternal, Message: w.Message}
	var data wireErrorData
	if len(w.Data) > 0 && json.Unmarshal(w.Data, &data) == nil && data.Code != "" {
		e.Code = data.Code
		return e
	}
	switch w.Code {
	case numMethodNotFound:
		e.Code = CodeMethodNotFound
	case numInvalidParams:
		e.Code = CodeInvalidParams
	}
	return e
}

// message is either a request/notification or a response, told apart by
// which fields are set.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *wireError      `json:"error"`
}

// Conn is the host's side of a connection to one plugin: calls go out,
// responses are matched by id, notifications go to onNotify.
type Conn struct {
	r        *bufio.Reader
	w        io.Writer
	wmu      sync.Mutex
	onNotify func(method string, params json.RawMessage)

	mu      sync.Mutex
	next    int64
	pending map[int64]chan *message
	closed  bool
	err     error
	done    chan struct{}
	closers []io.Closer
}

// NewConn starts reading from r; calls are written to w. onNotify, if
// non-nil, receives notifications. If r or w is an io.Closer it is
// closed by Close.
func NewConn(r io.Reader, w io.Writer, onNotify func(method string, params json.RawMessage)) *Conn {
	c := &Conn{
		r:        bufio.NewReaderSize(r, 64<<10),
		w:        w,
		onNotify: onNotify,
		pending:  map[int64]chan *message{},
		done:     make(chan struct{}),
	}
	for _, v := range []any{r, w} {
		if cl, ok := v.(io.Closer); ok {
			c.closers = append(c.closers, cl)
		}
	}
	go c.readLoop()
	return c
}

// Call sends method with params and decodes the result into result (nil
// to ignore it). A JSON-RPC error comes back as *Error; a connection
// failure as ErrClosed (wrapping the cause); a ctx deadline as ctx's
// error, after which a late reply is dropped.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("rpc: marshal params: %w", err)
		}
		raw = b
	}
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	c.next++
	id := c.next
	ch := make(chan *message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	body, err := json.Marshal(request{JSONRPC: "2.0", ID: &id, Method: method, Params: raw})
	if err != nil {
		c.forget(id)
		return fmt.Errorf("rpc: marshal request: %w", err)
	}
	if err := c.write(body); err != nil {
		c.forget(id)
		return fmt.Errorf("%w: write: %v", ErrClosed, err)
	}

	select {
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	case <-c.done:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrClosed, err)
	case m := <-ch:
		if m.Error != nil {
			return fromWire(m.Error)
		}
		if result != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, result); err != nil {
				return fmt.Errorf("rpc: %s result: %w", method, err)
			}
		}
		return nil
	}
}

func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Conn) write(body []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return WriteFrame(c.w, body)
}

func (c *Conn) readLoop() {
	for {
		payload, err := ReadFrame(c.r)
		if err != nil {
			c.fail(err)
			return
		}
		var m message
		if err := json.Unmarshal(payload, &m); err != nil {
			c.fail(fmt.Errorf("%w: %v", ErrBadFrame, err))
			return
		}
		switch {
		case m.ID == nil && m.Method != "":
			if c.onNotify != nil {
				c.onNotify(m.Method, m.Params)
			}
		case m.ID != nil && m.Method == "":
			c.mu.Lock()
			ch := c.pending[*m.ID]
			delete(c.pending, *m.ID)
			c.mu.Unlock()
			if ch != nil {
				mm := m
				ch <- &mm
			}
		default:
			// A request from the plugin: nothing the host serves.
			if m.ID != nil {
				resp, _ := json.Marshal(response{JSONRPC: "2.0", ID: m.ID, Error: toWire(&Error{Code: CodeMethodNotFound, Message: "the host serves no methods"})})
				_ = c.write(resp)
			}
		}
	}
}

// fail ends the connection with err: every pending and later call fails.
func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if err == nil {
		err = io.EOF
	}
	c.err = err
	close(c.done)
	c.pending = map[int64]chan *message{}
	closers := c.closers
	c.mu.Unlock()
	for _, cl := range closers {
		_ = cl.Close()
	}
}

// Close ends the connection; pending calls fail with ErrClosed.
func (c *Conn) Close() error {
	c.fail(errors.New("closed"))
	return nil
}

// Err is why the connection ended, or nil while it is open.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		return nil
	}
	return c.err
}

// Done is closed when the connection has ended.
func (c *Conn) Done() <-chan struct{} { return c.done }
