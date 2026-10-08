package api

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Loomux/server/internal/health"
)

// API v1 contract snapshot: /api/v1 is a compatibility promise from
// 1.0.0 on (docs/release/v0.2.0.md, "What 1.0.0 will mean"): no route or
// field a v1 client relies on is removed or changes meaning within 1.x;
// additions only. TestAPIv1Contract holds the API to that mechanically:
// it derives the contract from the code — the routes from the
// mux.HandleFunc patterns in this package's source, the fields from the
// json tags of each route's body types — and compares it with
// testdata/api-v1-contract.txt. A line gone from the code is a breaking
// change; a new line is an addition that has to be recorded, so the
// golden file always says what the API is and every change to it shows
// up in review. See docs/release/versioning.md, "The API v1 contract".

var updateContract = flag.Bool("update", false, "rewrite testdata/api-v1-contract.txt from the code")

const contractGolden = "testdata/api-v1-contract.txt"

// contractBody is one JSON body a route reads or writes: role says which
// (request, response, a named response such as response:busy, or a
// stream's event:<name>), v is a value of its Go type. A nil v is a
// response with no body (204).
type contractBody struct {
	role string
	v    any
}

// mapLiteral stands for a body a handler writes as a map literal, not a
// struct, so reflection can't see its keys: they are read from the
// writeJSON call in the named function's source instead.
type mapLiteral struct{ fn string }

// apiV1Bodies is every /api/v1 route and its bodies. Every route
// registered in this package must be here (and nothing else), so a new
// route can't join the API without its bodies joining the contract.
// Error bodies are the same for every route: see contractShared.
var apiV1Bodies = map[string][]contractBody{
	"POST /api/v1/login":                       {{"request", loginRequest{}}, {"response", loginResponse{}}},
	"POST /api/v1/logout":                      {{"response", nil}},
	"GET /api/v1/sessions":                     {{"response", listSessionsResponse{}}},
	"DELETE /api/v1/sessions/{id}":             {{"response", nil}},
	"POST /api/v1/dispatch":                    {{"request", dispatchRequest{}}, {"response", dispatchResponse{}}, {"response:busy", busyResponse{}}},
	"GET /api/v1/dispatches/{id}":              {{"response", dispatchResponse{}}},
	"POST /api/v1/dispatches/{id}/cancel":      {{"response", cancelResponse{}}},
	"GET /api/v1/workspaces":                   {{"response", listWorkspacesResponse{}}},
	"DELETE /api/v1/workspaces/{id}":           {{"response", deleteWorkspaceResponse{}}},
	"PATCH /api/v1/workspaces/{id}":            {{"request", patchWorkspaceRequest{}}, {"response", nil}},
	"GET /api/v1/conversations":                {{"response", listConversationsResponse{}}},
	"GET /api/v1/conversations/{id}":           {{"response", getConversationResponse{}}},
	"GET /api/v1/conversations/{id}/events":    {{"response", listEventsResponse{}}},
	"GET /api/v1/conversations/{id}/stream":    {{"event:task_update", taskUpdateEvent{}}, {"event:dispatch_update", dispatchUpdateEvent{}}, {"event:message_added", messageAddedEvent{}}},
	"GET /api/v1/tasks/{id}/attach-info":       {{"response", attachInfoResponse{}}},
	"GET /api/v1/tasks/{id}/transcript":        {{"response", taskTranscriptResponse{}}},
	"POST /api/v1/tasks/{id}/cancel":           {{"response", cancelResponse{}}},
	"POST /api/v1/targets":                     {{"request", targetRequest{}}, {"response", targetResponse{}}},
	"GET /api/v1/targets":                      {{"response", listTargetsResponse{}}},
	"PUT /api/v1/targets/{id}":                 {{"request", targetRequest{}}, {"response", targetResponse{}}},
	"DELETE /api/v1/targets/{id}":              {{"response", nil}},
	"GET /api/v1/targets/{id}/agents":          {{"response", listTargetAgentsResponse{}}},
	"POST /api/v1/targets/{id}/probe":          {{"response", probeTargetResponse{}}},
	"POST /api/v1/targets/{id}/test":           {{"response", testTargetResponse{}}},
	"POST /api/v1/targets/{id}/scan-host-key":  {{"response", scanHostKeyResponse{}}},
	"POST /api/v1/targets/{id}/pin":            {{"request", pinHostKeyRequest{}}, {"response", targetResponse{}}},
	"DELETE /api/v1/targets/{id}/pin":          {{"response", targetResponse{}}},
	"GET /api/v1/credentials":                  {{"response", listCredentialsResponse{}}},
	"POST /api/v1/credentials":                 {{"request", createCredentialRequest{}}, {"response", credentialResponse{}}},
	"PUT /api/v1/credentials/{id}/value":       {{"request", setCredentialValueRequest{}}, {"response", nil}},
	"DELETE /api/v1/credentials/{id}":          {{"response", nil}},
	"GET /api/v1/ssh-keys":                     {{"response", listSSHKeysResponse{}}},
	"POST /api/v1/ssh-keys":                    {{"request", createSSHKeyRequest{}}, {"response", sshKeyResponse{}}},
	"DELETE /api/v1/ssh-keys/{id}":             {{"response", nil}},
	"GET /api/v1/settings/router":              {{"response", routerSettingsResponse{}}},
	"GET /api/v1/settings/router/audit":        {{"response", listRouterSettingsChangesResponse{}}},
	"PUT /api/v1/settings/router/{tier}":       {{"request", setRouterTierRequest{}}, {"response", routerTierResponse{}}},
	"DELETE /api/v1/settings/router/{tier}":    {{"response", nil}},
	"POST /api/v1/settings/router/{tier}/test": {{"response", testRouterTierResponse{}}},
	"POST /api/v1/targets/{id}/migrate-ssh":    {{"request", migrateSSHRequest{}}, {"response", migrateSSHResponse{}}},
	"GET /api/v1/health":                       {{"response", health.Result{}}},
	"GET /api/v1/health/deep":                  {{"response", health.Result{}}},
	"GET /api/v1/version":                      {{"response", versionResponse{}}},
	"GET /api/v1/web/version":                  {{"response", webVersionResponse{}}},
	"POST /api/v1/web/update":                  {{"response", webVersionResponse{}}},
	"POST /api/v1/web/rollback":                {{"response", webVersionResponse{}}},
}

// contractShared is the bodies that aren't one route's: the error every
// /api/v1 route answers with (writeError), and the rejection of an /api
// path outside /api/v1 (handleUnsupportedAPIPath).
var contractShared = map[string][]contractBody{
	"* /api/v1/*": {{"error", errorResponse{}}},
	"* /api/*":    {{"response:unsupported", unsupportedVersionResponse{}}},
}

func TestAPIv1Contract(t *testing.T) {
	fset, src := parseAPISource(t)

	routes := registeredV1Routes(src)
	if len(routes) == 0 {
		t.Fatal("found no mux.HandleFunc(\"METHOD /api/v1/...\") calls in package api's source")
	}
	for _, r := range routes {
		if _, ok := apiV1Bodies[r]; !ok {
			t.Errorf("route %q is registered but not in apiV1Bodies (api/contract_test.go): add it with its request/response types", r)
		}
	}
	for r := range apiV1Bodies {
		if !slices.Contains(routes, r) {
			t.Errorf("apiV1Bodies (api/contract_test.go) lists %q, which no mux.HandleFunc registers", r)
		}
	}

	w := &contractWriter{src: src, seen: map[string]bool{}}
	for route, bodies := range apiV1Bodies {
		w.route(t, route, bodies)
	}
	for scope, bodies := range contractShared {
		w.route(t, scope, bodies)
	}

	// Every body type declared here must reach the contract, so one the
	// registry forgot can't hide behind a registered route.
	for _, name := range bodyTypeNames(src) {
		if !w.seen[name] {
			t.Errorf("type %s looks like an API body but no route in apiV1Bodies (api/contract_test.go) uses it", name)
		}
	}
	for _, fn := range mapLiteralWriters(src) {
		if !w.mapFns[fn] {
			t.Errorf("%s writes a map literal as a response; add it to apiV1Bodies as mapLiteral{%q}", fn, fn)
		}
	}
	// The same for the stream's event names, which are string literals.
	written := streamEventNames(src)
	var registered []string
	for _, bodies := range apiV1Bodies {
		for _, b := range bodies {
			if name, ok := strings.CutPrefix(b.role, "event:"); ok {
				registered = append(registered, name)
			}
		}
	}
	for _, name := range written {
		if !slices.Contains(registered, name) {
			t.Errorf("the stream writes `event: %s` but apiV1Bodies (api/contract_test.go) has no event:%s body", name, name)
		}
	}
	for _, name := range registered {
		if !slices.Contains(written, name) {
			t.Errorf("apiV1Bodies (api/contract_test.go) lists event:%s, which nothing writes", name)
		}
	}

	// Each route's status codes, query parameters and headers
	// (api/contract_http_test.go).
	httpA := newHTTPAnalyser(fset, src)
	scopes := map[string][]*ast.FuncDecl{}
	for route, h := range routeHandlers(src) {
		scopes[route] = httpA.handlerRoots(h)
	}
	for scope, fn := range contractSharedHandlers {
		fd := httpA.funcs[fn]
		if fd == nil {
			t.Errorf("contractSharedHandlers (api/contract_http_test.go) names %s for %q, which isn't declared", fn, scope)
			continue
		}
		scopes[scope] = []*ast.FuncDecl{fd}
	}
	for scope, roots := range scopes {
		if len(roots) == 0 {
			t.Errorf("%s: found no package api function in its handler", scope)
			continue
		}
		facts, problems := httpA.routeFacts(roots)
		for _, p := range problems {
			t.Errorf("%s: %s", scope, p)
		}
		w.lines = append(w.lines, facts.lines(scope)...)
	}
	for fn := range httpUnanalysed {
		if len(httpA.problems[fn]) == 0 {
			t.Errorf("httpUnanalysed (api/contract_http_test.go) lists %s, which the walk now analyses: remove it", fn)
		}
	}

	if t.Failed() {
		return
	}

	current := w.lines
	slices.Sort(current)
	current = slices.Compact(current)

	if *updateContract {
		out := contractHeader + strings.Join(current, "\n") + "\n"
		if err := os.WriteFile(contractGolden, []byte(out), 0o644); err != nil {
			t.Fatalf("write %s: %v", contractGolden, err)
		}
		t.Logf("wrote %s (%d lines)", contractGolden, len(current))
		return
	}

	golden := readContractGolden(t)
	for _, l := range golden {
		if !slices.Contains(current, l) {
			t.Errorf("API v1 contract line removed or changed: %q\n"+
				"\tThis breaks API v1's additions-only promise (docs/release/v0.2.0.md): a v1 client may rely on it.\n"+
				"\tRestore it, or leave it for /api/v2. Before 1.0.0 only, a deliberate break is recorded with\n"+
				"\t`go test ./api -run TestAPIv1Contract -update` and called out in changes/.", l)
		}
	}
	for _, l := range current {
		if !slices.Contains(golden, l) {
			t.Errorf("API v1 contract line added: %q\n"+
				"\tRecord the addition with `go test ./api -run TestAPIv1Contract -update` and commit %s.",
				l, filepath.Join("api", contractGolden))
		}
	}
}

const contractHeader = `# API v1 contract: every /api/v1 route, the JSON fields of its bodies,
# the status codes it answers with, and the query parameters and headers
# it reads and sets.
# Generated by TestAPIv1Contract (api/contract_test.go); do not edit by hand.
# Regenerate with: go test ./api -run TestAPIv1Contract -update
# Lines may be added. Removing or changing one breaks API v1's
# additions-only promise (docs/release/versioning.md).
#
# Format: <METHOD path> <role> <JSON path> <kind>[ omitempty][ nullable]
# A route alone on its line is registered; "-" as the JSON path is the
# body itself. [] is an array's elements, {} a map's values.
# And: <METHOD path> status <code>
#      <METHOD path> query <name>
#      <METHOD path> request-header <Name>
#      <METHOD path> response-header <Name>
`

func readContractGolden(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(contractGolden)
	if err != nil {
		t.Fatalf("read %s: %v (create it with `go test ./api -run TestAPIv1Contract -update`)", contractGolden, err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		lines = append(lines, l)
	}
	return lines
}

// contractWriter renders bodies as contract lines.
type contractWriter struct {
	src   []*ast.File
	lines []string
	// seen is every package api type name reached; mapFns every function
	// whose map literal was read.
	seen   map[string]bool
	mapFns map[string]bool
}

func (w *contractWriter) route(t *testing.T, route string, bodies []contractBody) {
	t.Helper()
	w.lines = append(w.lines, route)
	for _, b := range bodies {
		prefix := route + " " + b.role + " "
		switch v := b.v.(type) {
		case nil:
			w.lines = append(w.lines, prefix+"- none")
		case mapLiteral:
			keys, kind := mapLiteralKeys(w.src, v.fn)
			if len(keys) == 0 {
				t.Errorf("%s: found no writeJSON(w, ..., map[...]...{...}) with string keys in %s", route, v.fn)
				continue
			}
			if w.mapFns == nil {
				w.mapFns = map[string]bool{}
			}
			w.mapFns[v.fn] = true
			w.lines = append(w.lines, prefix+"- object")
			for _, k := range keys {
				w.lines = append(w.lines, prefix+"."+k+" "+kind)
			}
		default:
			w.field(prefix, "", reflect.TypeOf(v), "", 0)
		}
	}
}

// field writes the line for one JSON value at path (the body itself
// when path is ""), then the lines for whatever it contains.
func (w *contractWriter) field(prefix, path string, typ reflect.Type, flags string, depth int) {
	if depth > 20 {
		panic("contract: type nesting too deep (a recursive type?) at " + prefix + path)
	}
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
		flags += " nullable"
	}
	shown := path
	if shown == "" {
		shown = "-"
	}
	w.lines = append(w.lines, prefix+shown+" "+jsonKind(typ)+flags)

	// Walk to what the value contains: an array's elements, a map's
	// values, an object's fields.
	inner, innerPath := typ, path
	for {
		for inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		switch inner.Kind() {
		case reflect.Slice, reflect.Array:
			inner, innerPath = inner.Elem(), innerPath+"[]"
			continue
		case reflect.Map:
			inner, innerPath = inner.Elem(), innerPath+"{}"
			continue
		}
		break
	}
	if inner.Kind() != reflect.Struct || inner == reflect.TypeOf(time.Time{}) {
		return
	}
	if inner.PkgPath() == reflect.TypeOf(contractBody{}).PkgPath() {
		w.seen[inner.Name()] = true
	}
	for _, f := range jsonFields(inner) {
		w.field(prefix, innerPath+"."+f.name, f.typ, f.flags, depth+1)
	}
}

type jsonField struct {
	name  string
	typ   reflect.Type
	flags string
}

// jsonFields is a struct's fields as encoding/json names them: exported
// fields only, json:"-" skipped, embedded structs flattened.
func jsonFields(typ reflect.Type) []jsonField {
	var out []jsonField
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			et := f.Type
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				out = append(out, jsonFields(et)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		var flags string
		if slices.Contains(strings.Split(opts, ","), "omitempty") {
			flags = " omitempty"
		}
		out = append(out, jsonField{name: name, typ: f.Type, flags: flags})
	}
	return out
}

// jsonKind is the JSON shape a Go type encodes as: what a client sees,
// so a change of Go type that keeps the shape (int to int64) isn't a
// contract change, and one that doesn't is.
func jsonKind(typ reflect.Type) string {
	if typ == reflect.TypeOf(time.Time{}) {
		return "time"
	}
	switch typ.Kind() {
	case reflect.Pointer:
		return jsonKind(typ.Elem())
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		if typ.Elem().Kind() == reflect.Uint8 {
			return "string" // []byte encodes as base64
		}
		return "[]" + jsonKind(typ.Elem())
	case reflect.Map:
		return "{}" + jsonKind(typ.Elem())
	case reflect.Struct:
		return "object"
	case reflect.Interface:
		return "any"
	}
	return typ.Kind().String()
}

// parseAPISource parses this package's non-test Go files.
func parseAPISource(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		files = append(files, f)
	}
	return fset, files
}

// registeredV1Routes is every "METHOD /api/v1/..." pattern passed to a
// HandleFunc or Handle call.
func registeredV1Routes(src []*ast.File) []string {
	var routes []string
	for _, f := range src {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			pattern, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			method, path, found := strings.Cut(pattern, " ")
			if !found {
				method, path = "*", pattern
			}
			if strings.HasPrefix(path, "/api/v1/") {
				routes = append(routes, method+" "+path)
			}
			return true
		})
	}
	return routes
}

// bodyTypeNames is every struct type declared in the package whose name
// says it is an API body.
func bodyTypeNames(src []*ast.File) []string {
	var names []string
	for _, f := range src {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
				return true
			}
			for _, suffix := range []string{"Request", "Response", "Event"} {
				if strings.HasSuffix(ts.Name.Name, suffix) {
					names = append(names, ts.Name.Name)
				}
			}
			return true
		})
	}
	return names
}

// streamEventNames is the name of every Server-Sent Event the package
// writes: each string literal starting "event: <name>\n".
func streamEventNames(src []*ast.File) []string {
	var names []string
	for _, f := range src {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			if rest, ok := strings.CutPrefix(s, "event: "); ok {
				name, _, _ := strings.Cut(rest, "\n")
				if !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
			return true
		})
	}
	return names
}

// writeJSONMapLiterals calls fn with the enclosing function's name and
// the map literal of every writeJSON(w, status, map...{...}) call.
func writeJSONMapLiterals(src []*ast.File, fn func(funcName string, lit *ast.CompositeLit)) {
	for _, f := range src {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) != 3 {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "writeJSON" {
					return true
				}
				lit, ok := call.Args[2].(*ast.CompositeLit)
				if !ok {
					return true
				}
				if _, isMap := lit.Type.(*ast.MapType); isMap {
					fn(fd.Name.Name, lit)
				}
				return true
			})
		}
	}
}

// mapLiteralWriters is every function that writes a map literal body.
func mapLiteralWriters(src []*ast.File) []string {
	var fns []string
	writeJSONMapLiterals(src, func(name string, _ *ast.CompositeLit) {
		if !slices.Contains(fns, name) {
			fns = append(fns, name)
		}
	})
	return fns
}

// mapLiteralKeys is the string keys of the map literals funcName writes,
// and their values' JSON kind (from the map type's value type).
func mapLiteralKeys(src []*ast.File, funcName string) (keys []string, kind string) {
	kind = "any"
	writeJSONMapLiterals(src, func(name string, lit *ast.CompositeLit) {
		if name != funcName {
			return
		}
		if id, ok := lit.Type.(*ast.MapType).Value.(*ast.Ident); ok {
			switch id.Name {
			case "string":
				kind = "string"
			case "bool":
				kind = "boolean"
			case "int", "int64", "float64":
				kind = "number"
			}
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if bl, ok := kv.Key.(*ast.BasicLit); ok && bl.Kind == token.STRING {
				if k, err := strconv.Unquote(bl.Value); err == nil && !slices.Contains(keys, k) {
					keys = append(keys, k)
				}
			}
		}
	})
	slices.Sort(keys)
	return keys, kind
}
