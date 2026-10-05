package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Loomux/server/api"
	"github.com/Loomux/server/webbundle"
)

// fakeWeb is a WebBundles serving one of two directories.
type fakeWeb struct {
	dirs      [2]string
	cur       int
	current   [2]*webbundle.Release
	latest    *webbundle.Release
	enabled   bool
	installed bool
}

func (f *fakeWeb) Root() string                { return f.dirs[f.cur] }
func (f *fakeWeb) Current() *webbundle.Release { return f.current[f.cur] }
func (f *fakeWeb) Installed() bool             { return f.cur == 1 }
func (f *fakeWeb) Enabled() bool               { return f.enabled }
func (f *fakeWeb) Latest(context.Context) (*webbundle.Release, error) {
	if !f.enabled {
		return nil, webbundle.ErrDisabled
	}
	return f.latest, nil
}
func (f *fakeWeb) Previous() *webbundle.Release {
	if f.cur == 1 {
		return f.current[0]
	}
	return nil
}
func (f *fakeWeb) UpdateAvailable(l *webbundle.Release) bool {
	return l != nil && l.BuiltAt.After(f.Current().BuiltAt)
}
func (f *fakeWeb) Install(context.Context) (*webbundle.Release, error) {
	if f.cur == 1 {
		return nil, webbundle.ErrUpToDate
	}
	f.cur = 1
	return f.current[1], nil
}
func (f *fakeWeb) Rollback() (*webbundle.Release, error) {
	if f.cur == 0 {
		return nil, webbundle.ErrNoPrevious
	}
	f.cur = 0
	return f.current[0], nil
}

func webDir(t *testing.T, index string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func decodeWebVersion(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// LOOM-118: the web client says which bundle it runs and whether a newer
// one is out; an update swaps the bundle served at once, and a rollback
// swaps it back. All of it needs a session.
func TestWebUpdate_VersionUpdateRollback(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	baked := &webbundle.Release{Tag: "web-aaaaaaa", Short: "aaaaaaa", BuiltAt: t0}
	newer := &webbundle.Release{Tag: "web-bbbbbbb", Short: "bbbbbbb", BuiltAt: t0.Add(time.Hour)}
	web := &fakeWeb{dirs: [2]string{webDir(t, "baked"), webDir(t, "newer")},
		current: [2]*webbundle.Release{baked, newer}, latest: newer, enabled: true}
	httpSrv, _, _ := newTestServer(t, api.WithWebBundles(web))
	token, _ := login(t, httpSrv.URL, testPassword)

	page := func() string {
		resp, err := http.Get(httpSrv.URL + "/conversations/x")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	for _, path := range []string{"/api/v1/web/version"} {
		if resp := authedRequest(t, http.MethodGet, httpSrv.URL+path, "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s without a session = %d, want 401", path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/api/v1/web/update", "/api/v1/web/rollback"} {
		if resp := authedRequest(t, http.MethodPost, httpSrv.URL+path, "", nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("POST %s without a session = %d, want 401", path, resp.StatusCode)
		}
	}

	v := decodeWebVersion(t, authedRequest(t, http.MethodGet, httpSrv.URL+"/api/v1/web/version", token, nil))
	if v["update_available"] != true || v["updates_enabled"] != true || v["source"] != "image" || page() != "baked" {
		t.Fatalf("before update: %v, page %q", v, page())
	}

	resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/api/v1/web/update", token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update = %d", resp.StatusCode)
	}
	v = decodeWebVersion(t, resp)
	if v["source"] != "installed" || v["current"].(map[string]any)["tag"] != "web-bbbbbbb" || page() != "newer" {
		t.Fatalf("after update: %v, page %q", v, page())
	}
	if resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/api/v1/web/update", token, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("second update = %d, want 409", resp.StatusCode)
	}

	if resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/api/v1/web/rollback", token, nil); resp.StatusCode != http.StatusOK || page() != "baked" {
		t.Fatalf("rollback = %d, page %q", resp.StatusCode, page())
	}
	if resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/api/v1/web/rollback", token, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("second rollback = %d, want 409", resp.StatusCode)
	}
}

func TestWebUpdate_NotConfigured(t *testing.T) {
	web := &fakeWeb{dirs: [2]string{webDir(t, "baked"), ""}}
	httpSrv, _, _ := newTestServer(t, api.WithWebBundles(web))
	token, _ := login(t, httpSrv.URL, testPassword)

	v := decodeWebVersion(t, authedRequest(t, http.MethodGet, httpSrv.URL+"/api/v1/web/version", token, nil))
	if v["updates_enabled"] != false || v["update_available"] != false || v["current"] != nil {
		t.Errorf("version = %v", v)
	}
	if resp := authedRequest(t, http.MethodPost, httpSrv.URL+"/api/v1/web/update", token, nil); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("update = %d, want 503", resp.StatusCode)
	}
}
