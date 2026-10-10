package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The Engine API over plain net/http (design §9: no Docker client
// library; the handful of endpoints used are small and the plugin
// controls every request). Requests go to the engine through a dialer
// the transport provides: the local socket, or an SSH session running
// docker system dial-stdio on the docker host.

// apiVersion is the Engine API version every request names: 1.41 is
// Docker 20.10, the oldest the plugin supports.
const apiVersion = "v1.41"

// maxErrorBody bounds what is read of an error answer.
const maxErrorBody = 64 << 10

// dialFunc connects to the engine.
type dialFunc func(ctx context.Context) (net.Conn, error)

// engine is a client of one Docker engine.
type engine struct {
	http      *http.Client
	transport *http.Transport
}

// newEngine is a client over dial. Connections are kept alive and
// reused, so one SSH session serves many requests.
func newEngine(dial dialFunc) *engine {
	t := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:        2,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     60 * time.Second,
		DisableCompression:  true,
	}
	return &engine{http: &http.Client{Transport: t}, transport: t}
}

// Close drops the idle connections (the SSH sessions behind them).
func (e *engine) Close() { e.transport.CloseIdleConnections() }

func (e *engine) closeIdle() { e.transport.CloseIdleConnections() }

// engineError is an answer with an error status, and the daemon's
// message for it (bounded, one line).
type engineError struct {
	Status  int
	Message string
}

func (e *engineError) Error() string {
	return fmt.Sprintf("docker engine: %d: %s", e.Status, e.Message)
}

// isStatus reports whether err is the engine answering status.
func isStatus(err error, status int) bool {
	var ee *engineError
	return errors.As(err, &ee) && ee.Status == status
}

// do sends one request and returns the response when its status is a
// success (2xx or 304); any other status becomes an engineError.
func (e *engine) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	u := "http://docker/" + apiVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 || resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := ""
	var js struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &js) == nil && js.Message != "" {
		msg = js.Message
	} else {
		msg = string(raw)
	}
	msg = oneLine(msg)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return nil, &engineError{Status: resp.StatusCode, Message: msg}
}

// decode reads a JSON answer into out (nil to discard it).
func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNotModified || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("docker engine: reading the answer: %w", err)
	}
	return nil
}

func (e *engine) get(ctx context.Context, path string, query url.Values, out any) error {
	resp, err := e.do(ctx, http.MethodGet, path, query, nil, "")
	if err != nil {
		return err
	}
	return decode(resp, out)
}

func (e *engine) post(ctx context.Context, path string, query url.Values, body, out any) error {
	var r io.Reader
	contentType := ""
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		r, contentType = &buf, "application/json"
	}
	resp, err := e.do(ctx, http.MethodPost, path, query, r, contentType)
	if err != nil {
		return err
	}
	return decode(resp, out)
}

func (e *engine) delete(ctx context.Context, path string, query url.Values) error {
	resp, err := e.do(ctx, http.MethodDelete, path, query, nil, "")
	if err != nil {
		return err
	}
	return decode(resp, nil)
}

// putTar uploads a tar archive (PUT /containers/{id}/archive).
func (e *engine) putTar(ctx context.Context, path string, query url.Values, tar io.Reader) error {
	resp, err := e.do(ctx, http.MethodPut, path, query, tar, "application/x-tar")
	if err != nil {
		return err
	}
	return decode(resp, nil)
}

// stream returns an answer's body for the caller to read (an image
// pull's progress, a container's log).
func (e *engine) stream(ctx context.Context, method, path string, query url.Values) (io.ReadCloser, error) {
	resp, err := e.do(ctx, method, path, query, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ping is GET /_ping: the engine answers, and says its API version.
func (e *engine) ping(ctx context.Context) (string, error) {
	resp, err := e.do(ctx, http.MethodGet, "/_ping", nil, nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
	return resp.Header.Get("API-Version"), nil
}

func oneLine(s string) string {
	return truncate(strings.Join(strings.Fields(s), " "), 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
