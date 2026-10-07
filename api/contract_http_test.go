package api

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"net/http"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
)

// The HTTP half of the API v1 contract (TestAPIv1Contract): per route,
// the status codes it can answer with, the query parameters and request
// headers it reads and the response headers it sets. Unlike bodies these
// have no types to reflect on, so they are read from the source: from the
// functions a route's mux.HandleFunc registers (the handler, and
// s.requireAuth around it), then every function in package api those
// reach, transitively. Anything the walk can't follow — a status that
// isn't a constant, a request handed to a function outside the package, a
// header named by a variable — fails the test rather than being skipped,
// so a fact can't leave the contract unnoticed.

// contractSharedHandlers is the function behind each contractShared
// scope that has one, so its HTTP facts are recorded with its body.
var contractSharedHandlers = map[string]string{
	"* /api/*": "Server.handleUnsupportedAPIPath",
}

// httpUnanalysed is the functions the walk can't analyse, each with the
// reason; their problems are tolerated, everything else they contain is
// still recorded. It is empty: every reachable function is analysed. An
// entry that no longer has a problem fails the test.
var httpUnanalysed = map[string]string{}

// hopByHopHeaders are connection-level headers (RFC 9110 §7.6.1): what
// the connection does, not what the API says, so setting or dropping one
// isn't a contract change.
var hopByHopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Transfer-Encoding", "Upgrade"}

// paramPassAllowed is the functions outside package api a handler may
// hand its ResponseWriter or *http.Request to: none of them sets a
// status or a header, or reads a query parameter or a request header.
var paramPassAllowed = []string{
	"http.NewResponseController",
	"http.MaxBytesReader",
	"json.NewEncoder",
	"fmt.Fprint",
	"fmt.Fprintf",
	"fmt.Fprintln",
	"io.WriteString",
}

// httpFacts is what one route (or one function) contributes.
type httpFacts struct {
	statuses, queries, reqHeaders, respHeaders map[string]bool
}

func newHTTPFacts() *httpFacts {
	return &httpFacts{map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}}
}

func (f *httpFacts) merge(o *httpFacts) {
	for _, pair := range [][2]map[string]bool{{f.statuses, o.statuses}, {f.queries, o.queries}, {f.reqHeaders, o.reqHeaders}, {f.respHeaders, o.respHeaders}} {
		for k := range pair[1] {
			pair[0][k] = true
		}
	}
}

// lines renders f as contract lines under route.
func (f *httpFacts) lines(route string) []string {
	var out []string
	for _, kind := range []struct {
		name string
		set  map[string]bool
	}{{"status", f.statuses}, {"query", f.queries}, {"request-header", f.reqHeaders}, {"response-header", f.respHeaders}} {
		for k := range kind.set {
			out = append(out, route+" "+kind.name+" "+k)
		}
	}
	slices.Sort(out)
	return out
}

// httpAnalyser walks package api's source.
type httpAnalyser struct {
	funcs   map[string]*ast.FuncDecl // by funcKey
	keys    map[*ast.FuncDecl]string
	methods map[string]map[string]*ast.FuncDecl // receiver type, name
	// forwards is, per function, the parameters it passes on as a
	// status (writeJSON's and writeError's status).
	forwards map[string][]int
	facts    map[string]*httpFacts
	problems map[string][]string
	statuses map[string]int // http.StatusX name to its code
}

// funcKey names a function: "readJSON", or "Server.handleDispatch".
func funcKey(fd *ast.FuncDecl) string {
	if t := recvType(fd); t != "" {
		return t + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func recvType(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	t := fd.Recv.List[0].Type
	if st, ok := t.(*ast.StarExpr); ok {
		t = st.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func recvName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 || len(fd.Recv.List[0].Names) == 0 {
		return ""
	}
	return fd.Recv.List[0].Names[0].Name
}

func newHTTPAnalyser(src []*ast.File) *httpAnalyser {
	a := &httpAnalyser{
		funcs:    map[string]*ast.FuncDecl{},
		keys:     map[*ast.FuncDecl]string{},
		methods:  map[string]map[string]*ast.FuncDecl{},
		forwards: map[string][]int{},
		facts:    map[string]*httpFacts{},
		problems: map[string][]string{},
		statuses: map[string]int{},
	}
	// The StatusX constant for each code, from its text: "Request Entity
	// Too Large" is StatusRequestEntityTooLarge. The few whose names
	// don't follow (StatusTeapot) aren't found, so using one fails the
	// test instead of mis-recording it.
	for code := 100; code < 600; code++ {
		if text := http.StatusText(code); text != "" {
			name := strings.Map(func(r rune) rune {
				if r == ' ' || r == '-' || r == '\'' {
					return -1
				}
				return r
			}, text)
			a.statuses["Status"+name] = code
		}
	}
	for _, f := range src {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			a.funcs[funcKey(fd)] = fd
			a.keys[fd] = funcKey(fd)
			if t := recvType(fd); t != "" {
				if a.methods[t] == nil {
					a.methods[t] = map[string]*ast.FuncDecl{}
				}
				a.methods[t][fd.Name.Name] = fd
			}
		}
	}
	a.findForwards()
	for _, fd := range a.funcs {
		a.analyse(fd)
	}
	return a
}

// callee is the package api function fun names inside fd: a package
// function, or a method called on fd's receiver.
func (a *httpAnalyser) callee(fd *ast.FuncDecl, fun ast.Expr) *ast.FuncDecl {
	switch f := fun.(type) {
	case *ast.Ident:
		if g := a.funcs[f.Name]; g != nil && recvType(g) == "" {
			return g
		}
	case *ast.SelectorExpr:
		if x, ok := f.X.(*ast.Ident); ok && fd.Recv != nil && x.Name == recvName(fd) {
			return a.methods[recvType(fd)][f.Sel.Name]
		}
	}
	return nil
}

// reached is every package api function fd refers to (calls, or passes
// on as a value).
func (a *httpAnalyser) reached(fd *ast.FuncDecl) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	var walk func(n ast.Node) bool
	walk = func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.SelectorExpr:
			if g := a.callee(fd, e); g != nil {
				out = append(out, g)
			}
			ast.Inspect(e.X, walk)
			return false
		case *ast.Ident:
			if g := a.callee(fd, e); g != nil {
				out = append(out, g)
			}
		}
		return true
	}
	ast.Inspect(fd.Body, walk)
	return out
}

// routeFacts is the facts of every function reachable from roots, and
// the problems found on the way.
func (a *httpAnalyser) routeFacts(roots []*ast.FuncDecl) (*httpFacts, []string) {
	facts := newHTTPFacts()
	var problems []string
	seen := map[*ast.FuncDecl]bool{}
	queue := slices.Clone(roots)
	for len(queue) > 0 {
		fd := queue[0]
		queue = queue[1:]
		if seen[fd] {
			continue
		}
		seen[fd] = true
		key := a.keys[fd]
		facts.merge(a.facts[key])
		if _, ok := httpUnanalysed[key]; !ok {
			problems = append(problems, a.problems[key]...)
		}
		queue = append(queue, a.reached(fd)...)
	}
	return facts, problems
}

// handlerRoots is the package api functions an HTTP handler expression
// (s.handleX, or s.requireAuth(s.handleX)) names.
func (a *httpAnalyser) handlerRoots(e ast.Expr) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if _, ok := x.X.(*ast.Ident); ok {
				if g := a.methods["Server"][x.Sel.Name]; g != nil {
					out = append(out, g)
				}
			}
			return false
		case *ast.Ident:
			if g := a.funcs[x.Name]; g != nil && recvType(g) == "" {
				out = append(out, g)
			}
		}
		return true
	})
	return out
}

// funcParams is fd's parameter names in order, and the names of the
// ResponseWriter, *http.Request and http.HandlerFunc parameters of fd
// and of every function literal inside it.
type funcParams struct {
	order               []string
	resp, req, handlers map[string]bool
}

func paramsOf(fd *ast.FuncDecl) funcParams {
	p := funcParams{resp: map[string]bool{}, req: map[string]bool{}, handlers: map[string]bool{}}
	add := func(fl *ast.FieldList, ordered bool) {
		if fl == nil {
			return
		}
		for _, field := range fl.List {
			typ := types.ExprString(field.Type)
			for _, n := range field.Names {
				if ordered {
					p.order = append(p.order, n.Name)
				}
				switch typ {
				case "http.ResponseWriter":
					p.resp[n.Name] = true
				case "*http.Request":
					p.req[n.Name] = true
				case "http.HandlerFunc", "http.Handler":
					p.handlers[n.Name] = true
				}
			}
			if len(field.Names) == 0 && ordered {
				p.order = append(p.order, "_")
			}
		}
	}
	add(fd.Type.Params, true)
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok {
			add(fl.Type.Params, false)
		}
		return true
	})
	return p
}

// statusArgs is the arguments of call that are written as a status:
// w.WriteHeader(x), http.Error(w, msg, x), or a package function that
// forwards a parameter as one (writeJSON(w, x, v)).
func (a *httpAnalyser) statusArgs(fd *ast.FuncDecl, call *ast.CallExpr) []ast.Expr {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if sel.Sel.Name == "WriteHeader" && len(call.Args) == 1 {
			return call.Args
		}
		if types.ExprString(sel) == "http.Error" && len(call.Args) == 3 {
			return call.Args[2:]
		}
	}
	var out []ast.Expr
	if g := a.callee(fd, call.Fun); g != nil {
		for _, i := range a.forwards[a.keys[g]] {
			if i < len(call.Args) {
				out = append(out, call.Args[i])
			}
		}
	}
	return out
}

// resolveStatus is the codes e can be inside fd: a StatusX constant, an
// integer literal, or a local variable only ever assigned those. param is
// the index of the parameter e is, when it is one (fd forwards it), or -1.
func (a *httpAnalyser) resolveStatus(fd *ast.FuncDecl, e ast.Expr) (codes []string, param int, problem string) {
	one := func(e ast.Expr) (string, string) {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "http" {
				if code, ok := a.statuses[x.Sel.Name]; ok {
					return strconv.Itoa(code), ""
				}
			}
		case *ast.BasicLit:
			if x.Kind == token.INT {
				return x.Value, ""
			}
		}
		return "", fmt.Sprintf("status %s isn't an http.StatusX constant or an integer", types.ExprString(e))
	}
	id, isIdent := e.(*ast.Ident)
	if !isIdent {
		c, p := one(e)
		if p != "" {
			return nil, -1, p
		}
		return []string{c}, -1, ""
	}
	if i := slices.Index(paramsOf(fd).order, id.Name); i >= 0 {
		return nil, i, ""
	}
	// A local: every value assigned to it.
	var found bool
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		var lhs []ast.Expr
		var rhs []ast.Expr
		switch s := n.(type) {
		case *ast.AssignStmt:
			lhs, rhs = s.Lhs, s.Rhs
		case *ast.ValueSpec:
			for _, name := range s.Names {
				lhs = append(lhs, name)
			}
			rhs = s.Values
		default:
			return true
		}
		for i, l := range lhs {
			if lid, ok := l.(*ast.Ident); ok && lid.Name == id.Name {
				found = true
				if len(rhs) != len(lhs) {
					problem = fmt.Sprintf("status %s is assigned from a multi-value expression", id.Name)
					continue
				}
				c, p := one(rhs[i])
				if p != "" {
					problem = p
					continue
				}
				if !slices.Contains(codes, c) {
					codes = append(codes, c)
				}
			}
		}
		return true
	})
	if !found {
		problem = fmt.Sprintf("status %s isn't a constant, a parameter or an assigned local", id.Name)
	}
	return codes, -1, problem
}

// findForwards finds, to a fixed point, which parameters each function
// passes on as a status.
func (a *httpAnalyser) findForwards() {
	for changed := true; changed; {
		changed = false
		for key, fd := range a.funcs {
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				for _, arg := range a.statusArgs(fd, call) {
					if _, i, _ := a.resolveStatus(fd, arg); i >= 0 && !slices.Contains(a.forwards[key], i) {
						a.forwards[key] = append(a.forwards[key], i)
						changed = true
					}
				}
				return true
			})
		}
	}
}

// literalArg is call's first argument when it is a string literal.
func literalArg(call *ast.CallExpr) (string, bool) {
	if len(call.Args) == 0 {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func literalIndex(ix *ast.IndexExpr) (string, bool) {
	lit, ok := ix.Index.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// lookup reads one key from what parent's chain (above node) does with
// node: node.Get("k")/node.Has("k")/node.Values("k") or node["k"].
func lookup(node ast.Node, parents []ast.Node, methods ...string) (string, bool) {
	if len(parents) == 0 {
		return "", false
	}
	switch p := parents[len(parents)-1].(type) {
	case *ast.IndexExpr:
		if p.X == node {
			return literalIndex(p)
		}
	case *ast.SelectorExpr:
		if len(parents) >= 2 && slices.Contains(methods, p.Sel.Name) {
			if call, ok := parents[len(parents)-2].(*ast.CallExpr); ok && call.Fun == p {
				return literalArg(call)
			}
		}
	}
	return "", false
}

// requestMethodHeaders is the *http.Request methods that read a header.
var requestMethodHeaders = map[string]string{
	"UserAgent": "User-Agent",
	"Referer":   "Referer",
	"Cookie":    "Cookie",
	"Cookies":   "Cookie",
	"BasicAuth": "Authorization",
}

// analyse records fd's own facts (not those of what it calls) and the
// problems that kept part of it from being read.
func (a *httpAnalyser) analyse(fd *ast.FuncDecl) {
	key := a.keys[fd]
	facts := newHTTPFacts()
	var problems []string
	problem := func(n ast.Node, format string, args ...any) {
		problems = append(problems, fmt.Sprintf("%s: %s (%s)", key, fmt.Sprintf(format, args...), types.ExprString(n.(ast.Expr))))
	}
	params := paramsOf(fd)
	isParam := func(e ast.Expr, set map[string]bool) bool {
		id, ok := e.(*ast.Ident)
		return ok && set[id.Name]
	}

	var stack []ast.Node
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		parents := stack
		stack = append(stack, n)

		switch e := n.(type) {
		case *ast.CallExpr:
			for _, arg := range a.statusArgs(fd, e) {
				codes, _, p := a.resolveStatus(fd, arg)
				if p != "" {
					problem(arg, "%s", p)
				}
				for _, c := range codes {
					facts.statuses[c] = true
				}
			}
			a.checkParamPass(fd, e, params, problem)

			sel, ok := e.Fun.(*ast.SelectorExpr)
			if !ok {
				break
			}
			// w.Header().Set("X", ...)
			if sel.Sel.Name == "Header" && isParam(sel.X, params.resp) {
				name, ok := lookup(e, parents, "Set", "Add", "Del", "Get", "Values")
				if !ok {
					problem(e, "response headers used in a way the contract can't read")
					break
				}
				p := parents[len(parents)-1]
				if ps, isSel := p.(*ast.SelectorExpr); isSel && (ps.Sel.Name != "Set" && ps.Sel.Name != "Add") {
					break // read or deleted, not set
				}
				name = textproto.CanonicalMIMEHeaderKey(name)
				if !slices.Contains(hopByHopHeaders, name) {
					facts.respHeaders[name] = true
				}
			}
			// r.FormValue("x"), r.URL.Query()...
			if isParam(sel.X, params.req) {
				switch sel.Sel.Name {
				case "FormValue", "PostFormValue":
					if name, ok := literalArg(e); ok {
						facts.queries[name] = true
					} else {
						problem(e, "form value named by a non-literal")
					}
				case "ParseForm", "ParseMultipartForm", "MultipartReader":
					problem(e, "form parsed in a way the contract can't read")
				}
				if h, ok := requestMethodHeaders[sel.Sel.Name]; ok {
					facts.reqHeaders[h] = true
				}
			}
			if sel.Sel.Name == "Query" {
				if u, ok := sel.X.(*ast.SelectorExpr); ok && u.Sel.Name == "URL" && isParam(u.X, params.req) {
					a.queryUses(fd, e, parents, facts, problem)
				}
			}
		case *ast.SelectorExpr:
			if !isParam(e.X, params.req) {
				break
			}
			switch e.Sel.Name {
			case "Header":
				name, ok := lookup(e, parents, "Get", "Values")
				if !ok {
					problem(e, "request headers used in a way the contract can't read")
					break
				}
				facts.reqHeaders[textproto.CanonicalMIMEHeaderKey(name)] = true
			case "Form", "PostForm", "MultipartForm", "Trailer":
				problem(e, "request field the contract can't read")
			}
		}
		// r.URL.RawQuery: the query read past the parser.
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "RawQuery" {
			if u, ok := sel.X.(*ast.SelectorExpr); ok && u.Sel.Name == "URL" && isParam(u.X, params.req) {
				problem(sel, "raw query read the contract can't follow")
			}
		}
		return true
	})
	a.facts[key] = facts
	a.problems[key] = problems
}

// queryUses records the keys read from the r.URL.Query() call q: read
// directly (r.URL.Query().Get("x")) or through a local (v := r.URL.Query()).
func (a *httpAnalyser) queryUses(fd *ast.FuncDecl, q *ast.CallExpr, parents []ast.Node, facts *httpFacts, problem func(ast.Node, string, ...any)) {
	if name, ok := lookup(q, parents, "Get", "Has"); ok {
		facts.queries[name] = true
		return
	}
	assign, ok := parents[len(parents)-1].(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		problem(q, "query used in a way the contract can't read")
		return
	}
	local, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		problem(q, "query used in a way the contract can't read")
		return
	}
	var stack []ast.Node
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		ps := stack
		stack = append(stack, n)
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != local.Name || id == local {
			return true
		}
		if name, ok := lookup(id, ps, "Get", "Has"); ok {
			facts.queries[name] = true
		} else {
			problem(id, "query used in a way the contract can't read")
		}
		return true
	})
}

// checkParamPass fails a call that hands fd's ResponseWriter or
// *http.Request to a function the walk can't follow: one outside package
// api, unless it's in paramPassAllowed or is the handler a wrapper was
// given (requireAuth's next, a root of the route itself).
func (a *httpAnalyser) checkParamPass(fd *ast.FuncDecl, call *ast.CallExpr, params funcParams, problem func(ast.Node, string, ...any)) {
	passes := false
	for _, arg := range call.Args {
		switch x := arg.(type) {
		case *ast.Ident:
			passes = passes || params.resp[x.Name] || params.req[x.Name]
		case *ast.CallExpr: // r.WithContext(ctx)
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithContext" {
				if id, ok := sel.X.(*ast.Ident); ok && params.req[id.Name] {
					passes = true
				}
			}
		}
	}
	if !passes || a.callee(fd, call.Fun) != nil {
		return
	}
	if id, ok := call.Fun.(*ast.Ident); ok && params.handlers[id.Name] {
		return
	}
	if slices.Contains(paramPassAllowed, types.ExprString(call.Fun)) {
		return
	}
	problem(call.Fun, "hands the response or request to a function outside package api; follow it or add it to paramPassAllowed")
}

// routeHandlers is the handler expression each "METHOD /api/v1/..."
// pattern is registered with.
func routeHandlers(src []*ast.File) map[string]ast.Expr {
	out := map[string]ast.Expr{}
	for _, f := range src {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
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
				out[method+" "+path] = call.Args[1]
			}
			return true
		})
	}
	return out
}
