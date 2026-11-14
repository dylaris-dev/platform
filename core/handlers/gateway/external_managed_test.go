package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"dylaris-core/handlers/billing"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// managedStore is the route-only fixture (linkDomain, owner-1, link "tok")
// plus what the managed exemption and the store's activate touch: the traffic
// rows, the route-only voice rows and kits. traffic holds the rows exactly as
// the activate leaves them, so a test can run the real activate and then ask
// the handlers.
type managedStore struct {
	*linkBedrockStore
	traffic  map[string]models.TrafficLimit // scope/region/kind
	limitErr error
	voice    map[string]store.LinkRouteVoice
	kits     []store.WarpAPIKey
	// roleID gives owner-1 a panel role (its row lives in perms as
	// "role:<id>"); permErr fails every voice permission read.
	roleID  *int
	permErr error
}

func (m *managedStore) GetUserPanelAuthz(string) (*int, store.CapOverrides, error) {
	return m.roleID, store.CapOverrides{}, nil
}
func (m *managedStore) GetVoicePermission(scope string) (*bool, error) {
	if m.permErr != nil {
		return nil, m.permErr
	}
	return m.linkBedrockStore.GetVoicePermission(scope)
}

func (m *managedStore) GetTrafficLimit(scope, region, kind string) (*models.TrafficLimit, error) {
	if m.limitErr != nil {
		return nil, m.limitErr
	}
	if l, ok := m.traffic[scope+"/"+region+"/"+kind]; ok {
		return &l, nil
	}
	return nil, nil
}
func (m *managedStore) ReplaceTrafficLimits(scope string, rows []models.TrafficLimit) error {
	delete(m.traffic, scope+"/*/edge")
	for _, r := range rows {
		m.traffic[scope+"/"+r.Region+"/"+r.Kind] = r
	}
	return nil
}
func (m *managedStore) DeleteTrafficLimit(scope, region, kind string) error {
	delete(m.traffic, scope+"/"+region+"/"+kind)
	return nil
}
func (m *managedStore) GetLinkRouteVoice(domain string) (*store.LinkRouteVoice, error) {
	if v, ok := m.voice[domain]; ok {
		return &v, nil
	}
	return nil, nil
}
func (m *managedStore) AllocateLinkRouteVoice(domain, region string, targetPort int, key []byte, min, _ int) (store.LinkRouteVoice, error) {
	v := store.LinkRouteVoice{Domain: domain, OwnerID: "owner-1", LinkToken: "tok", TargetHost: "127.0.0.1", Region: region, Port: min, TargetPort: targetPort, UplinkKey: key}
	m.voice[domain] = v
	return v, nil
}
func (m *managedStore) DeleteLinkRouteVoice(domain string) (bool, error) {
	_, ok := m.voice[domain]
	delete(m.voice, domain)
	return ok, nil
}
func (m *managedStore) ListWarpAPIKeysByOwner(owner string) ([]store.WarpAPIKey, error) {
	var out []store.WarpAPIKey
	for _, k := range m.kits {
		if k.OwnerID == owner {
			out = append(out, k)
		}
	}
	return out, nil
}

// What the store's activate writes besides the traffic rows; none of it
// matters to these tests.
func (m *managedStore) GetUserByID(id string) (*models.User, error) { return &models.User{ID: id}, nil }
func (m *managedStore) SetUserBillingStatus(string, string, *time.Time, *time.Time) error {
	return nil
}
func (m *managedStore) SetUserPurchasedEntitlement(string, *int64, bool, *int64, bool) error {
	return nil
}
func (m *managedStore) SetUserManualEntitlementKind(string, string, *time.Time, string) error {
	return nil
}
func (m *managedStore) SetGatewayRouteLimit(string, *int) error          { return nil }
func (m *managedStore) DeleteGatewayRouteLimit(string) error             { return nil }
func (m *managedStore) SetUserCustomDomainLimit(string, *int64) error    { return nil }
func (m *managedStore) ClearUserCustomDomainLimit(string) error          { return nil }
func (m *managedStore) SetUserBillingConsent(string, *bool, *bool) error { return nil }
func (m *managedStore) SetUserDataBillingEnabled(string, bool) error     { return nil }
func (m *managedStore) ListBeamIssuerKeysByOwner(string) ([]models.BeamIssuerKey, error) {
	return nil, nil
}

type managedEnv struct {
	e  *voiceSwitchEnv
	ms *managedStore
	x  *ExternalGatewayHandler
}

// newManagedEnv meets every enable precondition for linkDomain except the
// permission: global voice off, Bedrock audience off. managed gives owner-1
// the user "*" edge traffic row a managed plan writes.
func newManagedEnv(t *testing.T, managed bool) *managedEnv {
	t.Helper()
	e, ls := newLinkBedrock(t)
	ms := &managedStore{linkBedrockStore: ls, traffic: map[string]models.TrafficLimit{}, voice: map[string]store.LinkRouteVoice{}}
	if managed {
		inc := int64(5000)
		ms.traffic["user:owner-1/*/edge"] = models.TrafficLimit{Region: "*", Kind: "edge", IncludedGB: &inc}
	}
	ls.global = "false"
	ls.audience = "off"
	e.h.d.Store = ms
	e.h.d.FeatureFlags = services.NewFeatureFlags(ms)
	e.h.d.Gateway = &linkRouteFakeGateway{}
	e.mr.Set("online_link:tok", `{"caps":["voice"]}`)
	return &managedEnv{e: e, ms: ms, x: &ExternalGatewayHandler{gw: e.h}}
}

func (m *managedEnv) extVoice(owner string) (int, map[string]interface{}) {
	rec := httptest.NewRecorder()
	m.x.SetRouteVoice(rec, extReq("PUT", "/", owner, false, map[string]string{"domain": linkDomain}, map[string]interface{}{"enabled": true}))
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (m *managedEnv) extBedrock(owner string) (int, map[string]interface{}) {
	rec := httptest.NewRecorder()
	m.x.SetRouteBedrock(rec, extReq("PUT", "/", owner, false, map[string]string{"domain": linkDomain}, map[string]interface{}{"java": true, "bedrock": true}))
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// activate runs the REAL store activate (billing.StoreHandler.Provision) with
// the body the website sends.
func (m *managedEnv) activate(t *testing.T, body map[string]interface{}) {
	t.Helper()
	const key = "store-key-for-tests"
	rdb := m.e.h.d.Redis
	h := billing.NewStoreHandler(billing.Deps{Store: m.ms, Redis: rdb, StoreEnabled: true, StoreSharedKey: key,
		Billing: services.NewBillingLifecycleService(m.ms, services.NewQueueService(rdb), nil, "https://panel.example.com", 48*time.Hour, true)})
	b := map[string]interface{}{"uuid": "owner-1", "action": "activate"}
	for k, v := range body {
		b[k] = v
	}
	raw, _ := json.Marshal(b)
	r := httptest.NewRequest("POST", "/api/store/provision", bytes.NewReader(raw))
	r.Header.Set("X-Store-Key", key)
	rec := httptest.NewRecorder()
	h.Provision(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body.String())
	}
}

// managedActivateBody is website managedEntitlement for mg_1 (paid row and
// admin grant alike, account_state.go entitlementForPlan) with the traffic deal.
func managedActivateBody() map[string]interface{} {
	return map[string]interface{}{
		"maxNodes": 0, "maxLinks": 1, "maxRoutes": 50, "maxCustomDomains": "unlimited",
		"trafficLimits":         []map[string]interface{}{{"region": "*", "kind": "edge", "includedGb": 5000, "maxPurchaseGb": 5000}},
		"trafficBillingEnabled": false, "dataBillingEnabled": false, "backupBillingEnabled": false,
	}
}

func wantRefusal(t *testing.T, what string, status int, out map[string]interface{}, wantStatus int, code string) {
	t.Helper()
	if status != wantStatus || out["code"] != code || out["success"] != false {
		t.Fatalf("%s: %d %v, want %d %s", what, status, out, wantStatus, code)
	}
}

const (
	voiceNotAllowedMsg = "Voice chat is not available for this account."
	// An external caller without a managed plan is told what would lift it.
	planNeeded = " It needs an active managed gateway plan."
)

func TestExternalVoiceManagedAllowedWithGlobalOff(t *testing.T) {
	m := newManagedEnv(t, true)
	status, out := m.extVoice("owner-1")
	if status != http.StatusOK || out["success"] != true {
		t.Fatalf("managed voice with the global switch off: %d %v", status, out)
	}
	if _, ok := m.ms.voice[linkDomain]; !ok || !m.e.mr.Exists("voice:port:eu:"+strconv.Itoa(services.VoicePortMin)) {
		t.Fatal("voice was answered on but not stored and published")
	}
}

func TestExternalBedrockManagedAllowedWithAudienceOff(t *testing.T) {
	for _, audience := range []string{"off", "admin"} {
		t.Run(audience, func(t *testing.T) {
			m := newManagedEnv(t, true)
			m.ms.audience = audience
			status, out := m.extBedrock("owner-1")
			if status != http.StatusOK || out["success"] != true {
				t.Fatalf("managed Bedrock with audience %s: %d %v", audience, status, out)
			}
			if e, ok := m.ms.links[linkDomain]; !ok || !e.Bedrock {
				t.Fatal("Bedrock was answered on but not stored")
			}
			// Served, not only stored: what the edge and the bridge read.
			if got := m.e.mr.HGet(services.RouteEditionsKey, linkDomain); got != "both" {
				t.Fatalf("published edition %q, want both", got)
			}
		})
	}
}

func TestExternalVoiceNonManagedStillRefused(t *testing.T) {
	m := newManagedEnv(t, false)
	status, out := m.extVoice("owner-1")
	wantRefusal(t, "route-only voice", status, out, http.StatusConflict, "voice_not_allowed")
	if out["error"] != voiceNotAllowedMsg+planNeeded || len(m.ms.voice) != 0 {
		t.Fatalf("body %v, rows %d", out, len(m.ms.voice))
	}
	// An explicit user deny is not lifted by a plan, so it does not say so.
	m.ms.global = "true"
	m.ms.perms = map[string]bool{"user:owner-1": false}
	if status, out := m.extVoice("owner-1"); status != http.StatusConflict || out["error"] != voiceNotAllowedMsg {
		t.Fatalf("user deny: %d %v", status, out)
	}
}

func TestExternalBedrockNonManagedStillRefused(t *testing.T) {
	for _, audience := range []string{"off", "admin"} {
		m := newManagedEnv(t, false)
		m.ms.audience = audience
		status, out := m.extBedrock("owner-1")
		wantRefusal(t, "route-only Bedrock, audience "+audience, status, out, http.StatusConflict, "bedrock_disabled")
		if out["error"] != bedrockOff+planNeeded || len(m.ms.links) != 0 {
			t.Fatalf("body %v, rows %d", out, len(m.ms.links))
		}
		// An admin user's key is no admin here either, managed or not.
		rec := httptest.NewRecorder()
		m.x.SetRouteBedrock(rec, extReq("PUT", "/", "owner-1", true, map[string]string{"domain": linkDomain}, map[string]interface{}{"java": true, "bedrock": true}))
		if rec.Code != http.StatusConflict || len(m.ms.links) != 0 {
			t.Fatalf("admin user's key, audience %s: %d %s", audience, rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		m.x.SetRouteVoice(rec, extReq("PUT", "/", "owner-1", true, map[string]string{"domain": linkDomain}, map[string]interface{}{"enabled": true}))
		if rec.Code != http.StatusConflict || len(m.ms.voice) != 0 {
			t.Fatalf("admin user's key, voice: %d %s", rec.Code, rec.Body.String())
		}
	}
}

// The account held a plan (granted or paid, the activate is the same) and the
// store then said it no longer does: refused from that activate on.
func TestManagedPlanEndedIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		body map[string]interface{}
	}{
		// What the website sends once when a managed plan ended (WipeManaged).
		{"the wipe", map[string]interface{}{"maxNodes": 0, "maxLinks": 1, "maxRoutes": 3, "trafficLimits": []interface{}{}, "maxCustomDomains": nil}},
		// An activate that names no managed plan clears the row too.
		{"an activate without trafficLimits", map[string]interface{}{"maxNodes": 2, "maxLinks": 0, "maxRoutes": 6}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newManagedEnv(t, false)
			m.activate(t, managedActivateBody())
			if status, out := m.extVoice("owner-1"); status != http.StatusOK {
				t.Fatalf("premise: voice while managed: %d %v", status, out)
			}
			delete(m.ms.voice, linkDomain)
			m.activate(t, c.body)
			status, out := m.extVoice("owner-1")
			wantRefusal(t, "voice after the plan ended", status, out, http.StatusConflict, "voice_not_allowed")
			if out["error"] != voiceNotAllowedMsg+planNeeded {
				t.Fatalf("voice after the plan ended says %q", out["error"])
			}
			status, out = m.extBedrock("owner-1")
			wantRefusal(t, "Bedrock after the plan ended", status, out, http.StatusConflict, "bedrock_disabled")
			if out["error"] != bedrockOff+planNeeded {
				t.Fatalf("Bedrock after the plan ended says %q", out["error"])
			}
		})
	}
}

// An admin grant (website source = 'manual', product mg_1) reaches Core as
// the same activate a paid plan sends: with the user "*" edge traffic row and
// no Stripe anywhere. That alone makes the account managed.
func TestManualGrantCountsAsManaged(t *testing.T) {
	m := newManagedEnv(t, false)
	if status, _ := m.extVoice("owner-1"); status != http.StatusConflict {
		t.Fatalf("premise: refused before the grant, got %d", status)
	}
	m.activate(t, managedActivateBody())
	if status, out := m.extVoice("owner-1"); status != http.StatusOK {
		t.Fatalf("voice after the grant: %d %v", status, out)
	}
	if status, out := m.extBedrock("owner-1"); status != http.StatusOK {
		t.Fatalf("Bedrock after the grant: %d %v", status, out)
	}
}

// The panel routes never set the exemption: a managed account using our panel
// is under the global rules.
func TestPanelPathGetsNoManagedExemption(t *testing.T) {
	m := newManagedEnv(t, true)
	rec := linkCall(m.e, m.e.h.SetLinkRouteVoice, http.MethodPut, linkDomain, `{"enabled":true}`, "owner-1", false)
	if rec.Code != http.StatusConflict || !bytes.Contains(rec.Body.Bytes(), []byte("voice_not_allowed")) {
		t.Fatalf("panel voice: %d %s", rec.Code, rec.Body.String())
	}
	rec = linkCall(m.e, m.e.h.SetLinkRouteBedrock, http.MethodPut, linkDomain, `{"java":true,"bedrock":true}`, "owner-1", false)
	if rec.Code != http.StatusConflict || !bytes.Contains(rec.Body.Bytes(), []byte("bedrock_disabled")) {
		t.Fatalf("panel Bedrock: %d %s", rec.Code, rec.Body.String())
	}
	if len(m.ms.voice) != 0 || len(m.ms.links) != 0 {
		t.Fatal("a refused panel call wrote something")
	}
}

// The exemption replaces the permission check and nothing else.
func TestManagedExemptionSkipsNoOtherCheck(t *testing.T) {
	retiring := func(m *managedEnv) {
		at := time.Now().Add(time.Hour)
		m.ms.kits = []store.WarpAPIKey{{ID: 1, NodeID: "link-x", OwnerID: "owner-1", RetireAt: &at}}
		m.e.h.d.Gateway = &linkRouteFakeGateway{linkTokenFor: map[string]string{"link-x": "tok"}}
	}
	suspended := func(m *managedEnv) { m.ms.billing = &store.UserBilling{UserID: "owner-1", Status: "suspended"} }
	for _, c := range []struct {
		name   string
		voice  bool // else Bedrock
		owner  string
		setup  func(m *managedEnv)
		status int
		code   string
	}{
		{"voice: suspended owner", true, "owner-1", suspended, http.StatusForbidden, "forbidden"},
		{"Bedrock: suspended owner", false, "owner-1", suspended, http.StatusForbidden, "forbidden"},
		{"voice: retiring kit", true, "owner-1", retiring, http.StatusConflict, "kit_retiring"},
		{"Bedrock: retiring kit", false, "owner-1", retiring, http.StatusConflict, "kit_retiring"},
		{"voice: link without the voice cap", true, "owner-1", func(m *managedEnv) { m.e.mr.Del("online_link:tok") }, http.StatusConflict, "link_without_voice"},
		{"voice: no relay in the region", true, "owner-1", func(m *managedEnv) { m.e.mr.Del("voice:registry:r1") }, http.StatusConflict, "no_relay"},
		{"Bedrock: no Bedrock entry in the region", false, "owner-1", func(m *managedEnv) { m.e.mr.Del("bedrock:registry:b1") }, http.StatusConflict, "bedrock_unavailable"},
		{"voice: another account's route", true, "owner-2", func(m *managedEnv) {
			m.ms.traffic["user:owner-2/*/edge"] = models.TrafficLimit{Region: "*", Kind: "edge"}
		}, http.StatusNotFound, "not_found"},
		{"Bedrock: another account's route", false, "owner-2", func(m *managedEnv) {
			m.ms.traffic["user:owner-2/*/edge"] = models.TrafficLimit{Region: "*", Kind: "edge"}
		}, http.StatusNotFound, "not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newManagedEnv(t, true)
			c.setup(m)
			var status int
			var out map[string]interface{}
			if c.voice {
				status, out = m.extVoice(c.owner)
			} else {
				status, out = m.extBedrock(c.owner)
			}
			wantRefusal(t, c.name, status, out, c.status, c.code)
			if len(m.ms.voice) != 0 || len(m.ms.links) != 0 {
				t.Fatal("a refused call wrote something")
			}
		})
	}
}

// The exemption overrides the global switch and role rows, never an explicit
// user deny, and a permission read error allows nothing (no deny can be ruled
// out); it answers a server error, not a refusal.
func TestExternalVoiceManagedAgainstDenyRows(t *testing.T) {
	role := 3
	for _, c := range []struct {
		name  string
		setup func(m *managedEnv)
		ok    bool
	}{
		{"global off", func(*managedEnv) {}, true},
		{"role deny", func(m *managedEnv) {
			m.ms.global = "true"
			m.ms.roleID = &role
			m.ms.perms = map[string]bool{"role:3": false}
		}, true},
		{"user deny", func(m *managedEnv) {
			m.ms.global = "true"
			m.ms.perms = map[string]bool{"user:owner-1": false}
		}, false},
		{"permission read error", func(m *managedEnv) { m.ms.permErr = errors.New("connection reset by peer") }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newManagedEnv(t, true)
			c.setup(m)
			status, out := m.extVoice("owner-1")
			if c.ok {
				if status != http.StatusOK {
					t.Fatalf("%d %v, want 200", status, out)
				}
				return
			}
			if m.ms.permErr != nil {
				wantRefusal(t, c.name, status, out, http.StatusInternalServerError, "internal")
			} else {
				wantRefusal(t, c.name, status, out, http.StatusConflict, "voice_not_allowed")
			}
			if len(m.ms.voice) != 0 {
				t.Fatal("a refused enable wrote a row")
			}
		})
	}
}

// The mark names its owner: it never exempts a change to another account's
// row, even if a later handler reached one.
func TestManagedMarkIsPerOwner(t *testing.T) {
	m := newManagedEnv(t, true)
	r := withManagedAPI(httptest.NewRequest("PUT", "/", nil), m.ms, "owner-1")
	if !managedAPICall(r, "owner-1") || managedAPICall(r, "owner-2") || managedAPICall(r, "") {
		t.Fatal("the mark must hold for its owner only")
	}
	if managedAPICall(withManagedAPI(httptest.NewRequest("PUT", "/", nil), m.ms, ""), "") {
		t.Fatal("no owner, no mark")
	}
}

// A failed plan lookup is not "managed": it grants nothing. Where the global
// rules would refuse, the answer is a server error (the refusal would be a
// guess); where they allow, the change goes through.
func TestManagedLookupErrorFallsBackToGlobalRules(t *testing.T) {
	m := newManagedEnv(t, true)
	m.ms.limitErr = errors.New("connection reset by peer")
	status, out := m.extVoice("owner-1")
	wantRefusal(t, "voice, lookup error, global off", status, out, http.StatusInternalServerError, "internal")
	status, out = m.extBedrock("owner-1")
	wantRefusal(t, "Bedrock, lookup error, audience off", status, out, http.StatusInternalServerError, "internal")
	if len(m.ms.voice) != 0 {
		t.Fatal("a lookup error turned voice on")
	}

	m.ms.global = "true"
	m.ms.audience = "all"
	if status, out := m.extVoice("owner-1"); status != http.StatusOK {
		t.Fatalf("voice, lookup error, global on: %d %v", status, out)
	}
	if status, out := m.extBedrock("owner-1"); status != http.StatusOK {
		t.Fatalf("Bedrock, lookup error, audience all: %d %v", status, out)
	}
}

func (m *managedStore) ListCoreLinkRoutesByOwner(owner string) ([]store.CoreLinkRoute, error) {
	var out []store.CoreLinkRoute
	for _, rt := range m.core {
		if rt.OwnerID == owner {
			out = append(out, rt)
		}
	}
	return out, nil
}

func (m *managedStore) ListLinkRouteVoice() ([]store.LinkRouteVoice, error) {
	var out []store.LinkRouteVoice
	for _, v := range m.voice {
		out = append(out, v)
	}
	return out, nil
}

type listFlags struct {
	VoiceAllowed   bool `json:"voice_allowed"`
	BedrockAllowed bool `json:"bedrock_allowed"`
	BedrockActive  bool `json:"bedrock_active"`
}

// The route list's flags follow the decision the enables take: a managed
// account's switches work with the global rules off, so the list says so
// (E2E D2). The panel path keeps the global answer byte for byte.
func TestExternalRouteListFlagsFollowTheManagedExemption(t *testing.T) {
	for _, c := range []struct {
		name    string
		managed bool
		setup   func(m *managedEnv)
		want    listFlags
		status  int
	}{
		{"managed, global off", true, func(*managedEnv) {}, listFlags{true, true, true}, 200},
		{"managed, user deny", true, func(m *managedEnv) {
			m.ms.global = "true"
			m.ms.perms = map[string]bool{"user:owner-1": false}
		}, listFlags{false, true, true}, 200},
		{"not managed", false, func(*managedEnv) {}, listFlags{}, 200},
		{"plan lookup error", true, func(m *managedEnv) { m.ms.limitErr = errors.New("connection reset by peer") }, listFlags{}, 500},
		{"managed, permission read error", true, func(m *managedEnv) { m.ms.permErr = errors.New("connection reset by peer") }, listFlags{}, 500},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newManagedEnv(t, c.managed)
			seedLinkRoute(t, m.e.h.d.Redis, linkDomain, services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
			c.setup(m)
			rec := httptest.NewRecorder()
			m.x.ListRoutes(rec, extReq("GET", "/", "owner-1", false, nil, nil))
			if rec.Code != c.status {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body.String(), c.status)
			}
			if c.status != 200 {
				if extCodeOf(rec) != "internal" {
					t.Fatalf("error answer %s", rec.Body.String())
				}
				return
			}
			var out struct {
				Items []listFlags `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Items) != 1 {
				t.Fatalf("%v %s", err, rec.Body.String())
			}
			if out.Items[0] != c.want {
				t.Fatalf("flags %+v, want %+v", out.Items[0], c.want)
			}
		})
	}
	// The panel never carries the mark: the same managed account lists the
	// global rules' answer.
	m := newManagedEnv(t, true)
	seedLinkRoute(t, m.e.h.d.Redis, linkDomain, services.GatewayRoute{CoreOwned: true, OwnerID: "owner-1"})
	rec := linkCall(m.e, m.e.h.ListLinkRoutes, http.MethodGet, "", "", "owner-1", false)
	var items []listFlags
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &items) != nil || len(items) != 1 || items[0] != (listFlags{}) {
		t.Fatalf("panel list %d %s", rec.Code, rec.Body.String())
	}
}
