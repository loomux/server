package api

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInlineScriptHashes(t *testing.T) {
	snippet := "\n      try { document.documentElement.dataset.x = 1 } catch (e) {}\n    "
	sum := sha256.Sum256([]byte(snippet))
	want := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
	html := `<html><head><SCRIPT>` + snippet + `</SCRIPT>
<script type="module" crossorigin src="/assets/index.js"></script></head></html>`
	got := inlineScriptHashes([]byte(html))
	if len(got) != 1 || got[0] != want {
		t.Fatalf("inlineScriptHashes = %v, want [%s] (the inline one only)", got, want)
	}
	crlf := inlineScriptHashes([]byte("<script>" + strings.ReplaceAll(snippet, "\n", "\r\n") + "</script>"))
	if len(crlf) != 1 || crlf[0] != want {
		t.Errorf("CRLF script hashed as %v, want %s (browsers hash it with LF)", crlf, want)
	}
	// A character whose lowercase is longer must not shift the offsets.
	if got := inlineScriptHashes([]byte("<title>İİİİ</title><script>" + snippet + "</script>")); len(got) != 1 || got[0] != want {
		t.Errorf("after non-ASCII text: %v, want [%s]", got, want)
	}
	if got := inlineScriptHashes([]byte(`<script>unterminated`)); len(got) != 0 {
		t.Errorf("unterminated script hashed: %v", got)
	}
}

// LOOM-143: every response carries the hardening headers; the app shell
// gets a CSP allowing its own inline script by hash, the API a deny-all
// one and no-store; HSTS only over HTTPS (here, via the proxy).
func TestSecurityHeaders(t *testing.T) {
	dir := t.TempDir()
	inline := "var t = 1;"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<script>"+inline+"</script>"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewServer(nil, nil, nil, nil, nil, nil, nil, nil, WithStaticDir(dir))

	get := func(path string, hdr map[string]string) http.Header {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Header()
	}

	for _, path := range []string{"/", "/today", "/api/v1/health", "/api/v2/x"} {
		h := get(path, nil)
		for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
			if h.Get(k) != v {
				t.Errorf("%s: %s = %q, want %q", path, k, h.Get(k), v)
			}
		}
		if h.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS over plain HTTP", path)
		}
		if !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q lacks frame-ancestors 'none'", path, h.Get("Content-Security-Policy"))
		}
	}

	sum := sha256.Sum256([]byte(inline))
	app := get("/", nil).Get("Content-Security-Policy")
	if !strings.Contains(app, "script-src 'self' 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Errorf("app CSP %q doesn't allow index.html's inline script by hash", app)
	}
	for _, d := range strings.Split(app, ";") {
		if strings.HasPrefix(strings.TrimSpace(d), "script-src") && strings.Contains(d, "unsafe-inline") {
			t.Errorf("app CSP %q allows inline script wholesale", app)
		}
	}

	api := get("/api/v1/health", nil)
	if api.Get("Content-Security-Policy") != apiCSP || api.Get("Cache-Control") != "no-store" {
		t.Errorf("API: CSP %q, Cache-Control %q; want %q, no-store", api.Get("Content-Security-Policy"), api.Get("Cache-Control"), apiCSP)
	}

	if got := get("/", map[string]string{"X-Forwarded-Proto": "https"}).Get("Strict-Transport-Security"); got == "" {
		t.Error("no HSTS behind an HTTPS proxy")
	}
}
