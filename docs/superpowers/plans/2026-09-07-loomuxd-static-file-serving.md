# loomuxd Static-File-Serving (SPA Fallback) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `api.Server` an optional static-file-serving path so `loomuxd` can serve `loomux/web`'s built SPA directly, with SPA fallback to `index.html` for client-side routes.

**Architecture:** A new `WithStaticDir(dir string) Option` on `api.Server`. When set, any request whose path does not start with `/api/` is served from `dir`: a request that maps to a real file under `dir` gets that file (correct `Content-Type` via `http.FileServer`); anything else (an unknown path, a client-side route like `/conversations/abc123`, or `dir` itself) falls back to `dir/index.html`. When unset (the default — matches every test and deployment today), behavior is unchanged: those requests still 404. `/api/*` routing (including the existing unsupported-API-version rejection) is untouched either way — the static path is only ever reached for non-`/api/` requests.

**Tech Stack:** Go 1.27, `net/http` (`http.Dir`, `http.FileServer`, `http.ServeMux`), existing `api` package test conventions (`httptest`, table-style subtests).

**Spec:** `docs/design/web-client-design.md` ("Hosting / serving integration" section) — GitHub issue LOOM-33 (`gh issue view 32 --repo Loomux/server`) is the tracking ticket for this exact change; do not file a duplicate.

## Global Constraints

- Serve a configured static directory for any non-`/api/*` path (issue LOOM-33, design doc).
- SPA fallback to `index.html` for client-side routes — a deep-link refresh (e.g. `/conversations/abc123`) must return the SPA shell, not 404.
- Static serving must be strictly additive: unset by default, no change to any existing test or to `/api/*` routing/versioning behavior.
- Follow this package's existing patterns: functional `Option`s on `Server` (not new constructor params), env-var `Config` loading with fail-fast only on malformed (not merely absent) values, doc comments in this file's established voice, README updated to match (`api/README.md`'s `Layout`/`Design` sections document every option and env var already).

---

### Task 1: `api.Config` gains `StaticDir`

**Files:**
- Modify: `api/config.go`
- Test: `api/config_test.go`

**Interfaces:**
- Produces: `Config.StaticDir string` (empty by default — matches `app.Config.MarkerDir`'s "optional, no existence check at load time" convention, not `PasswordHash`'s fail-fast-if-malformed one, since an empty static dir is a legitimate "static serving disabled" state, not a malformed one).
- Produces: env var `LOOMUX_STATIC_DIR` (optional).

- [ ] **Step 1: Write the failing test**

Add to `api/config_test.go`:

```go
func TestLoadConfig_StaticDirDefaultsEmpty(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.StaticDir != "" {
		t.Errorf("StaticDir = %q, want empty (static serving disabled by default)", cfg.StaticDir)
	}
}

func TestLoadConfig_StaticDirFromEnv(t *testing.T) {
	t.Setenv("LOOMUX_AUTH_PASSWORD_HASH", validPasswordHashEnv(t))
	t.Setenv("LOOMUX_STATIC_DIR", "/srv/loomux/web")

	cfg, err := api.LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.StaticDir != "/srv/loomux/web" {
		t.Errorf("StaticDir = %q, want %q", cfg.StaticDir, "/srv/loomux/web")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./api/... -run TestLoadConfig_StaticDir -v`
Expected: FAIL — `cfg.StaticDir` doesn't exist yet (compile error: `Config` has no field `StaticDir`).

- [ ] **Step 3: Implement**

In `api/config.go`, add the env var constant and the `Config` field, and read it in `LoadConfig` (no validation — matches `app.Config.MarkerDir`, which is also an unvalidated optional path passed straight through to the layer that actually opens it):

```go
const (
	envPasswordHash = "LOOMUX_AUTH_PASSWORD_HASH"
	envAddr         = "LOOMUX_HTTP_ADDR"
	envSessionTTL   = "LOOMUX_SESSION_TTL"
	envStaticDir    = "LOOMUX_STATIC_DIR"

	defaultAddr = ":8080"
)
```

Add the field to `Config` (after `SessionTTL`):

```go
	// StaticDir, if set, is the directory containing the web client's
	// built static files (loomux/web's Vite build output) — passed to
	// WithStaticDir so Server serves it for any non-/api/* path, with SPA
	// fallback to index.html (design spec docs/design/web-client-design.md,
	// "Hosting / serving integration"; tracked as GitHub issue LOOM-33).
	// Empty (the default) disables static serving entirely — the same
	// "optional, unvalidated path" convention as app.Config.MarkerDir,
	// since there's no way to tell "not configured yet" apart from "wrong
	// path" without trying to open it, and that's Server's job, not
	// LoadConfig's.
	StaticDir string
```

Update `LoadConfig`'s return to include it:

```go
	return Config{PasswordHash: []byte(hash), Addr: addr, SessionTTL: ttl, StaticDir: os.Getenv(envStaticDir)}, nil
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./api/... -run TestLoadConfig -v`
Expected: PASS (all `TestLoadConfig_*` tests, including the two new ones and the pre-existing ones).

- [ ] **Step 5: Commit**

```bash
git add api/config.go api/config_test.go
git commit -m "feat(api): add LOOMUX_STATIC_DIR config for static-file serving"
```

---

### Task 2: `Server` static-file serving with SPA fallback

**Files:**
- Modify: `api/server.go`
- Test: `api/server_test.go`

**Interfaces:**
- Consumes: nothing new from Task 1 at compile time (this task's `Option` is independent of `Config`; `Config.StaticDir` is only wired to it in Task 3).
- Produces: `WithStaticDir(dir string) Option` — later tasks (Task 3, `cmd/loomuxd/main.go`) pass `apiCfg.StaticDir` to it.
- Produces: `Server.ServeHTTP` now serves static content for non-`/api/` paths when `WithStaticDir` was used; unchanged (404) otherwise.

- [ ] **Step 1: Write the failing tests**

Add to `api/server_test.go` (new imports needed: `os` and `path/filepath` — `path/filepath` is already imported):

```go
func TestStaticFileServing(t *testing.T) {
	dir := t.TempDir()
	writeFile := func(rel, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", rel, err)
		}
	}
	writeFile("index.html", "<html>spa shell</html>")
	writeFile("assets/app.js", "console.log('hi')")

	httpSrv, _, _ := newTestServer(t, api.WithStaticDir(dir))

	get := func(t *testing.T, path string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(httpSrv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp, string(body)
	}

	t.Run("serves a real static file as-is", func(t *testing.T) {
		resp, body := get(t, "/assets/app.js")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if body != "console.log('hi')" {
			t.Errorf("body = %q, want the raw asset content", body)
		}
	})

	t.Run("unknown client-side route falls back to index.html", func(t *testing.T) {
		resp, body := get(t, "/conversations/abc123")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (SPA fallback)", resp.StatusCode)
		}
		if body != "<html>spa shell</html>" {
			t.Errorf("body = %q, want index.html content (SPA fallback)", body)
		}
	})

	t.Run("root path serves index.html", func(t *testing.T) {
		resp, body := get(t, "/")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if body != "<html>spa shell</html>" {
			t.Errorf("body = %q, want index.html content", body)
		}
	})

	t.Run("api paths are unaffected by static serving", func(t *testing.T) {
		resp, body := get(t, "/api/v1/version")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if strings.Contains(body, "spa shell") {
			t.Errorf("body = %q, want the JSON version response, not static content", body)
		}
	})

	t.Run("unsupported api version is still rejected, not served as static", func(t *testing.T) {
		resp, body := get(t, "/api/v2/whatever")
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		if strings.Contains(body, "spa shell") {
			t.Errorf("body = %q, want the unsupported-version JSON response, not static content", body)
		}
	})
}

func TestStaticFileServing_DisabledByDefault(t *testing.T) {
	httpSrv, _, _ := newTestServer(t)

	resp, err := http.Get(httpSrv.URL + "/conversations/abc123")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (static serving not configured)", resp.StatusCode)
	}
}
```

`api/server_test.go` will need `"io"` and `"os"` added to its import block (`path/filepath`, `net/http`, and `strings` are already imported).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./api/... -run TestStaticFileServing -v`
Expected: FAIL — `api.WithStaticDir` doesn't exist yet (compile error).

- [ ] **Step 3: Implement**

In `api/server.go`, add a `static http.HandlerFunc` field to `Server` and a `staticDir` field to hold the option's value until `NewServer` builds the handler:

```go
type Server struct {
	dispatcher         Dispatcher
	sessions           SessionStore
	workspaces         WorkspaceLister
	tasks              TaskLister
	messages           MessageLister
	attachInfo         AttachInfoStore
	passwordHash       []byte
	sessionTTL         time.Duration
	loginThrottle      *loginThrottle
	streamPollInterval time.Duration
	staticDir          string
	static             http.HandlerFunc
	mux                *http.ServeMux
}
```

Add the option (near the other `With*` options, after `WithStreamPollInterval`):

```go
// WithStaticDir configures Server to serve a built single-page-app from
// dir for any request whose path does not start with /api/ — the web
// client's Vite build output (design spec docs/design/web-client-design.md,
// "Hosting / serving integration"; GitHub issue LOOM-33). A request that
// resolves to a real file under dir is served as-is (correct Content-Type
// via http.FileServer); anything else — an unknown path, or a client-side
// route like /conversations/abc123 that only exists in the SPA's own
// router — falls back to dir/index.html, so a hard refresh on a deep link
// gets the SPA shell instead of a 404. Unset (the default): every such
// request still 404s, unchanged from before this option existed.
func WithStaticDir(dir string) Option {
	return func(s *Server) { s.staticDir = dir }
}
```

In `NewServer`, after the options loop and before building `mux` (or after — order doesn't matter since they're independent fields), build the static handler if configured:

```go
	for _, opt := range opts {
		opt(s)
	}
	if s.staticDir != "" {
		s.static = newStaticHandler(s.staticDir)
	}
```

Replace `ServeHTTP` with a version that routes non-`/api/` paths to the static handler:

```go
// ServeHTTP implements http.Handler. Any /api/... path outside /api/v1/
// (a different or unsupported version, or the bare /api/ root) is
// rejected here, before reaching the mux — design spec §10 axis 1: "a
// mismatch is a clear rejection ... not silent breakage," structured
// rather than a bare 404. This check is a simple path-prefix test done
// ahead of routing, not a registered ServeMux pattern, specifically so
// it can never shadow a real /api/v1/... route hit with the wrong HTTP
// method (ServeMux's own 405 for that case is the correct, distinct
// response — a known path used incorrectly, not an unknown version).
//
// Every other path (anything not starting with /api/) goes to the static
// handler when WithStaticDir was used (LOOM-33) — never to mux, so a
// static build can never shadow or be shadowed by an /api/v1/ route.
// Without WithStaticDir, those paths 404 via http.NotFound, matching this
// package's behavior before this option existed.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			s.handleUnsupportedAPIPath(w, r)
			return
		}
		s.mux.ServeHTTP(w, r)
		return
	}
	if s.static != nil {
		s.static(w, r)
		return
	}
	http.NotFound(w, r)
}
```

Add the handler constructor near the bottom of the file, above `sessionContextKey`/`requireAuth` (or anywhere at package scope — placed here to group it with the other top-level helpers like `writeJSON`):

> **Note (post-implementation correction):** the block below is what actually
> shipped, not the first draft this plan was written against. The first draft
> rewrote the fallback request's path to `/index.html` and re-delegated to
> `fileServer.ServeHTTP` — that hits a built-in special case in net/http's
> `serveFile`, which 301-redirects any request whose path ends in
> `/index.html` to `./` (URL canonicalization, so `/foo/index.html` and
> `/foo/` aren't two live URLs for the same page). Since the rewritten path
> literally *is* `/index.html` every time, that redirected every fallback
> request in an infinite loop. Fixed by serving `index.html` directly via
> `http.ServeContent` instead of routing back through `fileServer`.

```go
// newStaticHandler serves a built SPA from dir: a request that maps to a
// real file under dir is served as-is via http.FileServer (correct
// Content-Type from the file extension); anything else — no such file,
// or the path is a directory (including the root "/") — falls back to
// dir/index.html, so a browser refresh on a client-side route (e.g.
// /conversations/abc123, React Router) gets the SPA shell instead of a
// 404 (design spec web-client-design.md, "Hosting / serving
// integration"). Uses http.Dir.Open (not a raw os.Stat) specifically
// because it already rejects ".." path elements — a traversal attempt
// falls into the same "no such file" branch as any other unknown path
// and gets the SPA shell, never an out-of-dir file.
//
// The fallback serves index.html via http.ServeContent directly rather
// than delegating to fileServer with a rewritten request path: net/http's
// own serveFile has a built-in special case that 301-redirects any
// request whose path ends in "/index.html" to "./" (URL canonicalization,
// so /foo/index.html and /foo/ aren't two live URLs for the same page) —
// rewriting every fallback request's path to literally "/index.html" and
// handing it back to fileServer would hit that special case on every
// single fallback and redirect-loop forever.
func newStaticHandler(dir string) http.HandlerFunc {
	root := http.Dir(dir)
	fileServer := http.FileServer(root)
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		f, err := root.Open("/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "index.html", info.ModTime(), f)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		f, err := root.Open(r.URL.Path)
		if err != nil {
			serveIndex(w, r)
			return
		}
		info, statErr := f.Stat()
		f.Close()
		if statErr != nil || info.IsDir() {
			serveIndex(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./api/... -v -race`
Expected: PASS — every test in the package, including the new `TestStaticFileServing*` tests and every pre-existing test (auth, dispatch, workspaces, conversations, stream, attach-info, throttle, config). Run with `-race` per this package's own testing convention (`api/README.md`'s Testing section — the stream handler's poll loop already requires it; this change doesn't add concurrency, but keep the same invocation to catch anything unexpected).

- [ ] **Step 5: Commit**

```bash
git add api/server.go api/server_test.go
git commit -m "feat(api): serve static SPA build with index.html fallback (LOOM-33)"
```

---

### Task 3: Wire into `cmd/loomuxd` and document in `api/README.md`

**Files:**
- Modify: `cmd/loomuxd/main.go`
- Modify: `api/README.md`

**Interfaces:**
- Consumes: `api.Config.StaticDir` (Task 1), `api.WithStaticDir` (Task 2).

- [ ] **Step 1: Wire the option in `runServer`**

In `cmd/loomuxd/main.go`, change `runServer` to pass `WithStaticDir` only when configured (mirrors the file's existing style of building an options slice conditionally — see `main`'s own `if cfg.MasterKey == nil` warning-but-continue pattern for "optional, don't force a value"):

```go
func runServer(ctx context.Context, loomux *app.App) {
	apiCfg, err := api.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	opts := []api.Option{api.WithSessionTTL(apiCfg.SessionTTL)}
	if apiCfg.StaticDir != "" {
		opts = append(opts, api.WithStaticDir(apiCfg.StaticDir))
	}

	server := api.NewServer(loomux, loomux.Store(), loomux.Store(), loomux.Store(), loomux.Store(), loomux.Store(), apiCfg.PasswordHash, opts...)
	httpServer := &http.Server{Addr: apiCfg.Addr, Handler: server}
```

(The rest of `runServer` — the shutdown goroutine and `ListenAndServe` call — is unchanged.)

- [ ] **Step 2: Build to confirm it compiles**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 3: Document in `api/README.md`**

Add a new bullet to the `## Design` section, after the "No idle-session reaper" bullet:

```markdown
- **Static SPA serving is opt-in and lives outside the API surface
  entirely** (LOOM-33). `WithStaticDir`/`LOOMUX_STATIC_DIR` (unset by
  default) points `Server` at a built web-client directory
  (`loomux/web`'s Vite output); any request whose path doesn't start
  with `/api/` is served from there, with fallback to `index.html` for
  anything that isn't a real file — a browser refresh on a client-side
  route like `/conversations/abc123` gets the SPA shell instead of a
  404. Routed entirely in `ServeHTTP` ahead of `mux`, mirroring the
  existing `/api/` version check already there — `/api/*` paths never
  reach the static handler and the static handler never reaches `mux`,
  so neither can shadow the other. Not built as part of LOOM-23/LOOM-24
  (the web client itself); this is the small server-side hosting
  addition their design flagged as a dependency.
```

Update the `config.go` bullet in `## Layout` to mention the new env var:

```markdown
- `config.go` — `Config`, `LoadConfig()`: `LOOMUX_AUTH_PASSWORD_HASH`
  (required, validated as a real bcrypt hash), `LOOMUX_HTTP_ADDR`
  (optional, default `:8080`), `LOOMUX_SESSION_TTL` (optional, default 30
  days), `LOOMUX_STATIC_DIR` (optional, default unset — static serving
  disabled).
```

Update the `server.go` bullet in `## Layout` to mention `WithStaticDir` alongside the other options it already lists (find the sentence introducing `NewServer`/the handlers and add a trailing mention — exact surrounding text will be visible when editing, match its existing voice):

```markdown
  ... `NewServer`, ... the `requireAuth` middleware, `APIVersion`,
  `WithStaticDir` (opt-in static SPA serving with `index.html` fallback,
  LOOM-33 — see Design above), and the handlers:
```

- [ ] **Step 4: Commit**

```bash
git add cmd/loomuxd/main.go api/README.md
git commit -m "feat(loomuxd): wire LOOMUX_STATIC_DIR into the running server"
```

---

### Task 4: End-to-end manual verification

**Files:** none (verification only — no code changes).

- [ ] **Step 1: Build the binary**

Run: `go build -o /tmp/loomuxd ./cmd/loomuxd`
Expected: no errors.

- [ ] **Step 2: Create a throwaway static build to serve**

```bash
mkdir -p /tmp/loomux-web-fixture/assets
echo '<html><body>spa shell</body></html>' > /tmp/loomux-web-fixture/index.html
echo "console.log('asset')" > /tmp/loomux-web-fixture/assets/app.js
```

- [ ] **Step 3: Start the server against a throwaway DB with static serving enabled**

```bash
PASS_HASH=$(echo -n 'e2e-test-password' | /tmp/loomuxd -hash-password)
LOOMUX_AUTH_PASSWORD_HASH="$PASS_HASH" \
LOOMUX_DB_PATH=/tmp/loomux-e2e.db \
LOOMUX_HTTP_ADDR=127.0.0.1:18080 \
LOOMUX_STATIC_DIR=/tmp/loomux-web-fixture \
LOOMUX_ROUTER_PRIMARY_BASE_URL=http://127.0.0.1:1 \
LOOMUX_ROUTER_PRIMARY_API_KEY=unused \
LOOMUX_ROUTER_PRIMARY_MODEL=unused \
/tmp/loomuxd &
sleep 1
```

(The router env vars are dummy values — `app.Build` requires `llmrouter.ConfigFromEnv` to succeed at startup, but nothing in this verification calls `/dispatch`, so they're never actually used.)

- [ ] **Step 4: Verify a real static asset is served correctly**

Run: `curl -s -o /dev/stdout -w '\nSTATUS:%{http_code} CONTENT-TYPE:%{content_type}\n' http://127.0.0.1:18080/assets/app.js`
Expected: body is `console.log('asset')`, `STATUS:200`, `CONTENT-TYPE` includes `javascript` or `text/plain` (whatever `http.FileServer` infers from `.js` — either is fine, just confirm it's not `text/html`).

- [ ] **Step 5: Verify SPA fallback on a deep client-side route (the case the task called out as easy to get wrong)**

Run: `curl -s -o /dev/stdout -w '\nSTATUS:%{http_code}\n' http://127.0.0.1:18080/conversations/some-fake-id`
Expected: body is `<html><body>spa shell</body></html>`, `STATUS:200` — a deep-link refresh does NOT 404.

- [ ] **Step 6: Verify the root path also serves the SPA shell**

Run: `curl -s -o /dev/stdout -w '\nSTATUS:%{http_code}\n' http://127.0.0.1:18080/`
Expected: body is `<html><body>spa shell</body></html>`, `STATUS:200`.

- [ ] **Step 7: Verify `/api/*` is completely unaffected**

Run: `curl -s http://127.0.0.1:18080/api/v1/version` — expected: real JSON `{"server_version":"...","api_version":"v1"}`, not the SPA shell.

Run: `curl -s -o /dev/stdout -w '\nSTATUS:%{http_code}\n' http://127.0.0.1:18080/api/v2/anything` — expected: `STATUS:404` with the structured `unsupported_versions` JSON body (from `handleUnsupportedAPIPath`), not the SPA shell.

- [ ] **Step 8: Stop the server and clean up**

```bash
kill %1
rm -rf /tmp/loomux-web-fixture /tmp/loomux-e2e.db /tmp/loomuxd
```

- [ ] **Step 9: Run the full test suite one more time**

Run: `go test ./... -race`
Expected: PASS across the whole module — confirms this change didn't regress anything outside `api/` either (e.g. `cmd/loomuxd` still builds/wires correctly).

---

## Post-implementation (not plan tasks — do after Task 4 passes)

- Comment on and close GitHub issue LOOM-33 (`gh issue comment 32 --repo Loomux/server` / `gh issue close 32 --repo Loomux/server`), per this repo's standing convention (CLAUDE.md) — summarize what changed, which commits, and the verification evidence from Task 4.
- Send an `agent-mailbox` `handoff` to `command-center` (per this repo's CLAUDE.md cross-workspace boundary rule) — do NOT touch Vikunja/Plane directly. Note in the handoff that GitHub issue LOOM-33 already references Vikunja Loomux #767 / Plane LOOM-33 in its body; ask command-center to verify/reconcile those against its own records rather than assuming they're already accurate, since this session did not create or check them.

## Post-merge correction (from independent review)

An independent review of this PR (a different model, run against the merged
diff) found a real bug in `ServeHTTP`'s routing check that this plan's Task 2
missed: it tested `strings.HasPrefix(r.URL.Path, "/api/")` (trailing slash
required), so the exact path `/api` (no trailing slash) didn't match and fell
through to the static handler — returning the SPA shell (200) instead of the
same unsupported-API-path rejection `/api/v2/...` already got. Fixed by also
matching the bare `/api` path explicitly: `r.URL.Path == "/api" ||
strings.HasPrefix(r.URL.Path, "/api/")`. Two tests were added alongside the
fix: a regression test for this exact case, and a path-traversal coverage
test (plain `..` and its `%2e%2e` percent-encoded form) confirming
`http.Dir.Open`'s existing dot-dot rejection already prevented escaping the
static directory — that part needed a test locking it in, not a fix.
