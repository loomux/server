package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// Browser hardening (LOOM-143). The web client keeps its bearer token in
// localStorage, so the page must not run script it didn't ship, talk to
// other origins, or be framed by another site.

// setCommonHeaders applies to every response, API and static alike.
func setCommonHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	// HSTS only means anything over HTTPS; behind the reverse proxy the
	// request reaches loomuxd as plain HTTP, so trust the proxy's
	// X-Forwarded-Proto for this one header. Spoofing it is harmless:
	// browsers ignore HSTS received over plain HTTP (RFC 6797).
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

// apiCSP is for JSON and SSE responses: nothing in them is ever rendered.
const apiCSP = "default-src 'none'; frame-ancestors 'none'"

// appCSP is the app shell's policy. scriptHashes allows index.html's own
// inline scripts (the pre-paint theme snippet), hashed from the file
// actually served, since a web update can change it without a server
// release. Inline styles stay allowed: React and react-aria set them,
// and they can't run code.
func appCSP(scriptHashes []string) string {
	script := "'self'"
	for _, h := range scriptHashes {
		script += " '" + h + "'"
	}
	return "default-src 'self'; script-src " + script +
		"; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:" +
		"; connect-src 'self'; manifest-src 'self'; worker-src 'self'" +
		"; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}

// inlineScriptHashes returns the CSP hash source of every inline
// <script> in html (one without src).
func inlineScriptHashes(html []byte) []string {
	var hashes []string
	// ASCII-only lowercasing keeps byte offsets equal to html's
	// (bytes.ToLower can change a non-ASCII character's length).
	lower := make([]byte, len(html))
	for i, c := range html {
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	for i := 0; ; {
		start := bytes.Index(lower[i:], []byte("<script"))
		if start < 0 {
			return hashes
		}
		start += i
		tagEnd := bytes.IndexByte(lower[start:], '>')
		if tagEnd < 0 {
			return hashes
		}
		tagEnd += start
		end := bytes.Index(lower[tagEnd:], []byte("</script"))
		if end < 0 {
			return hashes
		}
		end += tagEnd
		if !bytes.Contains(lower[start:tagEnd], []byte("src=")) {
			// Browsers hash the script text after the HTML parser has
			// turned CRLF and CR into LF.
			text := bytes.ReplaceAll(html[tagEnd+1:end], []byte("\r\n"), []byte("\n"))
			text = bytes.ReplaceAll(text, []byte("\r"), []byte("\n"))
			sum := sha256.Sum256(text)
			hashes = append(hashes, "sha256-"+base64.StdEncoding.EncodeToString(sum[:]))
		}
		i = end
	}
}
