// Package apidoc builds the HTTP API reference by reading the router source.
//
// Every fact a reference needs is already stated exactly once in the code: the
// path, the methods and the middleware chain in routes.go, the capability in
// the RequireCap call that gates the route, and the prose in each handler's doc
// comment. Restating 460+ routes in Markdown by hand would create a second
// source of truth that goes stale on the first endpoint anyone adds, so this
// package reads the first one instead. TestAPIDocIsCurrent fails the build when
// the checked-in file stops matching what the router actually registers.
//
// The practical consequence: to improve a route's description, improve the
// handler's doc comment and regenerate. There is nowhere else to write it.
package apidoc

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"dylaris-core/authz"
)

// Route is one registered endpoint, described the way the router sees it.
type Route struct {
	Methods []string // "GET", "POST", ...; empty when the registration sets none
	Path    string   // full template, including the subrouter's prefix
	Router  string   // the router variable it was registered on
	Auth    string   // the credential the route accepts, "" when it takes none
	Cap     string   // capability id from RequireCap (inline or bound to a variable), "" when the route declares none
	// Caps is every capability the chain applies, including one bound to a
	// variable first (pbCap := ...RequireCap("x")). Cap keeps the rendered
	// value; the per-method coverage pin reads this.
	Caps    []string
	Authz   string   // why it declares none: "in-handler", "exempt", "uncapped method"
	Gates   []string // the remaining middleware in the chain, outermost first
	Handler string   // "PacksHandler.List", or "" for an inline func literal
	Doc     string   // the handler's doc comment, first sentence, prefix stripped
}

// Parse reads routesFile and returns every route buildAPIRouter registers, in
// source order. handlerDirs are searched for the doc comments that become each
// route's description.
func Parse(routesFile string, handlerDirs []string) ([]Route, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, routesFile, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", routesFile, err)
	}
	docs, err := handlerDocs(handlerDirs)
	if err != nil {
		return nil, err
	}

	fn := findFunc(file, "buildAPIRouter")
	if fn == nil {
		return nil, fmt.Errorf("%s: no buildAPIRouter function", routesFile)
	}
	required := requiredCapTemplates(file)

	// appState and authHandler arrive as parameters rather than as a
	// handlers.NewX() assignment, so seed them; everything else is learned.
	varTypes := map[string]string{
		"appState":    "AppState",
		"authHandler": "AuthHandler",
	}
	prefixes := map[string]string{}

	var routes []Route
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			learnAssignment(s, varTypes, prefixes)
		case *ast.ExprStmt:
			r, ok := parseRegistration(s.X, varTypes, prefixes)
			if !ok {
				return true
			}
			r.Doc = docs[r.Handler]
			// A route with no capability is not simply "ungated", and the
			// differences matter to whoever reads the table. The first two maps
			// are the ones the strict coverage test enforces, so they cannot
			// drift from what is actually gated.
			//
			// The last case is the one worth naming: requiredCaps is keyed by
			// path TEMPLATE, so a template is satisfied as soon as ONE of its
			// methods carries the capability. Five GETs sit beside a capped
			// POST that way, deliberately (see the comments in requiredCaps),
			// and coverage cannot show it because coverage does not see methods.
			switch {
			case r.Cap != "":
			case authz.InHandlerAuthzRoutes[r.Path]:
				r.Authz = "in-handler"
			case authz.ExemptRoutes[r.Path]:
				// ExemptRoutes holds two different things, and calling both
				// "exempt" would tell a reader that anyone at all can read
				// another user's billing or 2FA status. Most of that map is
				// AUTHED-EXEMPT: a credential IS required, no capability
				// applies, and the handler scopes the answer to the caller.
				//
				// The split is drawn on the credential rather than on the map's
				// own section headings, because those file the warp and
				// API-key routes under "PUBLIC (no session auth)" - true as
				// written, and not what a reader would take from it.
				if r.Auth == "" {
					r.Authz = "public"
				} else {
					r.Authz = "no capability"
				}
			case required[r.Path] != "":
				r.Authz = "uncapped method"
			}
			routes = append(routes, r)
		}
		return true
	})
	if len(routes) == 0 {
		return nil, fmt.Errorf("%s: buildAPIRouter registered no routes, the parser is out of step with the source", routesFile)
	}
	return routes, nil
}

// requiredCapTemplates reads the requiredCaps literal out of the same file, so
// the reference can tell "this method declares no capability, but its template
// does" apart from "nothing gates this at all".
func requiredCapTemplates(file *ast.File) map[string]string {
	out := map[string]string{}
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, isValue := spec.(*ast.ValueSpec)
			if !isValue || len(vs.Names) == 0 || vs.Names[0].Name != "requiredCaps" {
				continue
			}
			for _, v := range vs.Values {
				lit, isLit := v.(*ast.CompositeLit)
				if !isLit {
					continue
				}
				for _, e := range lit.Elts {
					kv, isKV := e.(*ast.KeyValueExpr)
					if !isKV {
						continue
					}
					k, kOK := strLit(kv.Key)
					c, cOK := strLit(kv.Value)
					if kOK && cOK {
						out[k] = c
					}
				}
			}
		}
	}
	return out
}

// capVarPrefix marks a varTypes entry as a RequireCap wrapper rather than a
// handler type. A handler type never contains a colon.
const capVarPrefix = "cap:"

func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
			return fn
		}
	}
	return nil
}

// --- assignments ---

// learnAssignment records what a local variable is, so a later registration can
// be attributed to a handler type and a subrouter prefix.
func learnAssignment(s *ast.AssignStmt, varTypes, prefixes map[string]string) {
	if len(s.Lhs) == 0 || len(s.Rhs) != 1 {
		return
	}
	name, ok := s.Lhs[0].(*ast.Ident)
	if !ok {
		return
	}
	// x := handlers.NewFooHandler(...) makes x a *FooHandler.
	if call, isCall := s.Rhs[0].(*ast.CallExpr); isCall {
		if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && strings.HasPrefix(sel.Sel.Name, "New") {
			varTypes[name.Name] = strings.TrimPrefix(sel.Sel.Name, "New")
		}
		// x := appState.Authz.RequireCap("c") makes x a capability wrapper.
		if sel, isSel := call.Fun.(*ast.SelectorExpr); isSel && sel.Sel.Name == "RequireCap" && len(call.Args) == 1 {
			if c, isStr := strLit(call.Args[0]); isStr {
				varTypes[name.Name] = capVarPrefix + c
			}
		}
	}
	if base, prefix, isRouter := routerChain(s.Rhs[0]); isRouter {
		prefixes[name.Name] = prefixes[base] + prefix
	}
}

// routerChain recognises `mux.NewRouter()` and
// `<base>.PathPrefix("/p").Subrouter()...`, returning the base variable and the
// prefix the new router adds.
func routerChain(e ast.Expr) (base, prefix string, ok bool) {
	for {
		call, isCall := e.(*ast.CallExpr)
		if !isCall {
			return "", "", false
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return "", "", false
		}
		switch sel.Sel.Name {
		case "NewRouter":
			return "", prefix, true
		case "PathPrefix":
			if len(call.Args) == 1 {
				if p, isStr := strLit(call.Args[0]); isStr {
					prefix = p + prefix
				}
			}
		case "Subrouter", "StrictSlash", "UseEncodedPath", "SkipClean":
			// chain noise, keep descending
		default:
			return "", "", false
		}
		if id, isID := sel.X.(*ast.Ident); isID {
			return id.Name, prefix, true
		}
		e = sel.X
	}
}

// --- registrations ---

// parseRegistration unwraps `<router>.HandleFunc(path, chain).Methods(...)`
// from the outside in. Anything that is not a route registration returns false.
func parseRegistration(e ast.Expr, varTypes, prefixes map[string]string) (Route, bool) {
	var methods []string
	for {
		call, isCall := e.(*ast.CallExpr)
		if !isCall {
			return Route{}, false
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return Route{}, false
		}
		switch sel.Sel.Name {
		case "HandleFunc", "Handle":
			return buildRoute(call, sel.X, methods, varTypes, prefixes)
		case "Methods":
			for _, a := range call.Args {
				if m, isStr := strLit(a); isStr {
					methods = append(methods, m)
				}
			}
		}
		e = sel.X
	}
}

func buildRoute(call *ast.CallExpr, routerExpr ast.Expr, methods []string, varTypes, prefixes map[string]string) (Route, bool) {
	if len(call.Args) < 2 {
		return Route{}, false
	}
	path, ok := strLit(call.Args[0])
	if !ok {
		return Route{}, false
	}
	routerVar := ""
	if id, isID := routerExpr.(*ast.Ident); isID {
		routerVar = id.Name
	}
	r := Route{
		Methods: methods,
		Path:    prefixes[routerVar] + path,
		Router:  routerVar,
	}
	analyzeChain(call.Args[1], &r, varTypes)
	return r, true
}

// analyzeChain pulls the auth flag, the capability, the remaining gates and the
// terminal handler out of a middleware chain.
//
// The terminal handler is the one selector expression in the tree that is
// neither called nor the left half of another selector: in
// `authHandler.AuthMiddleware(appState.Authz.RequireCap("x")(h.List))` that is
// `h.List`, because AuthMiddleware and RequireCap are callees and appState.Authz
// is only the receiver of one. Constants passed as arguments (a rate limit, for
// instance) survive that filter too, so the variable must also be a known
// handler; the deepest match wins.
func analyzeChain(e ast.Expr, r *Route, varTypes map[string]string) {
	callees := map[ast.Expr]bool{}
	receivers := map[ast.Expr]bool{}
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			callees[x.Fun] = true
		case *ast.SelectorExpr:
			receivers[x.X] = true
		}
		return true
	})

	identHandler := ""
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			// A capability bound to a variable first is still the route's
			// capability: rendering only the inline form documented the 13
			// platform-backup routes as "no capability on this method".
			if id, isID := x.Fun.(*ast.Ident); isID && strings.HasPrefix(varTypes[id.Name], capVarPrefix) {
				c := strings.TrimPrefix(varTypes[id.Name], capVarPrefix)
				r.Cap = c
				r.Caps = append(r.Caps, c)
				return true
			}
			sel, isSel := x.Fun.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			switch sel.Sel.Name {
			// The credential belongs in its own column, not among the gates: a
			// reader who saw "public" next to a warp route because it takes a
			// warp key instead of a session would draw exactly the wrong
			// conclusion.
			case "AuthMiddleware":
				r.Auth = "session"
			// Both key-route constructors. A name this switch does not know
			// falls through to `default` and the route is documented as taking
			// NO credential - which is how renaming the single constructor
			// silently reclassified a key-authed route as anonymous, in the
			// generated reference and in the frozen-anonymous-surface guard.
			// Adding a third shape means adding it here too.
			case "APIKeyServerRoute", "APIKeyOwnerRoute":
				// Its argument is a capability too, checked against the same
				// catalog - just resolved from the key's scope rather than from
				// the caller's identity. The Auth column carries that
				// difference, so the capability belongs in the capability
				// column rather than being dropped.
				r.Auth = "user API key"
				if len(x.Args) == 1 {
					if c, isStr := strLit(x.Args[0]); isStr {
						r.Cap = c
						r.Caps = append(r.Caps, c)
					}
				}
			case "WarpAPIKeyMiddleware":
				r.Auth = "warp key"
			case "RequireCap":
				if len(x.Args) == 1 {
					if c, isStr := strLit(x.Args[0]); isStr {
						r.Cap = c
						r.Caps = append(r.Caps, c)
					}
				}
			default:
				r.Gates = append(r.Gates, sel.Sel.Name)
			}
		case *ast.SelectorExpr:
			if callees[x] || receivers[x] {
				return true
			}
			id, isID := x.X.(*ast.Ident)
			if !isID {
				return true
			}
			if t, known := varTypes[id.Name]; known {
				r.Handler = t + "." + x.Sel.Name
			}
		case *ast.Ident:
			// A plain function serves a few routes (beamToolsRedirect). Only a
			// fallback: an identifier is far weaker evidence than a selector on
			// a known handler variable, so it must never outrank one.
			if !callees[x] && !receivers[x] && identHandler == "" {
				identHandler = x.Name
			}
		}
		return true
	})
	if r.Handler == "" {
		r.Handler = identHandler
	}
}

func strLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

// --- handler doc comments ---

// handlerDocs maps "TypeName.Method" to the first sentence of that method's doc
// comment, with the Go-convention name/verb/path preamble removed.
func handlerDocs(dirs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", filepath.Join(dir, name), err)
			}
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Doc == nil {
					continue
				}
				// A handler is usually a method, but a few routes are served by
				// a plain function; those are keyed by bare name, matching what
				// analyzeChain records for them.
				key := fn.Name.Name
				if fn.Recv != nil {
					if len(fn.Recv.List) == 0 {
						continue
					}
					recv := receiverType(fn.Recv.List[0].Type)
					if recv == "" {
						continue
					}
					key = recv + "." + fn.Name.Name
				}
				if doc := cleanDoc(fn.Name.Name, fn.Doc.Text()); doc != "" {
					out[key] = doc
				}
			}
		}
	}
	return out, nil
}

func receiverType(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

var (
	spaceRE = regexp.MustCompile(`\s+`)
	// The path must start with a slash. Without that, prose like
	// "DELETE removes the row" parses as a verb followed by a path and loses
	// its first two words.
	verbPathRE = regexp.MustCompile(`^(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)(?:\s*[+/,]\s*(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS))*\s+/\S*\s*`)
	// The comments separate the path from the prose with a plain hyphen or with
	// one of the Unicode dashes (U+2010..U+2015); spelled by code point so the
	// source stays ASCII.
	separatorRE  = regexp.MustCompile(`^[-\x{2010}-\x{2015}:,]+\s*`)
	abbreviation = []string{"e.g.", "i.e.", "vs.", "etc.", "cf.", "Mr.", "approx."}
)

// cleanDoc turns a Go doc comment into a description. The convention here is
// "Name VERB /path - prose", and only the prose belongs in the table; a comment
// that is nothing but name, verb and path yields "" rather than a row that
// repeats the two columns beside it.
func cleanDoc(name, doc string) string {
	s := spaceRE.ReplaceAllString(strings.TrimSpace(doc), " ")
	s = strings.TrimSpace(strings.TrimPrefix(s, name))

	// The preamble comes in several orders: "Name VERB /path - prose",
	// "Name - VERB /path prose", "Name is GET /solder/api/ - the root probe".
	// Strip whichever piece is at the front and go round again, rather than
	// assuming one fixed sequence - a single pass leaves "GET /api/... " behind
	// whenever a dash separates the name from the verb.
	for {
		before := s
		s = strings.TrimSpace(separatorRE.ReplaceAllString(s, ""))
		// Only drop a linking verb when a verb and path really follow it:
		// "GetFileContent handles requests to read the content of a file" needs
		// the word to stay a sentence.
		for _, lead := range []string{"is ", "handles ", "serves "} {
			if rest := strings.TrimPrefix(s, lead); rest != s && verbPathRE.MatchString(rest) {
				s = rest
				break
			}
		}
		s = strings.TrimSpace(verbPathRE.ReplaceAllString(s, ""))
		if s == before {
			break
		}
	}
	return firstSentence(s)
}

// firstSentence cuts at the first sentence end, stepping over the abbreviations
// that actually occur in these comments rather than attempting real parsing.
func firstSentence(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] != '.' || i+1 >= len(s) || s[i+1] != ' ' {
			continue
		}
		head := s[:i+1]
		skip := false
		for _, a := range abbreviation {
			if strings.HasSuffix(head, a) {
				skip = true
				break
			}
		}
		if !skip {
			return head
		}
	}
	return s
}
