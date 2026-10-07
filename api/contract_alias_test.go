package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestHTTPAnalyserAliases checks the contract walk (contract_http_test.go)
// fails a handler that copies its ResponseWriter or *http.Request, or
// r.URL / r.Header / w.Header(), where it can't follow them, and leaves
// the uses it does follow alone.
func TestHTTPAnalyserAliases(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		aliased    bool
	}{
		// Each of these hides a fact from the contract.
		{"assign w", `rw := w; rw.Header().Set("X-Alias2", "1")`, true},
		{"struct literal w", `sw := struct{ w http.ResponseWriter }{w}; sw.w.Header().Set("X-Leak", "1")`, true},
		{"keyed struct literal w", `sw := struct{ w http.ResponseWriter }{w: w}; _ = sw`, true},
		{"r.URL", `u := r.URL; u.Query().Get("leak")`, true},
		{"var r", `var rr = r; _ = rr`, true},
		{"address of w", `p := &w; _ = p`, true},
		{"channel send", `ch := make(chan *http.Request, 1); ch <- r`, true},
		{"return r", `func() *http.Request { return r }()`, true},
		{"r.WithContext kept", `r2 := r.WithContext(r.Context()); _ = r2`, true},
		{"method value", `h := w.Header; h().Set("X-Leak", "1")`, true},
		{"r.URL.Query method value", `q := r.URL.Query; q().Get("leak")`, true},
		{"to an any parameter", `keep(w)`, true},
		{"other assertion", `hw := w.(interface{ Header() http.Header }); _ = hw`, true},

		// What the walk follows.
		{"response header", `w.Header().Set("X-Ok", "1"); w.WriteHeader(http.StatusOK); w.Write(nil)`, false},
		{"request reads", `_ = r.Context(); _ = r.PathValue("id"); _ = r.Method; _ = r.URL.Path; _ = r.Header.Get("X-In"); _ = r.URL.Query().Get("q")`, false},
		{"query local", `q := r.URL.Query(); _ = q.Get("q")`, false},
		{"passed on", `writeJSON(w, http.StatusOK, nil); fmt.Fprint(w, "x"); _ = http.MaxBytesReader(w, r.Body, 1)`, false},
		{"flusher", `f, ok := w.(http.Flusher); _, _ = f, ok`, false},
		{"struct field named w", `x := struct{ w int }{w: 1}; _ = x.w`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := `package api

import (
	"fmt"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, v any) { w.WriteHeader(status); _ = v }

func keep(v any) { _ = v }

func handle(w http.ResponseWriter, r *http.Request) {
	` + tc.body + `
}

var _ = fmt.Sprint
`
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "handle.go", src, 0)
			if err != nil {
				t.Fatal(err)
			}
			a := newHTTPAnalyser(fset, []*ast.File{f})
			var aliased []string
			for _, p := range a.problems["handle"] {
				if strings.Contains(p, "request/response aliased") {
					aliased = append(aliased, p)
				}
			}
			if tc.aliased && len(aliased) == 0 {
				t.Errorf("no aliasing problem; problems: %q", a.problems["handle"])
			}
			if !tc.aliased && len(a.problems["handle"]) > 0 {
				t.Errorf("unexpected problems: %q", a.problems["handle"])
			}
			for _, p := range aliased {
				if !strings.HasPrefix(p, "handle: ") || !strings.Contains(p, "handle.go:") {
					t.Errorf("problem doesn't name the function and position: %q", p)
				}
			}
		})
	}
}
