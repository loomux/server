package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(buf.String(), "Content-Length: 7\r\n\r\n") {
		t.Errorf("frame = %q", buf.String())
	}
	got, err := ReadFrame(bufio.NewReader(&buf))
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("ReadFrame = %q, %v", got, err)
	}
}

func TestReadFrameRefuses(t *testing.T) {
	cases := map[string]struct {
		in   string
		want error
	}{
		"garbage":      {"hello\n", ErrBadFrame},
		"no length":    {"Content-Type: x\r\n\r\n", ErrBadFrame},
		"bad length":   {"Content-Length: x\r\n\r\n", ErrBadFrame},
		"too large":    {"Content-Length: 2000000\r\n\r\n", ErrFrameTooLarge},
		"unterminated": {"Content-Length: 3", ErrBadFrame},
		"long line":    {"Content-Length: " + strings.Repeat("1", 300) + "\r\n\r\n", ErrBadFrame},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ReadFrame(bufio.NewReader(strings.NewReader(c.in)))
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
	if _, err := ReadFrame(bufio.NewReader(strings.NewReader(""))); !errors.Is(err, io.EOF) {
		t.Errorf("empty stream: want EOF, got %v", err)
	}
	if err := WriteFrame(io.Discard, make([]byte, MaxFrameBytes+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("WriteFrame oversize: %v", err)
	}
}

// pair connects a Conn to a Serve over pipes and returns the conn.
func pair(t *testing.T, h Handler) (*Conn, context.CancelFunc) {
	t.Helper()
	hostR, pluginW := io.Pipe()
	pluginR, hostW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Serve(ctx, pluginR, pluginW, h)
		_ = pluginW.Close()
	}()
	c := NewConn(hostR, hostW, nil)
	t.Cleanup(func() {
		cancel()
		c.Close()
		<-done
	})
	return c, cancel
}

func echo(ctx context.Context, method string, params json.RawMessage) (any, *Error) {
	switch method {
	case "echo":
		var v map[string]any
		_ = json.Unmarshal(params, &v)
		return v, nil
	case "slow":
		select {
		case <-time.After(2 * time.Second):
			return "late", nil
		case <-ctx.Done():
			return nil, &Error{Code: CodeInternal, Message: "cancelled"}
		}
	case "quota":
		return nil, &Error{Code: CodeQuota, Message: "too many"}
	}
	return nil, &Error{Code: CodeMethodNotFound, Message: "no " + method}
}

func TestCallRoundTrip(t *testing.T) {
	c, _ := pair(t, echo)
	var out map[string]any
	if err := c.Call(context.Background(), "echo", map[string]any{"x": "y"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["x"] != "y" {
		t.Errorf("result = %v", out)
	}
	err := c.Call(context.Background(), "quota", nil, nil)
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeQuota || rpcErr.Message != "too many" {
		t.Errorf("quota: %v", err)
	}
	err = c.Call(context.Background(), "nope", nil, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeMethodNotFound {
		t.Errorf("unknown method: %v", err)
	}
}

func TestConcurrentCalls(t *testing.T) {
	c, _ := pair(t, echo)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out map[string]any
			if err := c.Call(context.Background(), "echo", map[string]any{"i": float64(i)}, &out); err != nil {
				t.Error(err)
				return
			}
			if out["i"] != float64(i) {
				t.Errorf("call %d got %v", i, out)
			}
		}(i)
	}
	wg.Wait()
}

func TestCallDeadline(t *testing.T) {
	c, _ := pair(t, echo)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := c.Call(ctx, "slow", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	// The connection is still usable, and the late reply is dropped.
	var out map[string]any
	if err := c.Call(context.Background(), "echo", map[string]any{"ok": true}, &out); err != nil || out["ok"] != true {
		t.Errorf("after a timeout: %v %v", out, err)
	}
}

func TestNotifications(t *testing.T) {
	hostR, pluginW := io.Pipe()
	_, hostW := io.Pipe()
	got := make(chan string, 1)
	c := NewConn(hostR, hostW, func(method string, params json.RawMessage) {
		got <- method + ":" + string(params)
	})
	defer c.Close()
	if err := Notify(pluginW, "targets.changed", map[string]string{"id": "x"}); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s != `targets.changed:{"id":"x"}` {
			t.Errorf("notification = %q", s)
		}
	case <-time.After(time.Second):
		t.Fatal("notification not delivered")
	}
}

func TestConnRejectsGarbage(t *testing.T) {
	hostR, pluginW := io.Pipe()
	_, hostW := io.Pipe()
	c := NewConn(hostR, hostW, nil)
	defer c.Close()
	go func() { _, _ = pluginW.Write([]byte("starting up...\n")) }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := c.Call(ctx, "echo", nil, nil)
	if !errors.Is(err, ErrClosed) || !errors.Is(c.Err(), ErrBadFrame) {
		t.Fatalf("garbage: call %v, conn err %v", err, c.Err())
	}
}

func TestCloseFailsPending(t *testing.T) {
	c, _ := pair(t, echo)
	errCh := make(chan error, 1)
	go func() { errCh <- c.Call(context.Background(), "slow", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	c.Close()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("want ErrClosed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending call not failed by Close")
	}
	if err := c.Call(context.Background(), "echo", nil, nil); !errors.Is(err, ErrClosed) {
		t.Errorf("call after close: %v", err)
	}
}

func TestServeParseError(t *testing.T) {
	pluginR, hostW := io.Pipe()
	hostR, pluginW := io.Pipe()
	go func() { _ = Serve(context.Background(), pluginR, pluginW, echo) }()
	if err := WriteFrame(hostW, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	payload, err := ReadFrame(bufio.NewReader(hostR))
	if err != nil {
		t.Fatal(err)
	}
	var resp response
	if err := json.Unmarshal(payload, &resp); err != nil || resp.Error == nil || resp.Error.Code != numParseError || resp.ID != nil {
		t.Errorf("parse error response = %s", payload)
	}
	_ = hostW.Close()
}

func TestWireErrorMapping(t *testing.T) {
	for _, code := range []string{CodeMethodNotFound, CodeInvalidParams, CodeQuota, CodeUnavailable} {
		if got := fromWire(toWire(&Error{Code: code, Message: "m"})); got.Code != code || got.Message != "m" {
			t.Errorf("%s round-tripped as %+v", code, got)
		}
	}
	if got := fromWire(&wireError{Code: numMethodNotFound, Message: "x"}); got.Code != CodeMethodNotFound {
		t.Errorf("numeric -32601 without data = %+v", got)
	}
	if got := fromWire(&wireError{Code: -1, Message: "x"}); got.Code != CodeInternal {
		t.Errorf("unknown numeric = %+v", got)
	}
}

// LOOM-213: a plugin's notifications share Serve's writer, so they and
// its answers arrive as whole frames however they interleave.
func TestServeWriterNotifiesBetweenAnswers(t *testing.T) {
	hostR, pluginW := io.Pipe()
	pluginR, hostW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	w := NewWriter(pluginW)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ServeWriter(ctx, pluginR, w, echo)
		_ = pluginW.Close()
	}()
	var mu sync.Mutex
	notes := 0
	c := NewConn(hostR, hostW, func(method string, params json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		if method == "targets.changed" {
			notes++
		}
	})
	defer func() {
		cancel()
		c.Close()
		<-done
	}()
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := w.Notify("targets.changed", map[string]string{"id": "m", "pad": strings.Repeat("x", 4000)}); err != nil {
				t.Error(err)
			}
		}()
		go func(i int) {
			defer wg.Done()
			var out map[string]any
			if err := c.Call(context.Background(), "echo", map[string]any{"i": float64(i), "pad": strings.Repeat("y", 4000)}, &out); err != nil || out["i"] != float64(i) {
				t.Errorf("call %d: %v %v", i, out, err)
			}
		}(i)
	}
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		got := notes
		mu.Unlock()
		if got == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifications delivered = %d, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Notifications are hints: a host that handles none (an older one, nil
// onNotify) drops them and goes on serving calls, and one that handles
// some gets methods it doesn't know too, to ignore.
func TestConnToleratesNotifications(t *testing.T) {
	for _, handles := range []bool{false, true} {
		hostR, pluginW := io.Pipe()
		pluginR, hostW := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		w := NewWriter(pluginW)
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = ServeWriter(ctx, pluginR, w, echo)
			_ = pluginW.Close()
		}()
		got := make(chan string, 4)
		var onNotify func(string, json.RawMessage)
		if handles {
			onNotify = func(method string, _ json.RawMessage) { got <- method }
		}
		c := NewConn(hostR, hostW, onNotify)
		for _, method := range []string{"targets.changed", "plugin.from_the_future"} {
			if err := w.Notify(method, map[string]string{"id": "x"}); err != nil {
				t.Fatal(err)
			}
		}
		var out map[string]any
		if err := c.Call(context.Background(), "echo", map[string]any{"ok": true}, &out); err != nil || out["ok"] != true {
			t.Errorf("handles=%v: a call after notifications: %v %v", handles, out, err)
		}
		if handles && (<-got != "targets.changed" || <-got != "plugin.from_the_future") {
			t.Error("notifications not delivered in order")
		}
		cancel()
		c.Close()
		<-done
	}
}

// The other way: a notification to a plugin (a method it doesn't know,
// from a newer host) gets no answer, and the next call is served.
func TestServeAnswersNoNotification(t *testing.T) {
	hostR, pluginW := io.Pipe()
	pluginR, hostW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, pluginR, pluginW, echo) }()
	go func() {
		_ = Notify(hostW, "host.from_the_future", map[string]string{"x": "y"})
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "echo", "params": map[string]any{"n": 1}})
		_ = WriteFrame(hostW, b)
	}()
	frame, err := ReadFrame(bufio.NewReader(hostR))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		ID     *int            `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(frame, &resp); err != nil || resp.ID == nil || *resp.ID != 7 || string(resp.Result) != `{"n":1}` {
		t.Errorf("first frame from the plugin = %s, want the answer to call 7 (a notification gets none)", frame)
	}
}
