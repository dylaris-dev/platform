package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"dylaris-core/authz"
	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/golang-jwt/jwt/v5"
)

const testJWTSecret = "phase4-test-secret"

type authzFakeStore struct {
	store.Store
	users         map[string]*models.User
	usersByID     map[string]*models.User
	servers       map[int]*models.Server
	serversByUUID map[string]*models.Server
	settings      map[string]string
	panelRoles    map[int]*store.PanelRole
	serverRoles   map[int]*store.ServerRole
	panelAuthz    map[string]struct {
		roleID    *int
		overrides store.CapOverrides
	}
	serverGrants  map[string]*store.ServerGrant
	accountGrants map[string]*store.ServerGrant
}

func (f *authzFakeStore) GetUserByUsername(u string) (*models.User, error) {
	if user, ok := f.users[u]; ok {
		return user, nil
	}
	return nil, sql.ErrNoRows
}
func (f *authzFakeStore) GetUserByID(id string) (*models.User, error) {
	if user, ok := f.usersByID[id]; ok {
		return user, nil
	}
	return nil, sql.ErrNoRows
}
func (f *authzFakeStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.servers[id]; ok {
		return s, nil
	}
	return nil, sql.ErrNoRows
}
func (f *authzFakeStore) GetServerByUUID(u string) (*models.Server, error) {
	if s, ok := f.serversByUUID[u]; ok {
		return s, nil
	}
	return nil, sql.ErrNoRows
}
func (f *authzFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *authzFakeStore) GetPanelRole(id int) (*store.PanelRole, error) {
	if r, ok := f.panelRoles[id]; ok {
		return r, nil
	}
	return nil, sql.ErrNoRows
}
func (f *authzFakeStore) GetServerRole(id int) (*store.ServerRole, error) {
	if r, ok := f.serverRoles[id]; ok {
		return r, nil
	}
	return nil, sql.ErrNoRows
}
func (f *authzFakeStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	if v, ok := f.panelAuthz[userID]; ok {
		return v.roleID, v.overrides, nil
	}
	return nil, store.CapOverrides{}, nil
}
func (f *authzFakeStore) GetServerGrant(serverID int, userID string) (*store.ServerGrant, error) {
	if g, ok := f.serverGrants[skey(serverID, userID)]; ok {
		return g, nil
	}
	return nil, sql.ErrNoRows
}
func (f *authzFakeStore) GetAccountGrant(ownerUserID, userID string) (*store.ServerGrant, error) {
	if g, ok := f.accountGrants[ownerUserID+"|"+userID]; ok {
		return g, nil
	}
	return nil, sql.ErrNoRows
}

// CountAdmins/CountUsers back handlers.AppState.RequireSetupComplete, which
// wraps every /api/* route (except /api/setup/*) ahead of AuthMiddleware and
// RequireCap. Reporting one admin keeps the harness in "Complete mode" (a
// pass-through) instead of panicking on the nil embedded store.Store.
func (f *authzFakeStore) CountAdmins() (int, error) { return 1, nil }
func (f *authzFakeStore) CountUsers() (int, error)  { return 1, nil }

func skey(serverID int, userID string) string {
	return strconv.Itoa(serverID) + "|" + userID
}

type testIdentity struct {
	UserID   string
	Username string
	IsAdmin  bool
}

func mintToken(t *testing.T, id testIdentity) string {
	t.Helper()
	claims := &handlers.Claims{
		Username: id.Username,
		IsAdmin:  id.IsAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newAuthzTestServer(t *testing.T, fs *authzFakeStore) http.Handler {
	t.Helper()
	appState := &handlers.AppState{
		Store:        fs,
		FeatureFlags: services.NewFeatureFlags(fs),
		Authz:        authz.NewResolver(fs),
	}
	appState.Authz.SetDemoRead(appState.IsDemoServerID)
	authHandler := handlers.NewAuthHandler(appState, testJWTSecret)
	root, _ := buildAPIRouter(appState, authHandler, routeCfg{JWTSecret: testJWTSecret})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		root.ServeHTTP(w, r)
	})
}

func doAs(t *testing.T, h http.Handler, method, path string, id testIdentity) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+mintToken(t, id))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func (f *authzFakeStore) addUser(id, username string, admin bool) *models.User {
	u := &models.User{ID: id, Username: username, IsAdmin: admin}
	if f.users == nil {
		f.users = map[string]*models.User{}
		f.usersByID = map[string]*models.User{}
	}
	f.users[username] = u
	f.usersByID[id] = u
	return u
}

// panelHolder registers a non-admin user carrying exactly the given PANEL
// caps via a synthetic panel role (no seed 'admin'/'support' collision: role
// ids start at 1000). Reused by every PANEL batch's tests from here on.
func panelHolder(fs *authzFakeStore, userID, username string, caps ...string) {
	fs.addUser(userID, username, false)
	roleID := 1000 + len(fs.panelRoles)
	if fs.panelRoles == nil {
		fs.panelRoles = map[int]*store.PanelRole{}
		fs.panelAuthz = map[string]struct {
			roleID    *int
			overrides store.CapOverrides
		}{}
	}
	fs.panelRoles[roleID] = &store.PanelRole{ID: roleID, Capabilities: caps}
	rid := roleID
	fs.panelAuthz[userID] = struct {
		roleID    *int
		overrides store.CapOverrides
	}{roleID: &rid}
}

func TestAuthzHarness_Smoke_Auth(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("u1", "alice", false)
	srv := newAuthzTestServer(t, fs)
	// A registered user with a valid token must NOT be rejected at auth (not 401)
	// on an authenticated route. Pick a route that exists today and needs only auth.
	code := doAs(t, srv, "GET", "/api/status", testIdentity{UserID: "u1", Username: "alice"})
	if code == 0 {
		t.Fatal("harness produced no response")
	}
}

func intPtr(i int) *int { return &i }

// doJSON mirrors doAs but sends a JSON request body, for routes whose
// authorization decision (or, for POWER, the in-handler resolver check) reads
// from the parsed body rather than only the path.
func doJSON(t *testing.T, h http.Handler, method, path, body string, id testIdentity) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+mintToken(t, id))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// --- Phase 4 Task 3: server lifecycle / power / proxy / storage ---

func TestCap_ServerDelete(t *testing.T) {
	fs := &authzFakeStore{}
	owner := fs.addUser("owner-id", "owner", false)
	fs.addUser("stranger-id", "stranger", false)
	admin := fs.addUser("admin-id", "root", true)
	fs.servers = map[int]*models.Server{7: {ID: 7, OwnerID: owner.ID, OwnerName: "owner"}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "DELETE", "/api/servers/7", testIdentity{UserID: owner.ID, Username: "owner"}); c == 403 {
		t.Errorf("owner must not be 403 on server.delete, got 403")
	}
	if c := doAs(t, srv, "DELETE", "/api/servers/7", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Errorf("admin must not be 403, got 403")
	}
	if c := doAs(t, srv, "DELETE", "/api/servers/7", testIdentity{UserID: "stranger-id", Username: "stranger"}); c != 403 {
		t.Errorf("stranger must be 403 on server.delete, got %d", c)
	}
}

func TestCap_ServerResources_GrantHolderPasses(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("friend-id", "friend", false)
	fs.addUser("nobody-id", "nobody", false)
	fs.servers = map[int]*models.Server{7: {ID: 7, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{2: {ID: 2, Capabilities: []string{"server.settings.write"}}}
	fs.serverGrants = map[string]*store.ServerGrant{
		skey(7, "friend-id"): {UserID: "friend-id", ServerRoleID: intPtr(2)},
	}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "PATCH", "/api/servers/7/resources", testIdentity{UserID: "friend-id", Username: "friend"}); c == 403 {
		t.Errorf("grant holder of server.settings.write must not be 403")
	}
	if c := doAs(t, srv, "PATCH", "/api/servers/7/resources", testIdentity{UserID: "nobody", Username: "nobody"}); c != 403 {
		t.Errorf("ungranted user must be 403, got %d", c)
	}
}

func TestCap_PowerPerAction(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("starter-id", "starter", false)
	fs.servers = map[int]*models.Server{7: {ID: 7, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{5: {ID: 5, Capabilities: []string{"power.start"}}}
	fs.serverGrants = map[string]*store.ServerGrant{
		skey(7, "starter-id"): {UserID: "starter-id", ServerRoleID: intPtr(5)},
	}
	srv := newAuthzTestServer(t, fs)
	// starter holds power.start only -> a start action must NOT be 403; a kill action MUST be 403.
	startCode := doJSON(t, srv, "POST", "/api/servers/7/power", `{"action":"start"}`, testIdentity{UserID: "starter-id", Username: "starter"})
	if startCode == 403 {
		t.Errorf("power.start holder must not be 403 on start, got 403")
	}
	killCode := doJSON(t, srv, "POST", "/api/servers/7/power", `{"action":"kill"}`, testIdentity{UserID: "starter-id", Username: "starter"})
	if killCode != 403 {
		t.Errorf("power.start holder must be 403 on kill, got %d", killCode)
	}
}

// --- Phase 4 Task 4: console + RCON ---

func TestCap_ConsoleReadVsSend(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("reader-id", "reader", false)
	fs.addUser("nobody-id", "nobody", false)
	fs.servers = map[int]*models.Server{4: {ID: 4, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{9: {ID: 9, Capabilities: []string{"console.read"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(4, "reader-id"): {UserID: "reader-id", ServerRoleID: intPtr(9)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/4/console/history", testIdentity{UserID: "reader-id", Username: "reader"}); c == 403 {
		t.Error("console.read holder must reach console history")
	}
	if c := doAs(t, srv, "POST", "/api/servers/4/console/command", testIdentity{UserID: "reader-id", Username: "reader"}); c != 403 {
		t.Errorf("console.read holder must NOT send commands (needs console.send), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/servers/4/console/history", testIdentity{UserID: "nobody-id", Username: "nobody"}); c != 403 {
		t.Errorf("ungranted user must be 403 on console.read, got %d", c)
	}
}

func TestCap_RconConfigReadVsWrite(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("cfgreader-id", "cfgreader", false)
	fs.servers = map[int]*models.Server{4: {ID: 4, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{11: {ID: 11, Capabilities: []string{"config.read"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(4, "cfgreader-id"): {UserID: "cfgreader-id", ServerRoleID: intPtr(11)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/4/rcon/config", testIdentity{UserID: "cfgreader-id", Username: "cfgreader"}); c == 403 {
		t.Error("config.read holder must read rcon config")
	}
	if c := doAs(t, srv, "PUT", "/api/servers/4/rcon/config", testIdentity{UserID: "cfgreader-id", Username: "cfgreader"}); c != 403 {
		t.Errorf("config.read holder must NOT write rcon config (needs config.write), got %d", c)
	}
}

// --- Phase 4 Task 5: file browser (in-handler resolver) ---
// /api/files/* resolves its server from ?server_uuid= (not a path {id}/{uuid}),
// so it cannot be RequireCap-gated at the route (Rule R5). It stays in-handler
// but now calls the same resolver via resolveServerUUID/getServerUUID(Read).

func TestFiles_InHandlerAuthz(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("stranger-id", "stranger", false)
	srv3 := &models.Server{ID: 3, OwnerID: "owner-id", OwnerName: "owner", UUID: "srv-3"}
	fs.servers = map[int]*models.Server{3: srv3}
	fs.serversByUUID = map[string]*models.Server{"srv-3": srv3}
	srv := newAuthzTestServer(t, fs)
	// stranger listing files of a server they do not own -> handler denies (not 200)
	req := httptest.NewRequest("GET", "/api/files?server_uuid=srv-3", nil)
	req.Header.Set("Authorization", "Bearer "+mintToken(t, testIdentity{UserID: "stranger-id", Username: "stranger"}))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Errorf("stranger must not list a foreign server's files, got 200")
	}
	// a friend with files.read CAN (not the denied path). The inner handler may 500 on the
	// nil node dial; the point is it is NOT denied at the access gate.
	fs.serverRoles = map[int]*store.ServerRole{4: {ID: 4, Capabilities: []string{"files.read"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(3, "reader-id"): {UserID: "reader-id", ServerRoleID: intPtr(4)}}
	fs.addUser("reader-id", "reader", false)
	req2 := httptest.NewRequest("GET", "/api/files?server_uuid=srv-3", nil)
	req2.Header.Set("Authorization", "Bearer "+mintToken(t, testIdentity{UserID: "reader-id", Username: "reader"}))
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code == 403 {
		t.Errorf("files.read grant holder must pass the access gate, got 403")
	}
}

// --- Phase 4 Task 6: mods + custom tabs ---

func TestCap_ModsReadWriteDelete(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("modder-id", "modder", false)
	fs.servers = map[int]*models.Server{8: {ID: 8, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{20: {ID: 20, Capabilities: []string{"mods.read", "mods.write"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(8, "modder-id"): {UserID: "modder-id", ServerRoleID: intPtr(20)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/8/mods", testIdentity{UserID: "modder-id", Username: "modder"}); c == 403 {
		t.Error("mods.read holder must list mods")
	}
	if c := doAs(t, srv, "POST", "/api/servers/8/mods", testIdentity{UserID: "modder-id", Username: "modder"}); c == 403 {
		t.Error("mods.write holder must add mods")
	}
	// modder holds read+write but NOT delete -> DELETE must be 403
	if c := doAs(t, srv, "DELETE", "/api/servers/8/mods/1", testIdentity{UserID: "modder-id", Username: "modder"}); c != 403 {
		t.Errorf("mods.write-only holder must be 403 on mods delete (needs mods.delete), got %d", c)
	}
}

func TestCap_TabsReadVsWrite(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("tabreader-id", "tabreader", false)
	fs.servers = map[int]*models.Server{8: {ID: 8, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{21: {ID: 21, Capabilities: []string{"tabs.read"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(8, "tabreader-id"): {UserID: "tabreader-id", ServerRoleID: intPtr(21)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/8/tabs", testIdentity{UserID: "tabreader-id", Username: "tabreader"}); c == 403 {
		t.Error("tabs.read holder must list tabs")
	}
	if c := doAs(t, srv, "POST", "/api/servers/8/tabs", testIdentity{UserID: "tabreader-id", Username: "tabreader"}); c != 403 {
		t.Errorf("tabs.read holder must NOT create tabs (needs tabs.write), got %d", c)
	}
}

// Minting a tab-proxy ticket takes the same capability as listing the tabs.
//
// This used to run through the route table, because the mint was a route with
// RequireCap("tabs.read") on it. It is not any more: a cookie can only be set
// for the host that answers the request, and a tab is served on its own host,
// so the mint SPLIT: the decision is made on the panel's origin, where the
// session cookie is sent (handlers.MintTicket), and the content host only stores
// what that decision produced.
//
// The invariant did not move, and it is the important half. The ticket is not a
// hint about access, it IS the access - the content handler checks nothing but
// the cookie - so a member who may not ask WHICH tabs exist must not be able to
// mint one and read a tab in full. That was a real regression once, when this
// gate said overview.read while the listing said tabs.read.
//
// Asserting it against the source is deliberate: both halves resolve their tab
// through the database, so there is no route to drive and no handler call that
// reaches the authz check without one. The ORDER is asserted too - a capability
// check after the ticket is issued would be decoration.
func TestTabTicketStillRequiresTabsRead(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("handlers", "tab_proxy_host.go"))
	if err != nil {
		t.Fatalf("read tab_proxy_host.go: %v", err)
	}
	file := string(src)

	body, ok := funcBody(file, "func (h *ProxyHandler) MintTicket(")
	if !ok {
		t.Fatal("MintTicket is gone; if the mint moved again, move this assertion with it")
	}
	resolve := strings.Index(body, "h.state.Authz.Resolve(")
	hasCap := strings.Index(body, "HasCap(tabsReadCap)")
	issue := strings.Index(body, "IssueTabProxyTicket(")
	switch {
	case resolve < 0:
		t.Error("MintTicket no longer resolves the caller's capabilities on the tab's server")
	case hasCap < 0:
		t.Error("MintTicket no longer requires tabs.read; a member refused the tab LIST could mint a ticket and read the tab in full")
	case issue < 0:
		t.Error("MintTicket no longer issues a ticket - this assertion is checking the wrong function")
	case hasCap > issue:
		t.Error("the capability check runs AFTER the ticket is issued, so it decides nothing")
	}

	// And the other end must stay a delivery step. HostMint is reachable with no
	// session at all - it has to be, a cross-origin fetch carries none - so a
	// ticket ISSUED there would be issued to nobody in particular.
	hostMint, ok := funcBody(file, "func (h *ProxyHandler) HostMint(")
	if !ok {
		t.Fatal("HostMint is gone")
	}
	if strings.Contains(hostMint, "IssueTabProxyTicket(") {
		t.Error("HostMint issues a ticket again, on an endpoint that cannot identify its caller")
	}
}

// funcBody returns the source between a function's header and the next one.
func funcBody(file, header string) (string, bool) {
	i := strings.Index(file, header)
	if i < 0 {
		return "", false
	}
	body := file[i:]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	return body, true
}

func TestFiles_DeleteNeedsDeleteCap(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("writer-id", "writer", false)
	srv3 := &models.Server{ID: 3, OwnerID: "owner-id", OwnerName: "owner", UUID: "srv-3"}
	fs.servers = map[int]*models.Server{3: srv3}
	fs.serversByUUID = map[string]*models.Server{"srv-3": srv3}
	// writer holds files.write but NOT files.delete
	fs.serverRoles = map[int]*store.ServerRole{6: {ID: 6, Capabilities: []string{"files.read", "files.write"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(3, "writer-id"): {UserID: "writer-id", ServerRoleID: intPtr(6)}}
	srv := newAuthzTestServer(t, fs)
	// A delete request from a files.write-but-not-delete holder must be denied by the gate.
	req := httptest.NewRequest("POST", "/api/files/delete?server_uuid=srv-3", strings.NewReader(`{"path":"x.txt"}`))
	req.Header.Set("Authorization", "Bearer "+mintToken(t, testIdentity{UserID: "writer-id", Username: "writer"}))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Errorf("files.write-only holder must be 403 on delete (needs files.delete), got %d", rec.Code)
	}
}

// --- Phase 4 Task 7: scheduled tasks + spark ---

func TestCap_ScheduledTasksVerbs(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("sched-id", "sched", false)
	fs.servers = map[int]*models.Server{9: {ID: 9, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{30: {ID: 30, Capabilities: []string{"schedule.read", "schedule.write"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(9, "sched-id"): {UserID: "sched-id", ServerRoleID: intPtr(30)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/9/scheduled-tasks", testIdentity{UserID: "sched-id", Username: "sched"}); c == 403 {
		t.Error("schedule.read holder must list scheduled tasks")
	}
	if c := doAs(t, srv, "POST", "/api/servers/9/scheduled-tasks", testIdentity{UserID: "sched-id", Username: "sched"}); c == 403 {
		t.Error("schedule.write holder must create scheduled tasks")
	}
	// holds read+write but NOT delete -> DELETE must be 403
	if c := doAs(t, srv, "DELETE", "/api/servers/9/scheduled-tasks/1", testIdentity{UserID: "sched-id", Username: "sched"}); c != 403 {
		t.Errorf("schedule.write-only holder must be 403 on delete (needs schedule.delete), got %d", c)
	}
}

func TestCap_SparkUse(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("nospark-id", "nospark", false)
	fs.servers = map[int]*models.Server{9: {ID: 9, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{31: {ID: 31, Capabilities: []string{"spark.use"}}}
	fs.addUser("sparker-id", "sparker", false)
	fs.serverGrants = map[string]*store.ServerGrant{skey(9, "sparker-id"): {UserID: "sparker-id", ServerRoleID: intPtr(31)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/9/spark/profiles", testIdentity{UserID: "sparker-id", Username: "sparker"}); c == 403 {
		t.Error("spark.use holder must access spark profiles")
	}
	if c := doAs(t, srv, "GET", "/api/servers/9/spark/profiles", testIdentity{UserID: "nospark-id", Username: "nospark"}); c != 403 {
		t.Errorf("user without spark.use must be 403, got %d", c)
	}
}

// --- Phase 4 Task 8: stats + per-server audit ---

func TestCap_StatsRead(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("viewer-id", "viewer", false)
	fs.addUser("nobody-id", "nobody", false)
	fs.servers = map[int]*models.Server{10: {ID: 10, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{40: {ID: 40, Capabilities: []string{"stats.read"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(10, "viewer-id"): {UserID: "viewer-id", ServerRoleID: intPtr(40)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/10/stats/history", testIdentity{UserID: "viewer-id", Username: "viewer"}); c == 403 {
		t.Error("stats.read holder must read stats history")
	}
	if c := doAs(t, srv, "GET", "/api/servers/10/stats/history", testIdentity{UserID: "nobody-id", Username: "nobody"}); c != 403 {
		t.Errorf("user without stats.read must be 403, got %d", c)
	}
}

// --- Phase 4 Task 9: members + owner server-roles/grants ---

func TestCap_MembersVerbs(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("mm-id", "mm", false)
	fs.servers = map[int]*models.Server{12: {ID: 12, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{50: {ID: 50, Capabilities: []string{"members.read", "members.write"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(12, "mm-id"): {UserID: "mm-id", ServerRoleID: intPtr(50)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/12/members", testIdentity{UserID: "mm-id", Username: "mm"}); c == 403 {
		t.Error("members.read holder must list members")
	}
	// holds read+write but NOT delete -> DELETE member must be 403. The path
	// requires a 36-char {userId:[0-9a-f-]{36}} to even route.
	if c := doAs(t, srv, "DELETE", "/api/servers/12/members/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", testIdentity{UserID: "mm-id", Username: "mm"}); c != 403 {
		t.Errorf("members.write-only holder must be 403 on member delete (needs members.delete), got %d", c)
	}
}

// TestCap_ServerRolesOwnerScopedChokepointOpen documents the F2 design:
// OWNER-scope routes (no path {id}/{uuid}) pass the chokepoint for ANY
// authenticated user (RequireCap resolves serverID==0 -> ownerSelf). The
// handler's own owner_user_id scoping (Phase 3) is the real per-realm
// boundary; the delegation-cap airtightness for third-party grants is
// already covered by server_grants_test.go (TestAssignGrant_*), untouched
// here.
func TestCap_ServerRolesOwnerScopedChokepointOpen(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("someuser-id", "someuser", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/server-roles", testIdentity{UserID: "someuser-id", Username: "someuser"}); c == 403 {
		t.Error("OWNER-scope /server-roles must NOT 403 at the chokepoint for an authed user (ownerSelf); handler scopes")
	}
}

// TestCap_GrantsOwnerScopedChokepointOpen mirrors the above for /grants:
// members.write/delete are SERVER-scope and would 403 unconditionally on a
// path with no {id}/{uuid} to resolve a server from, so the route is gated
// with the OWNER-scope roles.write/roles.delete instead. Prove an ordinary
// authenticated user is not denied at the chokepoint (the handler's own
// members.write/delete delegation check against the TARGET server, plus the
// owner-realm scoping, remain the real boundary and are untouched).
func TestCap_GrantsOwnerScopedChokepointOpen(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("someuser-id", "someuser", false)
	srv := newAuthzTestServer(t, fs)
	if c := doJSON(t, srv, "POST", "/api/grants", `{"username":"someuser"}`, testIdentity{UserID: "someuser-id", Username: "someuser"}); c == 403 {
		t.Error("OWNER-scope /grants POST must NOT 403 at the chokepoint for an authed user (ownerSelf); handler scopes")
	}
	if c := doJSON(t, srv, "DELETE", "/api/grants", `{"username":"someuser"}`, testIdentity{UserID: "someuser-id", Username: "someuser"}); c == 403 {
		t.Error("OWNER-scope /grants DELETE must NOT 403 at the chokepoint for an authed user (ownerSelf); handler scopes")
	}
}

// TestCap_StoragePathIsNotOverviewRead: the node-wide storage answer takes the
// same cap as the migration it feeds, not the cap every invite carries.
func TestCap_StoragePathIsNotOverviewRead(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("viewer-id", "viewer", false)
	fs.servers = map[int]*models.Server{11: {ID: 11, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{44: {ID: 44, Capabilities: []string{"overview.read"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(11, "viewer-id"): {UserID: "viewer-id", ServerRoleID: intPtr(44)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/11/storage-path", testIdentity{UserID: "viewer-id", Username: "viewer"}); c != 403 {
		t.Errorf("overview.read holder must be 403 on storage-path (needs server.settings.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/servers/11/storage-path", testIdentity{UserID: "owner-id", Username: "owner"}); c == 403 {
		t.Error("the owner must reach storage-path on their own server")
	}
}

func TestCap_AuditForceNeedsSettingsWrite(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("viewer-id", "viewer", false)
	fs.servers = map[int]*models.Server{10: {ID: 10, OwnerID: "owner-id", OwnerName: "owner"}}
	// viewer holds overview.read only - neither viewing nor forcing the audit
	fs.serverRoles = map[int]*store.ServerRole{41: {ID: 41, Capabilities: []string{"overview.read"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(10, "viewer-id"): {UserID: "viewer-id", ServerRoleID: intPtr(41)}}
	srv := newAuthzTestServer(t, fs)
	// owner CAN force (settings.write via ownership short-circuit)
	if c := doAs(t, srv, "PUT", "/api/servers/10/audit/force", testIdentity{UserID: "owner-id", Username: "owner"}); c == 403 {
		t.Error("owner must be able to force their own server audit")
	}
	if c := doAs(t, srv, "PUT", "/api/servers/10/audit/force", testIdentity{UserID: "viewer-id", Username: "viewer"}); c != 403 {
		t.Errorf("overview.read-only holder must be 403 on audit/force (needs server.settings.write), got %d", c)
	}
}

// TestCap_AuditViewIsNotOverviewRead pins the fix: the per-server audit log is
// gated on its own server.audit.read, not on overview.read.
//
// This assertion is inverted from what it used to be ("overview.read holder
// must view the audit log"). overview.read is the cap every invite carries, so
// the old gate let each invited member read the record of what all the OTHER
// members did - including the IP address and user agent of the owner and of
// each of them. The panel greyed the tab out for non-owners and its comment
// claimed that was the rule; only the UI enforced it.
func TestCap_AuditViewIsNotOverviewRead(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("member-id", "member", false)
	fs.addUser("auditor-id", "auditor", false)
	fs.servers = map[int]*models.Server{10: {ID: 10, OwnerID: "owner-id", OwnerName: "owner"}}
	// A "Server admin" grant: every cap the full-access preset hands out. Even
	// that must not reach the audit log - the preset gives a friend the server,
	// not the log of what that friend did and from which address.
	fullAccess := []string{}
	for _, p := range authz.Presets() {
		if p.ID == "admin" {
			fullAccess = append(fullAccess, p.Capabilities...)
		}
	}
	if len(fullAccess) == 0 {
		t.Fatal("full-access preset not found; the assertion below would be vacuous")
	}
	fs.serverRoles = map[int]*store.ServerRole{
		42: {ID: 42, Capabilities: fullAccess},
		43: {ID: 43, Capabilities: []string{"server.audit.read"}},
	}
	fs.serverGrants = map[string]*store.ServerGrant{
		skey(10, "member-id"):  {UserID: "member-id", ServerRoleID: intPtr(42)},
		skey(10, "auditor-id"): {UserID: "auditor-id", ServerRoleID: intPtr(43)},
	}
	srv := newAuthzTestServer(t, fs)

	for _, path := range []string{"/api/servers/10/audit", "/api/servers/10/audit/status"} {
		if c := doAs(t, srv, "GET", path, testIdentity{UserID: "member-id", Username: "member"}); c != 403 {
			t.Errorf("%s: full-access member must be 403 without server.audit.read, got %d", path, c)
		}
		if c := doAs(t, srv, "GET", path, testIdentity{UserID: "owner-id", Username: "owner"}); c == 403 {
			t.Errorf("%s: the owner must read their own server's audit log", path)
		}
		// Delegation still works when the owner asks for it explicitly.
		if c := doAs(t, srv, "GET", path, testIdentity{UserID: "auditor-id", Username: "auditor"}); c == 403 {
			t.Errorf("%s: an explicit server.audit.read holder must pass", path)
		}
	}
}

// --- Phase 4 Task 10: backups ---

// TestCap_BackupStoragesPanel proves backup-storages is now a PANEL
// settings.read/write route (not the old in-handler admin-only gate):
// admin passes via the resolver's admin short-circuit; an ordinary
// authenticated user holding no panel role/cap is 403.
func TestCap_BackupStoragesPanel(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	fs.addUser("user-id", "user", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/backup-storages", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must reach backup-storages")
	}
	if c := doAs(t, srv, "GET", "/api/backup-storages", testIdentity{UserID: "user-id", Username: "user"}); c != 403 {
		t.Errorf("ordinary user must be 403 on PANEL backup-storages, got %d", c)
	}
}

// TestCap_BackupJobsServerScoped proves /servers/{id}/backup-jobs is gated
// with the fine per-method cap: a backups.read-only grant can list jobs but
// not create one (needs backups.create).
func TestCap_BackupJobsServerScoped(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	fs.addUser("bk-id", "bk", false)
	fs.servers = map[int]*models.Server{14: {ID: 14, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{60: {ID: 60, Capabilities: []string{"backups.read"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(14, "bk-id"): {UserID: "bk-id", ServerRoleID: intPtr(60)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/14/backup-jobs", testIdentity{UserID: "bk-id", Username: "bk"}); c == 403 {
		t.Error("backups.read holder must list backup jobs")
	}
	if c := doAs(t, srv, "POST", "/api/servers/14/backup-jobs", testIdentity{UserID: "bk-id", Username: "bk"}); c != 403 {
		t.Errorf("backups.read holder must NOT create backup jobs (needs backups.create), got %d", c)
	}
}

// --- Phase 4 Task 11: per-server network routes + link-routes (EXEMPT) ---

// TestCap_ServerRoutesNetworkVerbs proves /servers/{id}/routes is gated with
// the fine per-method cap: a network.read-only grant can list routes but not
// create one (needs network.write).
func TestCap_ServerRoutesNetworkVerbs(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	fs.addUser("owner-id", "owner", false)
	fs.addUser("net-id", "net", false)
	fs.addUser("stranger-id", "stranger", false)
	fs.servers = map[int]*models.Server{16: {ID: 16, OwnerID: "owner-id", OwnerName: "owner"}}
	fs.serverRoles = map[int]*store.ServerRole{70: {ID: 70, Capabilities: []string{"network.read"}}}
	fs.serverGrants = map[string]*store.ServerGrant{skey(16, "net-id"): {UserID: "net-id", ServerRoleID: intPtr(70)}}
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/servers/16/routes", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must not be 403 on server routes")
	}
	if c := doAs(t, srv, "GET", "/api/servers/16/routes", testIdentity{UserID: "net-id", Username: "net"}); c == 403 {
		t.Error("network.read holder must list server routes")
	}
	if c := doAs(t, srv, "POST", "/api/servers/16/routes", testIdentity{UserID: "net-id", Username: "net"}); c != 403 {
		t.Errorf("network.read holder must NOT create routes (needs network.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/servers/16/routes", testIdentity{UserID: "stranger-id", Username: "stranger"}); c != 403 {
		t.Errorf("unprivileged user must be 403 on server routes, got %d", c)
	}
}

// TestCap_LinkRoutesExemptAuthed proves /gateway/link-routes is EXEMPT-authed:
// any authed user passes the chokepoint (no RequireCap); the in-handler owner
// filter is the boundary, not tested here.
func TestCap_LinkRoutesExemptAuthed(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("someuser-id", "someuser", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/gateway/link-routes", testIdentity{UserID: "someuser-id", Username: "someuser"}); c == 403 {
		t.Error("EXEMPT-authed /gateway/link-routes must not 403 at a chokepoint (handler owner-filters)")
	}
}

// --- Phase 4 Task 12: users + roles + panel-roles + admin 2FA reset ---

// TestCap_UsersPanelReadVsWrite proves /users is gated with PANEL
// users.read/write: admin passes via the short-circuit, a panel-role holder
// of users.read only can list but not create, and an ordinary authenticated
// user without any panel cap is 403.
func TestCap_UsersPanelReadVsWrite(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	panelHolder(fs, "sup-id", "sup", "users.read") // support-like: read only
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/users", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must list users")
	}
	if c := doAs(t, srv, "GET", "/api/users", testIdentity{UserID: "sup-id", Username: "sup"}); c == 403 {
		t.Error("users.read panel-role holder must list users")
	}
	if c := doAs(t, srv, "POST", "/api/users", testIdentity{UserID: "sup-id", Username: "sup"}); c != 403 {
		t.Errorf("users.read-only holder must be 403 on POST /users (needs users.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/users", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on PANEL users list, got %d", c)
	}
}

// TestCap_UserDeleteNeedsUsersDelete proves DELETE /users/{id} needs the
// finer users.delete cap: a users.write-only holder (e.g. can reset
// passwords/roles) must NOT be able to delete a user.
func TestCap_UserDeleteNeedsUsersDelete(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	panelHolder(fs, "writer-id", "writer", "users.write")
	fs.addUser("target-id", "target", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "DELETE", "/api/users/11111111-2222-3333-4444-555555555555", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must not be 403 on user delete")
	}
	if c := doAs(t, srv, "DELETE", "/api/users/11111111-2222-3333-4444-555555555555", testIdentity{UserID: "writer-id", Username: "writer"}); c != 403 {
		t.Errorf("users.write-only holder must be 403 on user delete (needs users.delete), got %d", c)
	}
}

// TestCap_PanelRolesReadVsWriteVsDelete proves /admin/panel-roles is gated
// with PANEL panelroles.read/write/delete: admin passes, a panelroles.read
// holder can list but not create/delete, and an ordinary user is 403. This is
// also where the KNOWN, ACCEPTED escalation lives: panel-role CRUD has no
// delegation/subset check, so a panelroles.write holder (not exercised here,
// since it is out of scope to "fix") could grant itself every panel cap. Safe
// by default because only admin holds panelroles.write in the seed roles.
func TestCap_PanelRolesReadVsWriteVsDelete(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	panelHolder(fs, "reader-id", "reader", "panelroles.read")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/admin/panel-roles", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must list panel roles")
	}
	if c := doAs(t, srv, "GET", "/api/admin/panel-roles", testIdentity{UserID: "reader-id", Username: "reader"}); c == 403 {
		t.Error("panelroles.read holder must list panel roles")
	}
	if c := doJSON(t, srv, "POST", "/api/admin/panel-roles", `{"name":"x","capabilities":[]}`, testIdentity{UserID: "reader-id", Username: "reader"}); c != 403 {
		t.Errorf("panelroles.read-only holder must be 403 on create (needs panelroles.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/admin/panel-roles", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on PANEL panel-roles list, got %d", c)
	}
}

// TestCap_SetUserPanelRoleNeedsPanelRolesWrite proves PUT
// /admin/users/{id}/panel-role (the per-user panel-role assignment route) is
// gated with PANEL panelroles.write, not users.write: admin passes; a
// users.write holder (can edit users generally) is NOT enough and must be
// 403; a panelroles.write holder passes. This is the exact route the
// escalation note above is about (a panelroles.write holder could assign
// itself/another user an admin-equivalent panel role) - accepted as safe-by-
// default since only admin holds panelroles.write in the seed roles.
func TestCap_SetUserPanelRoleNeedsPanelRolesWrite(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	panelHolder(fs, "userwriter-id", "userwriter", "users.write")
	panelHolder(fs, "rolewriter-id", "rolewriter", "panelroles.write")
	srv := newAuthzTestServer(t, fs)
	body := `{"panelRoleId":null}`
	if c := doJSON(t, srv, "PUT", "/api/admin/users/11111111-2222-3333-4444-555555555555/panel-role", body, testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must not be 403 assigning a panel role")
	}
	if c := doJSON(t, srv, "PUT", "/api/admin/users/11111111-2222-3333-4444-555555555555/panel-role", body, testIdentity{UserID: "userwriter-id", Username: "userwriter"}); c != 403 {
		t.Errorf("users.write-only holder must be 403 on panel-role assignment (needs panelroles.write), got %d", c)
	}
	if c := doJSON(t, srv, "PUT", "/api/admin/users/11111111-2222-3333-4444-555555555555/panel-role", body, testIdentity{UserID: "rolewriter-id", Username: "rolewriter"}); c == 403 {
		t.Error("panelroles.write holder must not be 403 on panel-role assignment")
	}
}

// TestCap_AdminResetTOTPNeedsUsersWrite proves DELETE /users/{id}/2fa is
// gated with PANEL users.write: admin passes, an ordinary authenticated user
// is 403.
func TestCap_AdminResetTOTPNeedsUsersWrite(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "DELETE", "/api/users/11111111-2222-3333-4444-555555555555/2fa", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must not be 403 resetting a user's 2FA")
	}
	if c := doAs(t, srv, "DELETE", "/api/users/11111111-2222-3333-4444-555555555555/2fa", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on admin 2FA reset (needs users.write), got %d", c)
	}
}

// TestMeUsernameHistory_ExemptAuthed proves /me/username-history stays
// EXEMPT-authed (own history, not RequireCap'd): any authenticated user
// passes the chokepoint.
func TestMeUsernameHistory_ExemptAuthed(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/me/username-history", testIdentity{UserID: "plain-id", Username: "plain"}); c == 403 {
		t.Error("EXEMPT-authed /me/username-history must not 403 for any authed user")
	}
}

// --- Phase 4 Task 13: nodes / admission / enroll / placement / admin servers ---

// TestCap_NodesMutationsPanel proves POST /nodes is gated PANEL nodes.write:
// admin passes via the short-circuit, a nodes.write panel-role holder passes,
// an ordinary authenticated user is 403.
func TestCap_NodesMutationsPanel(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	panelHolder(fs, "nodeop-id", "nodeop", "nodes.read", "nodes.write")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "POST", "/api/nodes", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must create nodes")
	}
	if c := doAs(t, srv, "POST", "/api/nodes", testIdentity{UserID: "nodeop-id", Username: "nodeop"}); c == 403 {
		t.Error("nodes.write panel-role holder must create nodes")
	}
	if c := doAs(t, srv, "POST", "/api/nodes", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on POST /nodes, got %d", c)
	}
}

// TestCap_AdminServersPanel proves GET /admin/servers is gated PANEL
// servers.read: a servers.read panel-role holder passes, an ordinary
// authenticated user is 403.
func TestCap_AdminServersPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "sv-id", "sv", "servers.read")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/admin/servers", testIdentity{UserID: "sv-id", Username: "sv"}); c == 403 {
		t.Error("servers.read holder must reach admin servers oversight")
	}
	if c := doAs(t, srv, "GET", "/api/admin/servers", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on admin servers, got %d", c)
	}
}

// TestCap_NodesListExemptAuthed proves GET /nodes is EXEMPT-authed (owner-
// inclusive filter): an authed user must NOT be 403 at a chokepoint - the
// admin-vs-BYON-owner filter inside GetNodes is the boundary, gating this
// PANEL nodes.read would regress a BYON owner out of their own node list.
func TestCap_NodesListExemptAuthed(t *testing.T) {
	fs := &authzFakeStore{}
	// GetNodes stays admin-only outside BYON mode (pre-existing business rule,
	// not the chokepoint under test); enable BYON so the owner-inclusive filter
	// is actually reachable for a non-admin caller.
	fs.settings = map[string]string{"feature_byon_enabled": "true"}
	fs.addUser("byon-id", "byon", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/nodes", testIdentity{UserID: "byon-id", Username: "byon"}); c == 403 {
		t.Error("EXEMPT-authed GET /nodes must not 403 an authed user (in-handler owner filter is the boundary)")
	}
}

// TestCap_NodeInspectReadsExemptAuthed proves the 4 INSPECT node reads
// (servers/storage/deploy-bundle/cpu) stay authed-exempt at the chokepoint:
// each already carries its own canManageNode admin-vs-BYON-owner data filter
// (cpu additionally allows a CanChangeResources grant), so none is un-gated.
func TestCap_NodeInspectReadsExemptAuthed(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("byon-id", "byon", false)
	srv := newAuthzTestServer(t, fs)
	for _, path := range []string{
		"/api/nodes/1/servers",
		"/api/nodes/1/storage",
		"/api/nodes/1/deploy-bundle",
		"/api/nodes/1/cpu",
	} {
		if c := doAs(t, srv, "GET", path, testIdentity{UserID: "byon-id", Username: "byon"}); c == 403 {
			t.Errorf("EXEMPT-authed %s must not 403 an authed user at the chokepoint (canManageNode is the boundary), got 403", path)
		}
	}
}

// TestCap_NodeAdmissionPanel proves /admin/settings/node-admission is gated
// PANEL nodes.read/nodes.write: a nodes.read-only holder can read the
// admission config but not write it.
func TestCap_NodeAdmissionPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "reader-id", "reader", "nodes.read")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/admin/settings/node-admission", testIdentity{UserID: "reader-id", Username: "reader"}); c == 403 {
		t.Error("nodes.read holder must read node admission config")
	}
	if c := doAs(t, srv, "PUT", "/api/admin/settings/node-admission", testIdentity{UserID: "reader-id", Username: "reader"}); c != 403 {
		t.Errorf("nodes.read-only holder must NOT write admission config (needs nodes.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/admin/settings/node-admission", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on admin node-admission read, got %d", c)
	}
}

// TestCap_NodeAdmissionCIDRsPanel proves POST /admin/settings/node-admission/cidrs
// needs nodes.write: a nodes.read-only holder is 403, a nodes.write holder passes.
func TestCap_NodeAdmissionCIDRsPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "reader-id", "reader", "nodes.read")
	panelHolder(fs, "writer-id", "writer", "nodes.read", "nodes.write")
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "POST", "/api/admin/settings/node-admission/cidrs", testIdentity{UserID: "reader-id", Username: "reader"}); c != 403 {
		t.Errorf("nodes.read-only holder must NOT add a CIDR (needs nodes.write), got %d", c)
	}
	if c := doAs(t, srv, "POST", "/api/admin/settings/node-admission/cidrs", testIdentity{UserID: "writer-id", Username: "writer"}); c == 403 {
		t.Error("nodes.write holder must add a CIDR")
	}
}

// TestCap_RollSecretNeedsNodesWrite proves POST /admin/nodes/{id}/roll-secret is
// gated the way reset-pairing is: it revokes a node's secret, so reading nodes
// is not enough.
func TestCap_RollSecretNeedsNodesWrite(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "reader-id", "reader", "nodes.read")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "POST", "/api/admin/nodes/5/roll-secret", testIdentity{UserID: "reader-id", Username: "reader"}); c != 403 {
		t.Errorf("nodes.read-only holder must NOT roll a node's key (needs nodes.write), got %d", c)
	}
	if c := doAs(t, srv, "POST", "/api/admin/nodes/5/roll-secret", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on roll-secret, got %d", c)
	}
}

// TestCap_DiskOrphansPanel proves the disk-orphans browse routes are gated
// PANEL nodes.read (browse) vs nodes.write (assign).
func TestCap_DiskOrphansPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "reader-id", "reader", "nodes.read")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/disk/orphans/1/some-uuid/files", testIdentity{UserID: "reader-id", Username: "reader"}); c == 403 {
		t.Error("nodes.read holder must browse orphan files")
	}
	if c := doAs(t, srv, "POST", "/api/disk/orphans/assign", testIdentity{UserID: "reader-id", Username: "reader"}); c != 403 {
		t.Errorf("nodes.read-only holder must NOT assign an orphan (needs nodes.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/disk/orphans/1/some-uuid/files", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on orphan browse, got %d", c)
	}
}

// TestCap_EnrollTokenAndPlacementHelpers_ExemptAuthed proves the BYON
// enroll-token self-service routes and the placement helper routes stay
// authed-exempt: any authenticated user reaches them (feature-gating/BYON
// scoping happens in-handler, not at the chokepoint). BYON is enabled so the
// enroll-token routes' own in-handler feature-gate (byonActive) does not mask
// the chokepoint assertion under test.
func TestCap_EnrollTokenAndPlacementHelpers_ExemptAuthed(t *testing.T) {
	fs := &authzFakeStore{}
	fs.settings = map[string]string{"feature_byon_enabled": "true"}
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/nodes/enroll-token"},
		{"POST", "/api/placement/pick"},
		{"GET", "/api/placement/tags"},
		{"GET", "/api/placement/regions"},
	} {
		if c := doAs(t, srv, tc.method, tc.path, testIdentity{UserID: "plain-id", Username: "plain"}); c == 403 {
			t.Errorf("EXEMPT-authed %s %s must not 403 an authed user, got 403", tc.method, tc.path)
		}
	}
}

// TestCap_RegionsPanel proves /admin/regions is gated PANEL regions.read
// (list) vs regions.write (create): a regions.read holder can list but not
// create, and an ordinary user is 403.
func TestCap_RegionsPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "rr-id", "rr", "regions.read")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/admin/regions", testIdentity{UserID: "rr-id", Username: "rr"}); c == 403 {
		t.Error("regions.read holder must list regions")
	}
	if c := doAs(t, srv, "POST", "/api/admin/regions", testIdentity{UserID: "rr-id", Username: "rr"}); c != 403 {
		t.Errorf("regions.read-only holder must be 403 on POST (needs regions.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/admin/regions", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on admin regions, got %d", c)
	}
}

// TestCap_TicketAdminPanel proves admin ticket-category management is gated
// PANEL tickets.read (list) vs tickets.write (mutate) vs tickets.delete
// (delete): a tickets.read/write holder reaches the admin category list, an
// ordinary user is 403, and a tickets.read-only holder cannot delete a
// category (that needs tickets.delete).
func TestCap_TicketAdminPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "sup-id", "sup", "tickets.read", "tickets.write")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/admin/ticket-categories", testIdentity{UserID: "sup-id", Username: "sup"}); c == 403 {
		t.Error("tickets.read/write holder must reach admin ticket-categories")
	}
	if c := doAs(t, srv, "GET", "/api/admin/ticket-categories", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on admin ticket-categories, got %d", c)
	}
	// tickets.read-only holder cannot delete a category (needs tickets.delete)
	panelHolder(fs, "tr-id", "tr", "tickets.read")
	if c := doAs(t, srv, "DELETE", "/api/admin/ticket-categories/1", testIdentity{UserID: "tr-id", Username: "tr"}); c != 403 {
		t.Errorf("tickets.read-only holder must be 403 on category delete (needs tickets.delete), got %d", c)
	}
}

// TestCap_TicketsUserRouteExemptAuthed proves the user-facing /tickets route
// is authed-exempt (in-handler owner/watcher/support ACL is the boundary,
// untouched by this batch): an authed user must not be 403 at the chokepoint.
func TestCap_TicketsUserRouteExemptAuthed(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("u-id", "u", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/tickets", testIdentity{UserID: "u-id", Username: "u"}); c == 403 {
		t.Error("EXEMPT-authed GET /tickets must not 403 an authed user (in-handler ACL is the boundary)")
	}
}

// TestCap_BillingPanel proves the admin billing/usage/limit-override routes are
// gated by PANEL plans.* at the chokepoint (read for GET, write for PATCH),
// while the caller's own /me/usage stays authed-exempt (not RequireCap-gated).
//
// Plan CRUD used to be checked here too. Those routes are gone: nothing sold a
// plan, and the capability that guarded them still guards what replaced them.
func TestCap_BillingPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "bl-id", "bl", "plans.read")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/admin/settings/billing", testIdentity{UserID: "bl-id", Username: "bl"}); c == 403 {
		t.Error("plans.read holder must read billing settings")
	}
	if c := doAs(t, srv, "PUT", "/api/admin/settings/billing", testIdentity{UserID: "bl-id", Username: "bl"}); c != 403 {
		t.Errorf("plans.read-only holder must be 403 on PUT billing settings (needs plans.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/admin/settings/billing", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on admin billing settings, got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/me/usage", testIdentity{UserID: "plain-id", Username: "plain"}); c == 403 {
		t.Error("EXEMPT-authed /me/usage must not 403 an authed user")
	}
}

// The removed plan routes must stay removed: a 404 here rather than a 403 is
// what proves the surface is gone, not merely guarded.
func TestPlanRoutesAreGone(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("admin-id", "adm", true)
	srv := newAuthzTestServer(t, fs)
	for _, path := range []string{"/api/admin/plans", "/api/settings/gateway/hub-redis-admin"} {
		if c := doAs(t, srv, "GET", path, testIdentity{UserID: "admin-id", Username: "adm", IsAdmin: true}); c != 404 {
			t.Errorf("GET %s = %d, want 404 (the route should not exist)", path, c)
		}
	}
}

// TestCap_PlatformSettingsPanel proves the admin platform-settings surface
// (Phase 4 Task 17 - here auth policy + SMTP) is gated by PANEL settings.read/
// write at the chokepoint, the former IsAdmin gate in the handler is gone
// (settings.read alone, no admin flag, must pass), and the public maintenance-
// banner state stays reachable.
func TestCap_PlatformSettingsPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "st-id", "st", "settings.read")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/admin/settings/auth", testIdentity{UserID: "st-id", Username: "st"}); c == 403 {
		t.Error("settings.read holder must read admin auth settings")
	}
	if c := doAs(t, srv, "PUT", "/api/admin/settings/auth", testIdentity{UserID: "st-id", Username: "st"}); c != 403 {
		t.Errorf("settings.read-only holder must be 403 on settings write (needs settings.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/admin/settings/auth", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on admin settings, got %d", c)
	}
	// public maintenance banner must stay reachable
	if c := doAs(t, srv, "GET", "/api/maintenance", testIdentity{UserID: "plain-id", Username: "plain"}); c == 403 {
		t.Error("public /api/maintenance must not be 403")
	}
}

// TestCap_PlatformSettingsWriteSurfacePanel spot-checks the settings.write side
// of Task 17 across three DIFFERENT handler families (db migration, XDP config,
// demo-server flag) to prove the uniform read/write mapping actually reaches
// every cluster in this batch, not just the auth/SMTP pair above. RequireCap
// runs before the handler body, so a settings.read-only holder must be denied
// even though the handler itself (nil DBMigration/Redis in this fake harness)
// would otherwise 404/500 - only a 403 means the chokepoint itself denied.
func TestCap_PlatformSettingsWriteSurfacePanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "rw-id", "rw", "settings.read", "settings.write")
	panelHolder(fs, "ro-id", "ro", "settings.read")
	srv := newAuthzTestServer(t, fs)

	if c := doAs(t, srv, "GET", "/api/admin/db/migration", testIdentity{UserID: "ro-id", Username: "ro"}); c == 403 {
		t.Error("settings.read holder must read db migration status")
	}
	if c := doAs(t, srv, "POST", "/api/admin/db/migration", testIdentity{UserID: "ro-id", Username: "ro"}); c != 403 {
		t.Errorf("settings.read-only holder must be 403 starting a db migration, got %d", c)
	}
	if c := doAs(t, srv, "POST", "/api/admin/db/migration", testIdentity{UserID: "rw-id", Username: "rw"}); c == 403 {
		t.Error("settings.write holder must not be 403 starting a db migration")
	}
	if c := doAs(t, srv, "GET", "/api/admin/xdp/config", testIdentity{UserID: "ro-id", Username: "ro"}); c == 403 {
		t.Error("settings.read holder must read XDP config")
	}
	if c := doAs(t, srv, "PUT", "/api/admin/xdp/config", testIdentity{UserID: "ro-id", Username: "ro"}); c != 403 {
		t.Errorf("settings.read-only holder must be 403 saving XDP config, got %d", c)
	}
	if c := doAs(t, srv, "PATCH", "/api/admin/servers/1/demo", testIdentity{UserID: "ro-id", Username: "ro"}); c != 403 {
		t.Errorf("settings.read-only holder must be 403 flagging a demo server, got %d", c)
	}
}

// TestCap_SettingsExemptAuthedGETsNotBlocked proves the settings reads meant
// for EVERY authenticated user (not just settings.read holders) stay reachable
// at the chokepoint: GET /settings/features, GET /settings/beam and GET
// /settings/routing-mode each share their path template with an admin-only
// settings.write POST/save (see the routes.go comments - same split-template
// pattern as GET /nodes), plus the two long-standing EXEMPT helper GETs
// (/gateway/route-options, /settings/filemanager/limits).
func TestCap_SettingsExemptAuthedGETsNotBlocked(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	id := testIdentity{UserID: "plain-id", Username: "plain"}
	for _, path := range []string{
		"/api/settings/features",
		"/api/settings/beam",
		"/api/settings/routing-mode",
		"/api/gateway/route-options",
		"/api/settings/filemanager/limits",
	} {
		if c := doAs(t, srv, "GET", path, id); c == 403 {
			t.Errorf("EXEMPT-authed GET %s must not 403 an ordinary authed user", path)
		}
	}
}

// --- Phase 4 Task 18: gateway/warp topology + dns + infrastructure oversight ---

// TestCap_GatewayTopologyPanel proves the global gateway oversight surface
// (links/edges/routes/logs/stats/errors/dns-check/sync) is gated with PANEL
// topology.read/topology.write: a topology.read holder can view but not
// sync, and an ordinary authenticated user is denied outright.
func TestCap_GatewayTopologyPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "tp-id", "tp", "topology.read")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/gateway/links", testIdentity{UserID: "tp-id", Username: "tp"}); c == 403 {
		t.Error("topology.read holder must view gateway links")
	}
	if c := doAs(t, srv, "POST", "/api/gateway/sync", testIdentity{UserID: "tp-id", Username: "tp"}); c != 403 {
		t.Errorf("topology.read-only holder must be 403 on gateway sync (needs topology.write), got %d", c)
	}
	if c := doAs(t, srv, "GET", "/api/gateway/links", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on gateway topology, got %d", c)
	}
}

// TestCap_WarpRegionsTopologyPanel proves the warp regions/leaders/key-mint
// admin registry shares the same topology.read/topology.write split as the
// gateway surface above.
func TestCap_WarpRegionsTopologyPanel(t *testing.T) {
	fs := &authzFakeStore{}
	panelHolder(fs, "tp-id", "tp", "topology.read")
	panelHolder(fs, "tw-id", "tw", "topology.read", "topology.write")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/warp/regions", testIdentity{UserID: "tp-id", Username: "tp"}); c == 403 {
		t.Error("topology.read holder must list warp regions")
	}
	if c := doAs(t, srv, "POST", "/api/warp/regions", testIdentity{UserID: "tp-id", Username: "tp"}); c != 403 {
		t.Errorf("topology.read-only holder must be 403 upserting a warp region (needs topology.write), got %d", c)
	}
	if c := doAs(t, srv, "POST", "/api/warp/regions", testIdentity{UserID: "tw-id", Username: "tw"}); c == 403 {
		t.Error("topology.write holder must not be 403 upserting a warp region")
	}
	if c := doAs(t, srv, "POST", "/api/admin/warp/keys", testIdentity{UserID: "tw-id", Username: "tw"}); c == 403 {
		t.Error("topology.write holder must not be 403 minting a warp key")
	}
	if c := doAs(t, srv, "GET", "/api/warp/regions", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on warp regions, got %d", c)
	}
}

// TestCap_WarpLinkKitsExemptAuthed proves /warp/link-kits is EXEMPT-authed
// (tenant self-service): the in-handler owner filter + BYON gate is the
// boundary, not RequireCap - any authed user must pass the chokepoint.
func TestCap_WarpLinkKitsExemptAuthed(t *testing.T) {
	fs := &authzFakeStore{}
	// ListLinkKits has its own in-handler BYON gate (byonActive); enable BYON so
	// that gate doesn't mask the chokepoint result under test.
	fs.settings = map[string]string{"feature_byon_enabled": "true"}
	fs.addUser("byon-id", "byon", false)
	srv := newAuthzTestServer(t, fs)
	if c := doAs(t, srv, "GET", "/api/warp/link-kits", testIdentity{UserID: "byon-id", Username: "byon"}); c == 403 {
		t.Error("EXEMPT-authed /warp/link-kits must not 403 an authed user (in-handler owner filter is the boundary)")
	}
}

// TestCap_ModuleMutationsPanel proves module create/toggle/position/role
// RequireCap("settings.write") (there is no dedicated modules.* cap), while
// GET /modules stays EXEMPT-authed for every authenticated user (Phase 4
// Task 19).
func TestCap_ModuleMutationsPanel(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	panelHolder(fs, "mod-id", "mod", "settings.write")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	// GET /modules is EXEMPT-authed: a plain user must not be 403 at a chokepoint
	if c := doAs(t, srv, "GET", "/api/modules", testIdentity{UserID: "plain-id", Username: "plain"}); c == 403 {
		t.Error("EXEMPT-authed GET /modules must not 403 an authed user")
	}
	// admin must not be 403 toggling a module
	if c := doAs(t, srv, "PATCH", "/api/modules/1/toggle", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must not be 403 on module toggle")
	}
	// a settings.write holder can
	if c := doAs(t, srv, "PATCH", "/api/modules/1/toggle", testIdentity{UserID: "mod-id", Username: "mod"}); c == 403 {
		t.Error("settings.write holder must reach module toggle")
	}
	// a plain user cannot mutate modules
	if c := doAs(t, srv, "PATCH", "/api/modules/1/toggle", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on module toggle (needs settings.write), got %d", c)
	}
}

// TestCap_ModpackSolderOwnerChokepointOpen documents the F2 design for Phase 4
// Task 20's OWNER-scope modpack.* routes: the modpack builder (/me/packs,
// /packs/{id}/...) and the session-authed solder client/key management
// (/solder/clients, /solder/keys, /packs/{id}/clients) all carry no server
// {id}/{uuid}, so RequireCap resolves serverID==0 -> ownerSelf and passes the
// chokepoint for ANY authenticated user. The real per-realm boundary is
// packsHandler.ownsPack / solder_manage.go's solderCaller+ownsPackAndClient
// (both keyed on owner_user_id), which is untouched by this batch.
func TestCap_ModpackSolderOwnerChokepointOpen(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	srv := newAuthzTestServer(t, fs)
	id := testIdentity{UserID: "owner-id", Username: "owner"}
	if c := doAs(t, srv, "GET", "/api/me/packs", id); c == 403 {
		t.Error("OWNER modpack.read must not 403 at the chokepoint for an authed user (handler scopes)")
	}
	if c := doAs(t, srv, "POST", "/api/me/packs", id); c == 403 {
		t.Error("OWNER modpack.write must not 403 at the chokepoint for an authed user (handler scopes)")
	}
	if c := doAs(t, srv, "GET", "/api/me/modrinth-pat", id); c == 403 {
		t.Error("OWNER modpack.read (modrinth PAT) must not 403 at the chokepoint for an authed user")
	}
	if c := doAs(t, srv, "GET", "/api/solder/clients", id); c == 403 {
		t.Error("OWNER modpack.read (solder clients, session-authed) must not 403 at the chokepoint for an authed user")
	}
	if c := doAs(t, srv, "GET", "/api/solder/keys", id); c == 403 {
		t.Error("OWNER modpack.read (solder keys, session-authed) must not 403 at the chokepoint for an authed user")
	}
}

// TestCap_LibraryContentPanel proves Phase 4 Task 20's /library/* CONTENT
// classification: GET /library (and its /download sibling) stay EXEMPT-authed
// (any authenticated user may browse/download the platform-shared library, as
// today - see LibraryPicker.tsx), while the four mutations (mkdir/upload/
// delete/toggle) now RequireCap("settings.write") in place of the former
// in-handler `if !isAdmin` block - there is no dedicated library-admin cap, so
// this reuses the same PANEL settings.write mapping as Task 19's /modules
// mutations. This is a PANEL (not OWNER library.*) classification because the
// library is a single platform-shared catalog, not a per-user owned realm.
func TestCap_LibraryContentPanel(t *testing.T) {
	fs := &authzFakeStore{}
	admin := fs.addUser("admin-id", "root", true)
	panelHolder(fs, "lib-id", "lib", "settings.write")
	fs.addUser("plain-id", "plain", false)
	srv := newAuthzTestServer(t, fs)
	// GET /library is EXEMPT-authed: a plain user must not be 403 at the chokepoint
	if c := doAs(t, srv, "GET", "/api/library", testIdentity{UserID: "plain-id", Username: "plain"}); c == 403 {
		t.Error("EXEMPT-authed GET /library must not 403 an authed user")
	}
	if c := doAs(t, srv, "GET", "/api/library/download", testIdentity{UserID: "plain-id", Username: "plain"}); c == 403 {
		t.Error("EXEMPT-authed GET /library/download must not 403 an authed user")
	}
	// admin must not be 403 on a library mutation
	if c := doAs(t, srv, "POST", "/api/library/mkdir", testIdentity{UserID: admin.ID, Username: "root", IsAdmin: true}); c == 403 {
		t.Error("admin must not be 403 on library mkdir")
	}
	// a settings.write holder can too
	if c := doAs(t, srv, "POST", "/api/library/mkdir", testIdentity{UserID: "lib-id", Username: "lib"}); c == 403 {
		t.Error("settings.write holder must reach library mkdir")
	}
	// a plain user cannot mutate the library
	if c := doAs(t, srv, "POST", "/api/library/mkdir", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on library mkdir (needs settings.write), got %d", c)
	}
	if c := doAs(t, srv, "POST", "/api/library/delete", testIdentity{UserID: "plain-id", Username: "plain"}); c != 403 {
		t.Errorf("ordinary user must be 403 on library delete (needs settings.write), got %d", c)
	}
}

// TestCap_ApiKeysOwner proves Phase 4 Task 21's /me/api-keys classification:
// OWNER apikeys.read/write/delete is chokepoint-open for any authed user (the
// path carries no server {id}), with the handler's own per-user scoping (a
// user only ever lists/creates/revokes their own keys) and the key-creation
// delegation subset-check (a minted key can only carry servers+permissions
// the creating user already holds - Phase 5 routes this through the resolver in
// api_keys.go Create) as the real boundaries, same pattern as Task 20's
// modpack.*/library.*.
func TestCap_ApiKeysOwner(t *testing.T) {
	fs := &authzFakeStore{}
	fs.addUser("owner-id", "owner", false)
	srv := newAuthzTestServer(t, fs)
	id := testIdentity{UserID: "owner-id", Username: "owner"}
	if c := doAs(t, srv, "GET", "/api/me/api-keys", id); c == 403 {
		t.Error("apikeys.read must not 403 at the chokepoint for an authed user (handler scopes to self)")
	}
	if c := doAs(t, srv, "POST", "/api/me/api-keys", id); c == 403 {
		t.Error("apikeys.write must not 403 at the chokepoint for an authed user (handler scopes to self)")
	}
	if c := doAs(t, srv, "DELETE", "/api/me/api-keys/1", id); c == 403 {
		t.Error("apikeys.delete must not 403 at the chokepoint for an authed user (handler scopes to self)")
	}
}
