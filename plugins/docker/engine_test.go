package docker

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tcpDial is a dialFunc to an httptest server.
func tcpDial(srv *httptest.Server) dialFunc {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}
}

// The client speaks API v1.41 under the versioned prefix, decodes JSON
// answers and turns the daemon's error bodies into engineErrors.
func TestEngineRequests(t *testing.T) {
	var gotPath, gotQuery, gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotType = r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		switch {
		case r.URL.Path == "/v1.41/_ping":
			w.Header().Set("API-Version", "1.47")
			io.WriteString(w, "OK")
		case r.URL.Path == "/v1.41/containers/json":
			io.WriteString(w, `[{"Id":"abc","Names":["/lx-1"]}]`)
		case r.URL.Path == "/v1.41/containers/create":
			w.WriteHeader(201)
			io.WriteString(w, `{"Id":"new","Warnings":[]}`)
		case r.URL.Path == "/v1.41/containers/started/start":
			w.WriteHeader(304)
		case r.URL.Path == "/v1.41/containers/gone/json":
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"No such container: gone"}`)
		case r.URL.Path == "/v1.41/containers/broken/json":
			w.WriteHeader(500)
			io.WriteString(w, "plain text "+strings.Repeat("x", 500))
		case r.URL.Path == "/v1.41/containers/c/archive":
			w.WriteHeader(200)
		case r.URL.Path == "/v1.41/containers/c":
			w.WriteHeader(204)
		case r.URL.Path == "/v1.41/images/create":
			io.WriteString(w, `{"status":"Pulling"}`+"\n"+`{"status":"Done"}`+"\n")
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	e := newEngine(tcpDial(srv))
	defer e.Close()

	v, err := e.ping(ctx)
	if err != nil || v != "1.47" {
		t.Errorf("ping = %q, %v", v, err)
	}
	var list []struct{ Id string }
	if err := e.get(ctx, "/containers/json", url.Values{"all": {"1"}, "filters": {`{"label":["a=b"]}`}}, &list); err != nil || len(list) != 1 || list[0].Id != "abc" {
		t.Errorf("get = %+v, %v", list, err)
	}
	if gotQuery != `all=1&filters=%7B%22label%22%3A%5B%22a%3Db%22%5D%7D` {
		t.Errorf("query = %q", gotQuery)
	}
	var created struct{ Id string }
	if err := e.post(ctx, "/containers/create", url.Values{"name": {"lx-1"}}, map[string]any{"Image": "x"}, &created); err != nil || created.Id != "new" {
		t.Errorf("post = %+v, %v", created, err)
	}
	if gotType != "application/json" || string(gotBody) != `{"Image":"x"}`+"\n" {
		t.Errorf("post sent %q %q", gotType, gotBody)
	}
	// 304 (already started) is success.
	if err := e.post(ctx, "/containers/started/start", nil, nil, nil); err != nil {
		t.Errorf("post 304: %v", err)
	}
	err = e.get(ctx, "/containers/gone/json", nil, &created)
	var ee *engineError
	if !errors.As(err, &ee) || ee.Status != 404 || ee.Message != "No such container: gone" || !isStatus(err, 404) {
		t.Errorf("404 = %v", err)
	}
	err = e.get(ctx, "/containers/broken/json", nil, &created)
	if !errors.As(err, &ee) || ee.Status != 500 || !strings.HasPrefix(ee.Message, "plain text") || len(ee.Message) > 310 {
		t.Errorf("500 = %v", err)
	}
	if err := e.putTar(ctx, "/containers/c/archive", url.Values{"path": {"/ssh"}}, strings.NewReader("TARBYTES")); err != nil {
		t.Errorf("putTar: %v", err)
	}
	if gotPath != "/v1.41/containers/c/archive" || gotType != "application/x-tar" || string(gotBody) != "TARBYTES" {
		t.Errorf("putTar sent %q %q %q", gotPath, gotType, gotBody)
	}
	if err := e.delete(ctx, "/containers/c", url.Values{"v": {"1"}, "force": {"1"}}); err != nil {
		t.Errorf("delete: %v", err)
	}
	body, err := e.stream(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {"x"}})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	all, _ := io.ReadAll(body)
	body.Close()
	if !strings.Contains(string(all), "Done") {
		t.Errorf("stream = %q", all)
	}
	// A cancelled context ends a request.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.get(cctx, "/_ping", nil, nil); err == nil {
		t.Error("a cancelled context should fail the request")
	}
}

// socketPath is a unix socket path short enough to bind (108 bytes).
func socketPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "e.sock")
	if len(p) > 90 {
		dir, err := os.MkdirTemp("/tmp", "loomux-docker-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		p = filepath.Join(dir, "e.sock")
	}
	return p
}

// serveUnix serves h on a unix socket and returns its path.
func serveUnix(t *testing.T, h http.Handler) string {
	t.Helper()
	path := socketPath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		l.Close()
	})
	return path
}

func pingHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1.41/_ping" {
			w.Header().Set("API-Version", "1.47")
			io.WriteString(w, "OK")
			return
		}
		w.WriteHeader(404)
		io.WriteString(w, `{"message":"page not found"}`)
	})
}

func TestEngineOverUnixSocket(t *testing.T) {
	path := serveUnix(t, pingHandler())
	e := newEngine(unixDial(path))
	defer e.Close()
	if v, err := e.ping(ctx); err != nil || v != "1.47" {
		t.Errorf("ping = %q, %v", v, err)
	}
	e2 := newEngine(unixDial(path + ".missing"))
	defer e2.Close()
	if _, err := e2.ping(ctx); err == nil {
		t.Error("a missing socket should fail")
	}
}
