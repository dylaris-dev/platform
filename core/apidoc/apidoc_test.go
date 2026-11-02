package apidoc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixture mirrors the shapes routes.go actually uses: a prefixed subrouter, a
// root router that bypasses it, a nested RequireCap chain, a rate limiter that
// takes a constant before the handler, and a registration with no .Methods().
const fixture = `package main

var requiredCaps = map[string]string{
	"/api/legacy": "legacy.write", // GET exempt, POST legacy.write
}

func buildAPIRouter(appState *handlers.AppState, authHandler *handlers.AuthHandler, cfg routeCfg) (*mux.Router, *routeExtras) {
	packsHandler := handlers.NewPacksHandler(appState)
	proxyHandler := handlers.NewProxyHandler(appState, authHandler)
	healthHandler := handlers.NewHealthHandler(appState)

	r := mux.NewRouter()
	api := r.PathPrefix("/api").Subrouter().StrictSlash(true)
	solder := r.PathPrefix("/solder").Subrouter()

	r.HandleFunc("/healthz", healthHandler.Healthz).Methods("GET")
	solder.HandleFunc("/api/modpack", packsHandler.ListModpacks).Methods("GET")

	// --- Packs ---
	api.HandleFunc("/packs/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.read")(appState.AllowReadOnlyWhenDisabled(packsHandler.Get)))).Methods("GET")
	api.HandleFunc("/packs/{id:[0-9]+}", authHandler.AuthMiddleware(appState.Authz.RequireCap("modpack.delete")(packsHandler.Delete))).Methods("DELETE")
	r.HandleFunc("/api/share/{token}", shareLimiter.Limit(30, packsHandler.ServeShare)).Methods("GET")
	r.HandleFunc("/api/tabproxy/{token}", proxyHandler.Public)
	api.HandleFunc("/legacy", packsHandler.Legacy).Methods("GET", "POST")
	api.HandleFunc("/nowhere", packsHandler.Nowhere).Methods("GET")
	api.HandleFunc("/plain", plainHandler).Methods("GET")
	api.HandleFunc("/auth/profile", authHandler.AuthMiddleware(authHandler.GetProfileHandler)).Methods("GET")
	pbCap := appState.Authz.RequireCap("settings.write")
	api.HandleFunc("/platform-backups/jobs", authHandler.AuthMiddleware(pbCap(packsHandler.Jobs))).Methods("GET")

	return r, nil
}
`

const fixtureHandlers = `package handlers

// Get GET /api/packs/{id} - returns one pack. Second sentence is dropped.
func (h *PacksHandler) Get(w http.ResponseWriter, r *http.Request) {}

// ListModpacks GET /solder/api/modpack
func (h *PacksHandler) ListModpacks(w http.ResponseWriter, r *http.Request) {}

func (h *PacksHandler) Delete(w http.ResponseWriter, r *http.Request) {}

// plainHandler GET /api/plain - served by a function with no receiver.
func plainHandler(w http.ResponseWriter, r *http.Request) {}
`

func parseFixture(t *testing.T) []Route {
	t.Helper()
	dir := t.TempDir()
	routesFile := filepath.Join(dir, "routes.go")
	if err := os.WriteFile(routesFile, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	handlersDir := filepath.Join(dir, "handlers")
	if err := os.MkdirAll(handlersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(handlersDir, "packs.go"), []byte(fixtureHandlers), 0o644); err != nil {
		t.Fatal(err)
	}
	routes, err := Parse(routesFile, []string{handlersDir})
	if err != nil {
		t.Fatal(err)
	}
	return routes
}

func find(t *testing.T, routes []Route, method, path string) Route {
	t.Helper()
	for _, r := range routes {
		if r.Path != path {
			continue
		}
		if method == "" && len(r.Methods) == 0 {
			return r
		}
		if slices.Contains(r.Methods, method) {
			return r
		}
	}
	t.Fatalf("no route %s %s in %d parsed routes", method, path, len(routes))
	return Route{}
}

func TestParseRoutes(t *testing.T) {
	routes := parseFixture(t)
	if len(routes) != 11 {
		t.Fatalf("parsed %d routes, want 11", len(routes))
	}

	tests := []struct {
		name    string
		method  string
		path    string
		auth    string
		cap     string
		gates   string
		handler string
		doc     string
	}{
		{
			// The subrouter prefix has to be composed in, or every /api route
			// documents a path that does not exist.
			name: "subrouter prefix is composed", method: "GET", path: "/api/packs/{id:[0-9]+}",
			auth: "session", cap: "modpack.read",
			gates:   "AllowReadOnlyWhenDisabled",
			handler: "PacksHandler.Get",
			doc:     "returns one pack.",
		},
		{
			name: "same template, second method, own capability", method: "DELETE", path: "/api/packs/{id:[0-9]+}",
			auth: "session", cap: "modpack.delete",
			gates:   "",
			handler: "PacksHandler.Delete",
			doc:     "",
		},
		{
			// Registered on the root router, so it carries no /api prefix and no
			// AuthMiddleware - that difference is the reason the route exists.
			name: "root router route keeps its literal path", method: "GET", path: "/api/share/{token}",
			auth: "", cap: "",
			gates:   "Limit",
			handler: "PacksHandler.ServeShare",
		},
		{
			// The rate limit constant is an argument too; only the handler
			// variable must survive.
			name: "no .Methods() means any method", method: "", path: "/api/tabproxy/{token}",
			auth: "", cap: "",
			gates:   "",
			handler: "ProxyHandler.Public",
		},
		{
			name: "capability bound to a variable first", method: "GET", path: "/api/platform-backups/jobs",
			auth: "session", cap: "settings.write",
			gates:   "",
			handler: "PacksHandler.Jobs",
		},
		{
			name: "second subrouter has its own prefix", method: "GET", path: "/solder/api/modpack",
			auth: "", cap: "",
			gates:   "",
			handler: "PacksHandler.ListModpacks",
			doc:     "", // the comment is nothing but name, verb and path
		},
		{
			name: "route before any banner lands in the root section", method: "GET", path: "/healthz",
			auth: "", cap: "",
			gates:   "",
			handler: "HealthHandler.Healthz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := find(t, routes, tt.method, tt.path)
			if r.Auth != tt.auth {
				t.Errorf("auth = %q, want %q", r.Auth, tt.auth)
			}
			if r.Cap != tt.cap {
				t.Errorf("cap = %q, want %q", r.Cap, tt.cap)
			}
			if got := strings.Join(r.Gates, ", "); got != tt.gates {
				t.Errorf("gates = %q, want %q", got, tt.gates)
			}
			if r.Handler != tt.handler {
				t.Errorf("handler = %q, want %q", r.Handler, tt.handler)
			}
			if r.Doc != tt.doc {
				t.Errorf("doc = %q, want %q", r.Doc, tt.doc)
			}
		})
	}
}

func TestParseMultipleMethodsOnOneRegistration(t *testing.T) {
	r := find(t, parseFixture(t), "POST", "/api/legacy")
	if strings.Join(r.Methods, ",") != "GET,POST" {
		t.Errorf("methods = %v, want [GET POST]", r.Methods)
	}
}

// requiredCaps is keyed by path template, so a route whose own chain has no
// RequireCap still satisfies route coverage as long as a sibling method on the
// same template carries one. The reference has to say so, because that is the
// one case a reader would otherwise mistake for an oversight.
func TestUncappedMethodOnACappedTemplate(t *testing.T) {
	r := find(t, parseFixture(t), "GET", "/api/legacy")
	if r.Cap != "" {
		t.Errorf("cap = %q, want none", r.Cap)
	}
	if r.Authz != "uncapped method" {
		t.Errorf("authz = %q, want %q", r.Authz, "uncapped method")
	}
}

// Not every route is served by a method on a handler struct; a few are plain
// functions. Recording those as "(inline)" would hide a real, documented
// function behind a label that means "there is nothing to point at".
func TestPlainFunctionHandlerIsNamedAndDocumented(t *testing.T) {
	r := find(t, parseFixture(t), "GET", "/api/plain")
	if r.Handler != "plainHandler" {
		t.Errorf("handler = %q, want %q", r.Handler, "plainHandler")
	}
	if r.Doc != "served by a function with no receiver." {
		t.Errorf("doc = %q", r.Doc)
	}
}

// The exempt bucket carries two very different things and must not render as
// one label: /healthz needs nothing at all, while an AUTHED-EXEMPT route needs
// a credential and is scoped to the caller inside the handler. Calling both
// "exempt" would read as "anyone may call this".
func TestExemptSplitsOnTheCredential(t *testing.T) {
	routes := parseFixture(t)
	if r := find(t, routes, "GET", "/healthz"); r.Authz != "public" {
		t.Errorf("/healthz authz = %q, want %q", r.Authz, "public")
	}
	// /api/auth/profile is AUTHED-EXEMPT in the real map: session, no capability.
	if r := find(t, routes, "GET", "/api/auth/profile"); r.Authz != "no capability" {
		t.Errorf("/api/auth/profile authz = %q, want %q", r.Authz, "no capability")
	}
}

// A path in neither requiredCaps nor either authz map gets no classification at
// all - saying "exempt" there would invent a decision nobody made.
//
// The path has to be one that cannot exist for real: the classification reads
// the compiled-in authz maps, which is the point for routes.go and means a
// fixture cannot borrow a genuine path like /healthz to test the empty case.
func TestUnclassifiedRouteClaimsNothing(t *testing.T) {
	r := find(t, parseFixture(t), "GET", "/api/nowhere")
	if r.Authz != "" {
		t.Errorf("authz = %q, want empty", r.Authz)
	}
}

// A file with no buildAPIRouter, or one that registers nothing, means the
// parser has fallen out of step with the source. It must say so rather than
// render an empty document that looks like a project with no API.
func TestParseFailsLoudOnAnEmptyRouter(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "routes.go")
	if err := os.WriteFile(f, []byte("package main\n\nfunc buildAPIRouter() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(f, nil); err == nil {
		t.Fatal("want an error for a router that registers nothing, got nil")
	}

	g := filepath.Join(dir, "other.go")
	if err := os.WriteFile(g, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(g, nil); err == nil {
		t.Fatal("want an error when buildAPIRouter is absent, got nil")
	}
}

func TestCleanDoc(t *testing.T) {
	tests := []struct {
		name string
		fn   string
		doc  string
		want string
	}{
		{
			// The whole comment is the two columns beside it; an empty
			// description is more honest than repeating them.
			name: "name verb and path alone yield nothing",
			fn:   "MarkAllRead", doc: "MarkAllRead POST /api/notifications/read-all\n",
			want: "",
		},
		{
			name: "prose after a hyphen survives",
			fn:   "Get", doc: "Get GET /api/admin/settings/users - PANEL settings.read (RequireCap at the route).\n",
			want: "PANEL settings.read (RequireCap at the route).",
		},
		{
			name: "an em dash separates too",
			fn:   "Info", doc: "Info is GET /solder/api/ — the root probe.\n",
			want: "the root probe.",
		},
		{
			name: "only the first sentence is kept",
			fn:   "Start", doc: "Start POST /api/admin/storage/migration - Returns 202 with the initial job. Returns 409 when one is already running.\n",
			want: "Returns 202 with the initial job.",
		},
		{
			name: "a semicolon does not end a sentence",
			fn:   "DownloadRun", doc: "DownloadRun GET /api/backup-runs/{runId}/download - 302 for S3; streams for local storage.\n",
			want: "302 for S3; streams for local storage.",
		},
		{
			name: "an abbreviation does not end a sentence",
			fn:   "Pick", doc: "Pick POST /api/placement/pick - Chooses a node, e.g. the least loaded one in the region.\n",
			want: "Chooses a node, e.g. the least loaded one in the region.",
		},
		{
			name: "a linking verb before a real path goes with it",
			fn:   "Assignment", doc: "Assignment handles GET /api/warp/assignment?public_key=... - one peer's endpoints.\n",
			want: "one peer's endpoints.",
		},
		{
			// The same word carries the sentence when no path follows.
			name: "a linking verb without a path stays",
			fn:   "GetFileContent", doc: "GetFileContent handles requests to read the content of a file\n",
			want: "handles requests to read the content of a file",
		},
		{
			// The dash sits BEFORE the verb here, so a single strip pass in a
			// fixed order leaves the whole "GET /path" behind in the table.
			name: "a dash between the name and the verb",
			fn:   "Get2FAStatus", doc: "Get2FAStatus — GET /api/auth/2fa/status\nReturns whether 2FA is enabled.\n",
			want: "Returns whether 2FA is enabled.",
		},
		{
			// "DELETE removes ..." is prose, not a verb and a path.
			name: "a verb word in prose is not a route line",
			fn:   "Purge", doc: "Purge - DELETE removes the row and its archive.\n",
			want: "DELETE removes the row and its archive.",
		},
		{
			name: "a plain prose comment is left alone",
			fn:   "GetLinkUpdateSettings", doc: "GetLinkUpdateSettings returns the current Link update policy.\n",
			want: "returns the current Link update policy.",
		},
		{
			name: "multi-line comments collapse to one line",
			fn:   "DemoStatus", doc: "DemoStatus GET /api/auth/demo-login - public,\nrate-limited.\n",
			want: "public, rate-limited.",
		},
		{
			name: "no doc comment at all",
			fn:   "Delete", doc: "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanDoc(tt.fn, tt.doc); got != tt.want {
				t.Errorf("cleanDoc(%q) = %q, want %q", tt.doc, got, tt.want)
			}
		})
	}
}

func TestAnchorMatchesGitHubSlugRules(t *testing.T) {
	used := map[string]int{}
	tests := []struct {
		in, want string
	}{
		{"Custom Tabs", "custom-tabs"},
		{"RCON + API keys", "rcon--api-keys"},
		{"Solder client/key management (authed)", "solder-clientkey-management-authed"},
		{"Custom Tabs", "custom-tabs-1"}, // a repeat gets GitHub's numeric suffix
	}
	for _, tt := range tests {
		if got := anchor(tt.in, used); got != tt.want {
			t.Errorf("anchor(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRenderEscapesPipes(t *testing.T) {
	out := Render([]Route{{
		Methods: []string{"GET"}, Path: "/x",
		Handler: "H.M", Doc: "returns a|b",
	}}, "preamble")
	if !strings.Contains(out, `returns a\|b`) {
		t.Errorf("an unescaped pipe breaks the table row:\n%s", out)
	}
}

func TestNormalizeStripsCarriageReturns(t *testing.T) {
	if got := Normalize("a\r\nb\r\n"); got != "a\nb\n" {
		t.Errorf("Normalize = %q", got)
	}
}
