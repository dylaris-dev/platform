package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"dylaris-core/apidoc"
	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/handlers/auth"
	"dylaris-core/services"
)

// remoteGated is every route that only works against this platform's own
// gateway state (Redis, Hub, warp) and therefore answers 409 gateway_remote
// while the managed gateway is the provider. On key-authenticated routes (warp,
// link heartbeat, the provider's external API) the gate sits behind the auth
// and the limiters like everywhere else, so an anonymous caller learns nothing
// about the mode.
var remoteGated = []struct{ method, tpl string }{
	// Admin gateway topology, all routes, bulk tools (map row 1).
	{"GET", "/api/gateway/links"},
	{"GET", "/api/gateway/edges"},
	{"GET", "/api/gateway/routes"},
	{"GET", "/api/gateway/routes/suffixes"},
	{"POST", "/api/gateway/routes/bulk-delete"},
	{"POST", "/api/gateway/routes/{domain:[^/]+}/suspend"},
	{"POST", "/api/gateway/routes/{domain:[^/]+}/unsuspend"},
	{"DELETE", "/api/gateway/routes/{domain:.+}"},
	{"GET", "/api/gateway/logs"},
	{"GET", "/api/gateway/stats"},
	{"POST", "/api/gateway/sync"},
	{"GET", "/api/gateway/errors"},
	// Route-only (map rows 5-7; route-only is not offered on the managed gateway).
	{"GET", "/api/admin/users/{id:[0-9a-f-]{36}}/gateway-public-targets"},
	{"PUT", "/api/admin/users/{id:[0-9a-f-]{36}}/gateway-public-targets"},
	// The admin view of one managed gateway customer: provider-only.
	{"GET", "/api/admin/users/{id:[0-9a-f-]{36}}/gateway"},
	{"GET", "/api/gateway/link-routes"},
	{"POST", "/api/gateway/link-routes"},
	{"PATCH", "/api/gateway/link-routes/{domain:[^/]+}/proxy-protocol"},
	{"PUT", "/api/gateway/link-routes/{domain:[^/]+}/voice"},
	{"PUT", "/api/gateway/link-routes/{domain:[^/]+}/bedrock"},
	{"GET", "/api/gateway/link-routes/{domain:[^/]+}/floodgate-key"},
	{"DELETE", "/api/gateway/link-routes/{domain:.+}"},
	// Voice and Bedrock are refused in 7a (map rows 8, 9).
	{"GET", "/api/servers/{id:[0-9]+}/voice"},
	{"GET", "/api/servers/{id:[0-9]+}/bedrock"},
	{"PUT", "/api/servers/{id:[0-9]+}/bedrock/routes"},
	{"GET", "/api/admin/voice-permissions"},
	{"PUT", "/api/admin/voice-permissions"},
	{"DELETE", "/api/admin/voice-permissions"},
	{"GET", "/api/voice/overview"},
	// The provider's external API: a customer Core is not a provider (map row 11).
	{"GET", "/api/external/gateway/link-kits"},
	{"POST", "/api/external/gateway/link-kits"},
	{"DELETE", "/api/external/gateway/link-kits/{linkID}"},
	{"POST", "/api/external/gateway/link-kits/{linkID}/roll"},
	{"POST", "/api/external/gateway/link-kits/{linkID}/retire"},
	{"GET", "/api/external/gateway/routes"},
	{"POST", "/api/external/gateway/routes"},
	{"DELETE", "/api/external/gateway/routes/{domain:[^/]+}"},
	{"PATCH", "/api/external/gateway/routes/{domain:[^/]+}/proxy-protocol"},
	{"PUT", "/api/external/gateway/routes/{domain:[^/]+}/voice"},
	{"PUT", "/api/external/gateway/routes/{domain:[^/]+}/bedrock"},
	{"GET", "/api/external/gateway/custom-domains"},
	{"POST", "/api/external/gateway/custom-domains"},
	{"DELETE", "/api/external/gateway/custom-domains/{domain:[^/]+}"},
	{"POST", "/api/external/gateway/custom-domains/{domain:[^/]+}/txt-token"},
	{"POST", "/api/external/gateway/custom-domains/{domain:[^/]+}/verify-txt"},
	{"GET", "/api/external/gateway/regions"},
	{"GET", "/api/external/gateway/account"},
	{"GET", "/api/external/gateway/check-domain"},
	{"GET", "/api/external/gateway/usage"},
	{"GET", "/api/external/gateway/beam/keys"},
	{"POST", "/api/external/gateway/beam/keys"},
	{"DELETE", "/api/external/gateway/beam/keys/{kid}"},
	{"GET", "/api/external/gateway/beam/relays"},
	// The link heartbeat API and link boot: the customer's links talk to the
	// provider, never to this Core (map row 12).
	{"POST", "/api/warp/link-boot"},
	{"GET", "/api/warp/link/edges"},
	{"GET", "/api/warp/link/beams"},
	{"POST", "/api/warp/link/heartbeat"},
	{"POST", "/api/warp/link/stats"},
	{"GET", "/api/warp/link/voice"},
	{"GET", "/api/warp/link/targets"},
	// Local link kits (map row 13; kits are minted by the provider).
	{"GET", "/api/warp/link-kits"},
	{"POST", "/api/warp/link-kits"},
	{"DELETE", "/api/warp/link-kits/{linkID}"},
	{"POST", "/api/warp/link-kits/{linkID}/roll"},
	// Warp and home nodes (map row 40, spec P6).
	{"POST", "/api/warp/enroll"},
	{"GET", "/api/warp/assignment"},
	{"GET", "/api/warp/regions"},
	{"POST", "/api/warp/regions"},
	{"DELETE", "/api/warp/regions/{region}"},
	{"POST", "/api/warp/leaders"},
	{"DELETE", "/api/warp/leaders/{leaderId}"},
	{"POST", "/api/admin/warp/keys"},
	{"GET", "/api/admin/warp/keys"},
	{"DELETE", "/api/admin/warp/keys/{id:[0-9]+}"},
	{"DELETE", "/api/admin/warp/keys/{id:[0-9]+}/purge"},
	{"POST", "/api/admin/warp/keys/{id:[0-9]+}/roll"},
	{"POST", "/api/admin/warp/external-nodes"},
	{"POST", "/api/warp/node-keys"},
	{"GET", "/api/warp/node-keys"},
	{"DELETE", "/api/warp/node-keys/{nodeID}"},
	{"POST", "/api/warp/node-keys/{nodeID}/roll"},
	{"POST", "/api/warp/node-keys/{nodeID}/bind"},
	{"GET", "/api/settings/warp-firewall"},
	{"POST", "/api/settings/warp-firewall"},
	// Hub DNS form (map row 14).
	{"GET", "/api/settings/gateway/dns"},
	{"PUT", "/api/settings/gateway/dns"},
	{"POST", "/api/settings/gateway/dns/probe"},
	// Edge MOTD: our edges cannot read the customer's Redis (map row 21).
	{"GET", "/api/servers/{id:[0-9]+}/edge-motd"},
	{"PATCH", "/api/servers/{id:[0-9]+}/edge-motd"},
	// Bandwidth panel and warp rebalance (map row 34).
	{"GET", "/api/gateway-bandwidth/overview"},
	{"GET", "/api/gateway-bandwidth/history"},
	{"GET", "/api/gateway-bandwidth/rebalance"},
	{"POST", "/api/gateway-bandwidth/rebalance"},
	// Live player counts from the edges (map row 35; not on the provider API).
	{"GET", "/api/servers/players"},
}

// remoteAware is every route served by handlers/gateway, or by a handler that
// reaches gateway state (gatewayTouchingHandlers), that is deliberately NOT
// gated, with the reason. Such a new route must land in one of the two lists:
// unclassified, it could read empty Redis keys in remote mode.
var remoteAware = map[string]string{
	"GET /api/servers/{id:[0-9]+}/routes":                "lists the local remote_routes rows minus pending deletes (RemoteGateway.ServerRoutes)",
	"POST /api/servers/{id:[0-9]+}/routes":               "creates through the switching GatewayProvider; provider refusals mapped by gate.GatewayProviderError",
	"DELETE /api/servers/{id:[0-9]+}/routes/{domain:.+}": "checks the local remote_routes rows, deletes through the provider outbox",
	"GET /api/servers/{id:[0-9]+}/proxy-protocol":        "DB and node keys only; reports Auto unavailable in remote mode",
	"PATCH /api/servers/{id:[0-9]+}/proxy-protocol":      "precheck from the kits' client-addr capability (KitStatus); Auto refused",
	"GET /api/gateway/check-domain":                      "local remote_routes rows first, then the provider (CheckDomain, cached 10 s); 30/min per user",
	"PUT /api/servers/{id:[0-9]+}/voice/switch":          "turning voice off never needs the gateway (local rows only, no Redis in remote mode); turning it on answers the mode sentinel",
	"GET /api/warp/deploy-config":                        "DB settings and the gRPC certificate fingerprint only, no gateway state; node deploy snippets read it",
	// Own domains (7b-1): the user's local claim plus the provider's claim.
	"GET /api/gateway/custom-domains":                         "the caller's local claims, each with the provider's claim (ProviderCustomDomains, cached) filtered to them",
	"POST /api/gateway/custom-domains":                        "local claim first, then EnsureProviderClaim; a provider refusal takes back a claim made by the call",
	"DELETE /api/gateway/custom-domains/{domain:[^/]+}":       "in-use check from the local remote_routes rows of the caller's servers, then ReleaseProviderClaim",
	"POST /api/gateway/custom-domains/{domain:.+}/txt-token":  "the local claim's own record only, no gateway state",
	"POST /api/gateway/custom-domains/{domain:.+}/verify-txt": "local DNS check, then VerifyProviderClaim; 10/min per user in the handler",
	// The managed gateway's own endpoints: they refuse in LOCAL mode instead
	// (TestManagedOnlyRoutesRefuseInLocalMode).
	"GET /api/nodes/{id:[0-9]+}/managed-link":           "the node's provider link kit (EnsureKit); 409 gateway_local in local mode",
	"GET /api/gateway/provider/usage":                   "the provider's usage (RemoteGateway.Usage); 409 gateway_local in local mode",
	"POST /api/nodes/{id:[0-9]+}/managed-link/retire":   "retires the node's provider link kit (RetireNodeKit); 409 gateway_local in local mode",
	"GET /api/gateway/provider/orphan-kits":             "provider kits no local remote_link_kits row names (OrphanKits); 409 gateway_local in local mode",
	"DELETE /api/gateway/provider/orphan-kits/{linkId}": "revokes a provider kit after re-reading the local rows (RevokeOrphanKit); 409 gateway_local in local mode",
	"GET /api/settings/gateway/provider":                "chooses the provider: settings and, with a key set, the provider's account",
	"POST /api/settings/gateway/provider":               "chooses the provider: checks the account before remote, deletes nothing on local",
	"POST /api/settings/gateway/provider/test":          "chooses the provider: one account call with the given or stored key, persists nothing",
	// Outside handlers/gateway (found by gatewayTouchingHandlers).
	"GET /api/admin/health":                    "every gateway component (gateway, voice, bedrock) checks LocalGatewayErr first and reports the mode instead of reading Redis",
	"GET /api/infrastructure/overview":         "edges, links and route count only when LocalGatewayErr is nil; link presence from the provider (RemoteGateway.SetNodesLinkOnline), nothing while the mode is unread",
	"GET /api/nodes":                           "link presence from the provider (RemoteGateway.SetNodesLinkOnline) in remote mode, nothing while the mode is unread",
	"GET /api/gateway/dns-check":               "edges through LocalGatewayRedis (nil in remote mode) and no hoster or custom-domain records: only the operator's own origin records",
	"POST /api/servers/{id:[0-9]+}/setup":      "proxy-protocol default asks the provider (ServerDomains, NodeKitCarriesClientAddr) in remote mode and says no while the mode is unread",
	"POST /api/admin/servers/{id:[0-9]+}/move": "queueMigration checks the target's provider kit (retiring, client-addr) in remote mode; real player IP refuses while the mode is unread",
	"POST /api/servers/{id:[0-9]+}/transfer":   "same queueMigration checks as the move",
	"GET /api/settings/beam":                   "relay discovery through LocalGatewayRedis (nil in remote mode): only the manual override; provider relays are stage 7b-3",
	"GET /api/beam/config":                     "relay through LocalGatewayRedis (nil in remote mode): only the manual override; provider relays are stage 7b-3",
	"GET /api/beam/ticket":                     "relay through LocalGatewayRedis (nil in remote mode): only the manual override, else the direct hints; provider relays are stage 7b-3",
	"POST /api/beam/ticket":                    "same as GET /api/beam/ticket",
}

// gatewayPackageHandlers is every "Type.Method" declared in handlers/gateway,
// the names apidoc reports as a route's terminal handler.
func gatewayPackageHandlers(t *testing.T) map[string]bool {
	t.Helper()
	pkgs, err := parser.ParseDir(token.NewFileSet(), "handlers/gateway", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, p := range pkgs {
		for _, f := range p.Files {
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && receiverMethod(fn) != "" {
					out[receiverMethod(fn)] = true
				}
			}
		}
	}
	return out
}

// gatewayStateIdents are the calls that read this platform's own gateway
// state (Redis keys the edges, links and relays write) or reach the gateway
// provider. A handler that reaches one of them must be classified, wherever
// its package lives.
var gatewayStateIdents = []string{"LocalGatewayRedis", "ReadRoutesFromRedis", "GetRoutesFromRedis", "DiscoverBeamRelays", "ResolveRelay", "GatewayPlayersByServer", "LinkPresence", "LinkToken", "DiscoveryProof",
	"GetEdgesFromRedis", "GetLinksFromRedis", "CountRoutesFromRedis", "LiveBedrockEntries"}

// gatewayTouchingHandlers is every function in a non-test file under handlers/
// that names a gatewayStateIdents identifier, in its own body or in a helper it
// calls, keyed "Type.Method" for a method (the name apidoc reports) and
// "pkg.Func" for a function. Followed calls: on the method's own receiver
// (h.helper), to a function of the same package, and to a function of another
// handlers package (nodeops.Helper). Not followed: calls through a field or an
// interface (h.d.Gateway.X), func values, and code outside handlers/. Also
// blind to a handler that reads gateway Redis keys by literal string through
// its own Redis client (h.d.Redis.Get(ctx, "dylaris:gateway:...")). Names are
// matched without types, so a struct field called LinkToken counts and two
// types of one name in two packages share a key: both only ADD a requirement.
func gatewayTouchingHandlers(t *testing.T) map[string]bool {
	t.Helper()
	want := map[string]bool{}
	for _, n := range gatewayStateIdents {
		want[n] = true
	}
	out := map[string]bool{}
	calls := map[string][]string{}
	err := filepath.WalkDir("handlers", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		pkg := f.Name.Name
		handlerPkgs := map[string]string{} // local import name -> package name
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if rest, ok := strings.CutPrefix(p, "dylaris-core/handlers/"); ok {
				name := rest[strings.LastIndex(rest, "/")+1:]
				local := name
				if imp.Name != nil {
					local = imp.Name.Name
				}
				handlerPkgs[local] = name
			}
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name, typ, recv := pkg+"."+fn.Name.Name, "", ""
			if m := receiverMethod(fn); m != "" {
				name, typ = m, strings.TrimSuffix(m, "."+fn.Name.Name)
				if names := fn.Recv.List[0].Names; len(names) == 1 {
					recv = names[0].Name
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.Ident:
					if want[x.Name] {
						out[name] = true
					}
				case *ast.CallExpr:
					switch c := x.Fun.(type) {
					case *ast.Ident:
						calls[name] = append(calls[name], pkg+"."+c.Name)
					case *ast.SelectorExpr:
						if id, ok := c.X.(*ast.Ident); ok && recv != "" && id.Name == recv {
							calls[name] = append(calls[name], typ+"."+c.Sel.Name)
						} else if ok && handlerPkgs[id.Name] != "" {
							calls[name] = append(calls[name], handlerPkgs[id.Name]+"."+c.Sel.Name)
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for changed := true; changed; {
		changed = false
		for fn, callees := range calls {
			for _, c := range callees {
				if !out[fn] && out[c] {
					out[fn], changed = true, true
				}
			}
		}
	}
	return out
}

// receiverMethod is "Type.Method" for a method declaration, else "".
func receiverMethod(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return ""
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return ""
}

var routeVar = regexp.MustCompile(`\{(\w+)(:[^{}]*(\{\d+\}[^{}]*)*)?\}`)

// samplePath fills a route template with values its patterns accept.
func samplePath(tpl string) string {
	return routeVar.ReplaceAllStringFunc(tpl, func(v string) string {
		switch {
		case strings.Contains(v, "{36}"):
			return "00000000-0000-0000-0000-000000000001"
		case strings.HasPrefix(v, "{id"):
			return "1"
		case strings.HasPrefix(v, "{domain"):
			return "play.example.com"
		default:
			return "x1"
		}
	})
}

func remoteGateServer(t *testing.T, mode func() (bool, bool)) (http.Handler, testIdentity) {
	t.Helper()
	fs := &authzFakeStore{settings: map[string]string{"routing_mode": "gateway"}}
	admin := fs.addUser("admin-id", "root", true)
	fs.addUser("member-id", "member", false)
	appState := &handlers.AppState{
		Store:         fs,
		FeatureFlags:  services.NewFeatureFlags(fs),
		Authz:         authz.NewResolver(fs),
		GatewayRemote: mode,
	}
	appState.FeatureFlags.SetGatewayRemote(mode)
	authHandler := auth.NewAuthHandler(authDepsOf(appState), testJWTSecret)
	root, _ := buildAPIRouter(appState, authHandler, routeCfg{JWTSecret: testJWTSecret})
	srv := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		root.ServeHTTP(w, r)
	})
	return srv, testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}
}

func serveAs(t *testing.T, h http.Handler, method, path string, id testIdentity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+mintToken(t, id))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func answeredGatewayRemote(rec *httptest.ResponseRecorder) bool {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code == http.StatusConflict && body.Code == "gateway_remote"
}

func TestRemoteModeGatesEveryLocalOnlyRoute(t *testing.T) {
	// The guard: every handlers/gateway or gateway-touching route is
	// classified, and every entry still names a registered route.
	routes, err := apidoc.Parse("routes.go", []string{"handlers"})
	if err != nil {
		t.Fatal(err)
	}
	gated := map[string]bool{}
	for _, g := range remoteGated {
		k := g.method + " " + g.tpl
		if gated[k] {
			t.Errorf("remoteGated lists %s twice", k)
		}
		gated[k] = true
	}
	inGateway := gatewayPackageHandlers(t)
	touching := gatewayTouchingHandlers(t)
	registered := map[string]bool{}
	byKey := map[string]apidoc.Route{}
	for _, r := range routes {
		for _, k := range (walkedRoute{methods: r.Methods, path: r.Path}).keys() {
			registered[k] = true
			byKey[k] = r
			_, aware := remoteAware[k]
			switch {
			case gated[k] && aware:
				t.Errorf("%s is in both remoteGated and remoteAware", k)
			case (inGateway[r.Handler] || touching[r.Handler]) && !gated[k] && !aware:
				t.Errorf("%s (%s) is unclassified for remote mode: wrap it in gates.RequireLocalGateway and add it to remoteGated, or make it remote-aware and add it to remoteAware with the reason", k, r.Handler)
			}
		}
	}
	for k := range gated {
		if !registered[k] {
			t.Errorf("remoteGated[%q] matches no registered route", k)
		}
	}
	for k := range remoteAware {
		if !registered[k] {
			t.Errorf("remoteAware[%q] matches no registered route", k)
		}
	}

	srv, admin := remoteGateServer(t, func() (bool, bool) { return true, true })
	for _, g := range remoteGated {
		r := byKey[g.method+" "+g.tpl]
		gatedInChain := false
		for _, name := range r.Gates {
			gatedInChain = gatedInChain || name == "RequireLocalGateway"
		}
		if !gatedInChain {
			t.Errorf("%s %s carries no RequireLocalGateway in its chain %v", g.method, g.tpl, r.Gates)
		}
		path := samplePath(g.tpl)
		if r.Auth != "session" {
			// A warp or API key the fake store cannot mint: the chain check
			// above is the proof, and an anonymous caller must meet the auth,
			// never the mode.
			req := httptest.NewRequest(g.method, path, strings.NewReader("{}"))
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if answeredGatewayRemote(rec) || (rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden) {
				t.Errorf("remote mode, anonymous: %s %s answered %d %s, want the auth's 401/403", g.method, path, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec := serveAs(t, srv, g.method, path, admin); !answeredGatewayRemote(rec) {
			t.Errorf("remote mode: %s %s answered %d %s, want 409 gateway_remote", g.method, path, rec.Code, rec.Body.String())
		}
	}

	// Control: the refusal comes from the mode. Routes that answer before any
	// gateway state is touched in local mode (key auth, a settings read).
	local, admin := remoteGateServer(t, func() (bool, bool) { return false, true })
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/warp/enroll"},
		{"POST", "/api/warp/link/heartbeat"},
		{"GET", "/api/external/gateway/routes"},
		{"GET", "/api/settings/warp-firewall"},
	} {
		if rec := serveAs(t, local, c.method, c.path, admin); answeredGatewayRemote(rec) || rec.Code == http.StatusServiceUnavailable {
			t.Errorf("local mode: %s %s answered %d %s, the gate must pass", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

// The managed gateway's own routes have nothing to serve on a local gateway.
func TestManagedOnlyRoutesRefuseInLocalMode(t *testing.T) {
	local, admin := remoteGateServer(t, func() (bool, bool) { return false, true })
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/nodes/1/managed-link"},
		{"GET", "/api/gateway/provider/usage"},
		{"POST", "/api/nodes/1/managed-link/retire"},
		{"GET", "/api/gateway/provider/orphan-kits"},
		{"DELETE", "/api/gateway/provider/orphan-kits/link-x"},
	} {
		rec := serveAs(t, local, c.method, c.path, admin)
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusConflict || body.Code != "gateway_local" {
			t.Errorf("local mode: %s %s answered %d %s, want 409 gateway_local", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

// The kit answer carries a link key: an account without topology.write never
// reaches the handler, whatever the mode.
func TestManagedLinkEndpointIsAdminOnly(t *testing.T) {
	srv, _ := remoteGateServer(t, func() (bool, bool) { return true, true })
	member := testIdentity{UserID: "member-id", Username: "member"}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/nodes/1/managed-link"},
		{"POST", "/api/nodes/1/managed-link/retire"},
		{"GET", "/api/gateway/provider/orphan-kits"},
		{"DELETE", "/api/gateway/provider/orphan-kits/link-x"},
	} {
		if rec := serveAs(t, srv, c.method, c.path, member); rec.Code != http.StatusForbidden {
			t.Errorf("non-admin: %s %s answered %d %s, want 403", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}
