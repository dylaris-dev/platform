package gateway

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
	"github.com/redis/go-redis/v9"
)

const (
	gwOwnerA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	gwOwnerB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	gwNobody = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	gwBroken = "dddddddd-dddd-4ddd-8ddd-dddddddddddd" // the user lookup fails
)

// gwBeamPubA is owner A's beam public key: material the view must never carry.
var gwBeamPubA = []byte("0123456789abcdef0123456789ABCDEF")

// adminGwStore is extStore plus the reads only the admin view makes.
type adminGwStore struct {
	*extStore
	users   map[string]bool
	beam    []models.BeamIssuerKey
	apiKeys []models.APIKey
}

func (s *adminGwStore) GetUserByID(id string) (*models.User, error) {
	if id == gwBroken {
		return nil, errors.New("connection reset by peer")
	}
	if s.users[id] {
		return &models.User{ID: id}, nil
	}
	return nil, sql.ErrNoRows
}

func (s *adminGwStore) ListBeamIssuerKeysByOwner(owner string) ([]models.BeamIssuerKey, error) {
	var out []models.BeamIssuerKey
	for _, k := range s.beam {
		if k.OwnerID == owner {
			out = append(out, k)
		}
	}
	return out, nil
}

// CountLinkKitsByOwner is the SQL's count: live link kits only, so the BYON
// node key this fixture seeds is not one (extStore's count has no such key).
func (s *adminGwStore) CountLinkKitsByOwner(owner string) (int, error) {
	n := 0
	for _, k := range s.kits {
		if k.OwnerID == owner && k.RetireAt == nil && strings.HasPrefix(k.NodeID, "link-") {
			n++
		}
	}
	return n, nil
}

func (s *adminGwStore) ListAPIKeysByUser(user string) ([]models.APIKey, error) {
	var out []models.APIKey
	for _, k := range s.apiKeys {
		if k.UserID == user {
			out = append(out, k)
		}
	}
	return out, nil
}

// newAdminGwFixture seeds two owners. A holds kitsA link kits (link-a1 is
// online with client-addr, the last one retiring), a BYON node key, a live
// route on link-a1 and a suspended one, a verified own domain whose record is
// missing, a managed plan with traffic, a beam key and two API keys (one
// without gateway scope). B holds one of each, all of it markers.
func newAdminGwFixture(t *testing.T, kitsA int) (*GatewayHandler, *adminGwStore, *redis.Client) {
	t.Helper()
	created := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	failing := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	over := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	retire := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	used := time.Date(2026, 11, 12, 7, 0, 0, 0, time.UTC)
	included := int64(5000)
	three := int64(3)
	es := &extStore{
		settings: map[string]string{},
		kits: []store.WarpAPIKey{
			{ID: 90, Name: "byon node", NodeID: "node-77", OwnerID: gwOwnerA, KeyHash: "kit-hash-secret-node", CreatedAt: created},
			{ID: 91, Name: "b-kit-marker", NodeID: "link-b1", OwnerID: gwOwnerB, KeyHash: "kit-hash-secret-b", CreatedAt: created},
		},
		routes: []store.CoreLinkRoute{
			{Domain: "a.example.com", OwnerID: gwOwnerA, LinkToken: "derived-token-link-a1", TargetHost: "mc-12-abc123def456", TargetPort: 25565, ProxyProtocol: true},
			{Domain: "a-susp.example.com", OwnerID: gwOwnerA, LinkToken: "derived-token-link-a1", TargetHost: "10.0.0.9", TargetPort: 25566,
				SuspendedAt: &over, SuspendedReason: "abuse report"},
			{Domain: "b-marker.example.com", OwnerID: gwOwnerB, LinkToken: "derived-token-link-b1", TargetHost: "10.9.9.9", TargetPort: 25565},
		},
		claims: []store.CustomDomainClaim{
			{ID: 1, UserID: gwOwnerA, Domain: "a-own.net", State: store.ClaimVerified, TXTToken: "txt-secret-a", CheckedAt: &used, FailingSince: &failing},
			{ID: 2, UserID: gwOwnerB, Domain: "b-own-marker.net", State: store.ClaimVerified, TXTToken: "txt-secret-b"},
		},
		regions: map[string][]store.RegionUsage{
			gwOwnerA: {{Region: "eu", Kind: "edge", Product: "route", Bytes: 1000}},
			gwOwnerB: {{Region: "us-marker", Kind: "edge", Product: "route", Bytes: 2}},
		},
		usage: map[string]*store.TrafficUsage{
			gwOwnerA: {UserID: gwOwnerA, EdgeBytes: 1_000_000_000_000, RelayBytes: 5, ManagedRelayBytes: 250_000_000_000},
		},
		limits:  map[string]*models.TrafficLimit{"user:" + gwOwnerA: {IncludedGB: &included}},
		billing: map[string]*store.UserBilling{gwOwnerA: {UserID: gwOwnerA, Status: "active", MaxLinks: &three, OverLimitSince: &over}},
	}
	for i := 1; i <= kitsA; i++ {
		k := store.WarpAPIKey{ID: i, Name: fmt.Sprintf("kit %d", i), NodeID: fmt.Sprintf("link-a%d", i), OwnerID: gwOwnerA,
			Region: "eu", KeyHash: fmt.Sprintf("kit-hash-secret-a%d", i), CreatedAt: created}
		if i == kitsA && kitsA > 1 {
			k.RetireAt = &retire
		}
		es.kits = append(es.kits, k)
	}
	st := &adminGwStore{
		extStore: es,
		users:    map[string]bool{gwOwnerA: true, gwOwnerB: true},
		beam: []models.BeamIssuerKey{
			{ID: "kid-a", OwnerID: gwOwnerA, PublicKey: gwBeamPubA, CreatedAt: created},
			{ID: "kid-b-marker", OwnerID: gwOwnerB, PublicKey: []byte("b-public-key-bytes-b-public-key!"), CreatedAt: created},
		},
		apiKeys: []models.APIKey{
			{ID: 1, UserID: gwOwnerA, Name: "provider key", KeyHash: "dyl_hash_secret_a",
				Scope: models.APIKeyScope{Permissions: []string{"gateway.read", "gateway.write"}}, CreatedAt: created, LastUsedAt: &used},
			{ID: 2, UserID: gwOwnerA, Name: "rcon only", KeyHash: "dyl_hash_secret_rcon",
				Scope: models.APIKeyScope{Permissions: []string{"rcon.exec"}}, CreatedAt: created},
			{ID: 3, UserID: gwOwnerB, Name: "b-key-marker", KeyHash: "dyl_hash_secret_b",
				Scope: models.APIKeyScope{Permissions: []string{"gateway.read"}}, CreatedAt: created},
		},
	}
	rdb := newLinkRouteRedis(t)
	rdb.Set(context.Background(), "online_link:derived-token-link-a1", `{"caps":["client-addr"]}`, 0)
	h := NewGatewayHandler(Deps{Store: st, Gateway: &linkRouteFakeGateway{}, Redis: rdb, FeatureFlags: services.NewFeatureFlags(routeOnlyOnFlags{})})
	return h, st, rdb
}

// gwCapsStore is the authz store of one non-admin caller, "viewer", whose
// panel role holds exactly caps.
type gwCapsStore struct{ caps []string }

func (gwCapsStore) GetServerByID(int) (*models.Server, error)      { return nil, nil }
func (gwCapsStore) GetServerByUUID(string) (*models.Server, error) { return nil, nil }
func (s gwCapsStore) GetPanelRole(id int) (*store.PanelRole, error) {
	return &store.PanelRole{ID: id, Capabilities: s.caps}, nil
}
func (gwCapsStore) GetServerRole(int) (*store.ServerRole, error) { return nil, nil }
func (gwCapsStore) GetUserPanelAuthz(userID string) (*int, store.CapOverrides, error) {
	role := 1000
	if userID != "viewer" {
		return nil, store.CapOverrides{}, nil
	}
	return &role, store.CapOverrides{}, nil
}
func (gwCapsStore) GetServerGrant(int, string) (*store.ServerGrant, error)     { return nil, nil }
func (gwCapsStore) GetAccountGrant(string, string) (*store.ServerGrant, error) { return nil, nil }

var gwAllCaps = []string{"topology.read", "plans.read", "users.read"}

// adminUserGateway asks as a caller holding every block's capability.
func adminUserGateway(h *GatewayHandler, id string) *httptest.ResponseRecorder {
	return adminUserGatewayAs(h, id, gwAllCaps...)
}

// adminUserGatewayAs asks as "viewer" holding exactly caps (the route already
// checked topology.read; the handler resolves the rest itself).
func adminUserGatewayAs(h *GatewayHandler, id string, caps ...string) *httptest.ResponseRecorder {
	h.d.Authz = authz.NewResolver(gwCapsStore{caps: caps})
	r := httptest.NewRequest(http.MethodGet, "/api/admin/users/"+id+"/gateway", nil)
	r = mux.SetURLVars(r, map[string]string{"id": id})
	r = r.WithContext(context.WithValue(context.WithValue(r.Context(), "userID", "viewer"), "isAdmin", false))
	rec := httptest.NewRecorder()
	h.AdminUserGateway(rec, r)
	return rec
}

// adminGwView is the answer, typed the way the panel reads it.
type adminGwView struct {
	Success        bool   `json:"success"`
	UserID         string `json:"userId"`
	Product        string `json:"product"`
	BillingStatus  string `json:"billingStatus"`
	OverLimitSince string `json:"overLimitSince"`
	Limits         struct {
		MaxLinks *int64 `json:"maxLinks"`
	} `json:"limits"`
	Kits                   []adminGatewayKit   `json:"kits"`
	KitsUsed               int                 `json:"kitsUsed"`
	KitsRetiring           int                 `json:"kitsRetiring"`
	Routes                 []adminGatewayRoute `json:"routes"`
	RoutesTruncated        bool                `json:"routesTruncated"`
	CustomDomains          []adminGatewayClaim `json:"customDomains"`
	CustomDomainsTruncated bool                `json:"customDomainsTruncated"`
	Usage                  struct {
		Period     string               `json:"period"`
		EdgeBytes  int64                `json:"edgeBytes"`
		RelayBytes int64                `json:"relayBytes"`
		Regions    []adminGatewayRegion `json:"regions"`
		Pool       *externalPool        `json:"pool"`
	} `json:"usage"`
	BeamKeys []adminGatewayBeamKey `json:"beamKeys"`
	APIKeys  []adminGatewayAPIKey  `json:"apiKeys"`
}

func TestAdminUserGatewayShowsOneOwner(t *testing.T) {
	h, _, _ := newAdminGwFixture(t, 2)
	rec := adminUserGateway(h, gwOwnerA)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var v adminGwView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Success || v.UserID != gwOwnerA || v.Product != "managed" || v.BillingStatus != "active" ||
		v.OverLimitSince != "2026-11-11T12:00:00Z" || v.Limits.MaxLinks == nil || *v.Limits.MaxLinks != 3 {
		t.Fatalf("account fields: %s", rec.Body.String())
	}
	// Two link kits, the BYON node key left out; link-a1 online with its caps,
	// link-a2 offline and retiring.
	if len(v.Kits) != 2 || v.KitsUsed != 1 || v.KitsRetiring != 1 {
		t.Fatalf("kits %+v used %d retiring %d", v.Kits, v.KitsUsed, v.KitsRetiring)
	}
	byID := map[string]adminGatewayKit{}
	for _, k := range v.Kits {
		byID[k.LinkID] = k
	}
	if k := byID["link-a1"]; k.Online == nil || !*k.Online || len(k.Caps) != 1 || k.Caps[0] != "client-addr" || k.Region != "eu" || k.RetireAt != "" {
		t.Fatalf("link-a1 = %+v", k)
	}
	if k := byID["link-a2"]; k.Online == nil || *k.Online || len(k.Caps) != 0 || k.RetireAt != "2026-11-14T08:00:00Z" {
		t.Fatalf("link-a2 = %+v", k)
	}
	if len(v.Routes) != 2 || v.RoutesTruncated {
		t.Fatalf("routes %+v", v.Routes)
	}
	for _, r := range v.Routes {
		if r.LinkID != "link-a1" {
			t.Errorf("route %s linkId = %q, want link-a1", r.Domain, r.LinkID)
		}
		if r.Domain == "a-susp.example.com" && (r.SuspendedAt == nil || r.SuspendedReason != "abuse report") {
			t.Errorf("suspended route = %+v", r)
		}
		if r.Domain == "a.example.com" && (r.SuspendedAt != nil || !r.ProxyProtocol || r.TargetPort != 25565) {
			t.Errorf("live route = %+v", r)
		}
	}
	if len(v.CustomDomains) != 1 || v.CustomDomains[0].Domain != "a-own.net" || v.CustomDomains[0].LapsesAt == nil ||
		!v.CustomDomains[0].LapsesAt.Equal(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC).Add(services.CustomDomainLapseGrace)) {
		t.Fatalf("claims %+v", v.CustomDomains)
	}
	if v.Usage.Period != time.Now().UTC().Format("2006-01") || v.Usage.EdgeBytes != 1_000_000_000_000 || v.Usage.RelayBytes != 5 ||
		len(v.Usage.Regions) != 1 || v.Usage.Regions[0].Product != "route" || v.Usage.Pool == nil ||
		v.Usage.Pool.IncludedGB != 5000 || v.Usage.Pool.Percent != 25 {
		t.Fatalf("usage: %s", rec.Body.String())
	}
	if len(v.BeamKeys) != 1 || v.BeamKeys[0].Kid != "kid-a" {
		t.Fatalf("beam keys %+v", v.BeamKeys)
	}
	if len(v.APIKeys) != 1 || v.APIKeys[0].Name != "provider key" || v.APIKeys[0].LastUsedAt == nil ||
		strings.Join(v.APIKeys[0].Permissions, ",") != "gateway.read,gateway.write" {
		t.Fatalf("api keys %+v (the rcon-only key must be left out)", v.APIKeys)
	}
	for _, marker := range []string{gwOwnerB, "b-kit-marker", "link-b1", "b-marker.example.com", "b-own-marker.net", "us-marker", "kid-b-marker", "b-key-marker"} {
		if strings.Contains(rec.Body.String(), marker) {
			t.Errorf("owner B's %q appears in owner A's view", marker)
		}
	}
}

// Review Focus 5: searched in the raw bytes, so a field added later under any
// name is caught too.
func TestAdminUserGatewayCarriesNoSecret(t *testing.T) {
	h, _, _ := newAdminGwFixture(t, 2)
	body := adminUserGateway(h, gwOwnerA).Body.String()
	for _, secret := range []string{
		"derived-token-",   // the link token, of kits and of routes
		"kit-hash-secret",  // the kit row's key hash
		"dyl_",             // an API key hash or key
		"txt-secret-a",     // the TXT value (an instruction, but not this view's)
		string(gwBeamPubA), // the beam public key, raw
		base64.StdEncoding.EncodeToString(gwBeamPubA), // as []byte marshals
		hex.EncodeToString(gwBeamPubA),
	} {
		if strings.Contains(body, secret) {
			t.Errorf("response carries %q: %s", secret, body)
		}
	}
}

// Online state and caps for every kit cost one pipelined trip, whatever the
// kit count.
func TestAdminUserGatewayOneRedisRoundTrip(t *testing.T) {
	for _, n := range []int{1, 5, 20} {
		h, _, rdb := newAdminGwFixture(t, n)
		hook := &roundTrips{}
		rdb.AddHook(hook)
		rec := adminUserGateway(h, gwOwnerA)
		var v adminGwView
		_ = json.Unmarshal(rec.Body.Bytes(), &v)
		if rec.Code != 200 || len(v.Kits) != n {
			t.Fatalf("%d kits: %d %s", n, rec.Code, rec.Body.String())
		}
		if hook.n != 1 {
			t.Errorf("%d kits: redis round trips = %d, want 1", n, hook.n)
		}
	}
}

func TestAdminUserGatewayUnknownOrBadUser(t *testing.T) {
	h, _, _ := newAdminGwFixture(t, 1)
	if rec := adminUserGateway(h, gwNobody); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "User not found") {
		t.Fatalf("unknown user: %d %s", rec.Code, rec.Body.String())
	}
	if rec := adminUserGateway(h, "not-a-uuid"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d %s", rec.Code, rec.Body.String())
	}
	// A failed lookup is not an answer about the user.
	if rec := adminUserGateway(h, gwBroken); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "not found") ||
		strings.Contains(rec.Body.String(), "connection reset") {
		t.Fatalf("lookup error: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminUserGatewayCutsLongLists(t *testing.T) {
	h, st, _ := newAdminGwFixture(t, 1)
	for i := 0; i < adminGatewayListCap+5; i++ {
		st.routes = append(st.routes, store.CoreLinkRoute{Domain: fmt.Sprintf("r%03d.example.com", i), OwnerID: gwOwnerA, TargetHost: "10.0.0.1", TargetPort: 25565})
	}
	var v adminGwView
	rec := adminUserGateway(h, gwOwnerA)
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if len(v.Routes) != adminGatewayListCap || !v.RoutesTruncated || v.CustomDomainsTruncated {
		t.Fatalf("routes %d truncated %v (claims truncated %v)", len(v.Routes), v.RoutesTruncated, v.CustomDomainsTruncated)
	}
}

// The external /usage answer is byte-identical to the one captured before
// usageStanding became a package func (golden bodies from that commit).
func TestExternalUsageUnchanged(t *testing.T) {
	f := newExtFixture(t, withManagedOwner(3, 5000))
	f.st.usage = map[string]*store.TrafficUsage{extTenantC: {UserID: extTenantC, EdgeBytes: 1_000_000_000_000, RelayBytes: 7, ManagedRelayBytes: 250_000_000_000,
		UpdatedAt: time.Date(2026, 11, 12, 10, 30, 0, 0, time.UTC)}}
	maxBuy := int64(2000)
	f.st.limits["user:"+extTenantC] = &models.TrafficLimit{IncludedGB: f.st.limits["user:"+extTenantC].IncludedGB, MaxPurchaseGB: &maxBuy}
	f.st.regions[extTenantC] = []store.RegionUsage{{Region: "eu", Kind: "edge", Product: "route", Bytes: 100}, {Region: "us", Kind: "relay", Product: "route", Bytes: 300}}
	const usage = `"usage":{"backupBytes":0,"edgeBytes":1000000000000,"limits":{"maxLinks":3,"maxNodes":null,"r2QuotaGb":null,"trafficCombinedGb":null,"trafficEdgeGb":null,"trafficRelayGb":null},"over":{"combined":false,"edge":false,"r2":false,"relay":false},"period":"0001-01-01T00:00:00Z","relayBytes":7,"updatedAt":"2026-11-12T10:30:00Z","userId":"user-c"}}` + "\n"
	const regions = `"regions":[{"region":"us","kind":"relay","bytes":300},{"region":"eu","kind":"edge","bytes":100}],"success":true,`
	cases := []struct{ query, want string }{
		{"/", `{"asOf":"2026-11-12T10:30:00Z","final":false,"pool":{"includedGb":5000,"maxPurchaseGb":2000,"usedBytes":1250000000000,"percent":25},` + regions + usage},
		{"/?period=2020-01", `{"asOf":"2026-11-12T10:30:00Z","final":true,` + regions + usage},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		f.h.GetUsage(rec, extReq("GET", c.query, extTenantC, false, nil, nil))
		if rec.Code != 200 || rec.Body.String() != c.want {
			t.Errorf("%s: %d\n got %s\nwant %s", c.query, rec.Code, rec.Body.String(), c.want)
		}
	}
}

// Blocks owned by other capabilities appear only for a caller holding them;
// what is left out is named in hidden and absent from the raw body.
func TestAdminUserGatewayBlocksFollowTheCallersCaps(t *testing.T) {
	// As JSON keys (with the colon): "usage" and "apiKeys" are also hidden's values.
	billingKeys := []string{`"billingStatus":`, `"overLimitSince":`, `"product":`, `"limits":`}
	usageKeys := []string{`"usage":`, `"pool":`, `"edgeBytes":`}
	keyKeys := []string{`"apiKeys":`, `"provider key"`}
	cases := []struct {
		name       string
		caps       []string
		hidden     []string
		shown, not []string
	}{
		{"topology.read only", []string{"topology.read"}, []string{"billing", "usage", "apiKeys"}, nil, append(append(billingKeys, usageKeys...), keyKeys...)},
		{"plus plans.read", []string{"topology.read", "plans.read"}, []string{"apiKeys"}, append(billingKeys, usageKeys...), keyKeys},
		{"plus users.read", []string{"topology.read", "users.read"}, []string{"billing", "usage"}, keyKeys, append(billingKeys, usageKeys...)},
		{"all three", gwAllCaps, []string{}, append(append(billingKeys, usageKeys...), keyKeys...), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, _, _ := newAdminGwFixture(t, 2)
			rec := adminUserGatewayAs(h, gwOwnerA, c.caps...)
			body := rec.Body.String()
			var v struct {
				Hidden []string          `json:"hidden"`
				Kits   []adminGatewayKit `json:"kits"`
				Routes []json.RawMessage `json:"routes"`
			}
			if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
				t.Fatalf("%d %s", rec.Code, body)
			}
			if strings.Join(v.Hidden, ",") != strings.Join(c.hidden, ",") || v.Hidden == nil {
				t.Errorf("hidden = %v, want %v", v.Hidden, c.hidden)
			}
			// The topology is shown to every caller the route lets in.
			if len(v.Kits) != 2 || len(v.Routes) != 2 || !strings.Contains(body, `"customDomains"`) || !strings.Contains(body, `"beamKeys"`) {
				t.Errorf("topology missing: %s", body)
			}
			for _, k := range c.shown {
				if !strings.Contains(body, k) {
					t.Errorf("%s missing: %s", k, body)
				}
			}
			for _, k := range c.not {
				if strings.Contains(body, k) {
					t.Errorf("%s present for %v: %s", k, c.caps, body)
				}
			}
		})
	}
}
