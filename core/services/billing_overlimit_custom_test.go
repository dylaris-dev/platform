package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	beamauth "dylaris-pkg/beam/auth"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/models"
	"dylaris-core/store"
)

// customSweepStore adds what the own-domain count reads to the over-limit fake:
// the custom-domain cap rows and settings, the durable route-only rows and the
// servers that tie a managed route to its owner.
type customSweepStore struct {
	*overLimitStore
	settings    map[string]string
	settingErrs map[string]error
	domains     map[string]*int64 // a present key is a row; nil value = NULL row
	domainErr   error             // returned for every scope instead of a row
	linkRoutes  []store.CoreLinkRoute
	servers     []models.Server
	admin       bool
}

func (f *customSweepStore) GetSetting(k string) (string, error) {
	if err := f.settingErrs[k]; err != nil {
		return "", err
	}
	if v, ok := f.settings[k]; ok {
		return v, nil
	}
	return "", sql.ErrNoRows // what PostgresStore.GetSetting says for a key never saved
}
func (f *customSweepStore) GetGatewayCustomDomainLimit(scope string) (*models.GatewayCustomDomainLimit, error) {
	if f.domainErr != nil {
		return nil, f.domainErr
	}
	if v, ok := f.domains[scope]; ok {
		return &models.GatewayCustomDomainLimit{Scope: scope, MaxCustomDomains: v}, nil
	}
	return nil, sql.ErrNoRows
}
func (f *customSweepStore) ListCoreLinkRoutesByOwner(string) ([]store.CoreLinkRoute, error) {
	return f.linkRoutes, nil
}
func (f *customSweepStore) ListServersByOwner(string) ([]models.Server, error) { return f.servers, nil }
func (f *customSweepStore) GetUserByID(id string) (*models.User, error) {
	return &models.User{ID: id, Username: "tenant", Email: "tenant@example.com", IsAdmin: f.admin}, nil
}

// publishRoute puts one route where GetRoutesFromRedis reads it.
func publishRoute(t *testing.T, rdb *redis.Client, rt GatewayRoute) {
	t.Helper()
	b, _ := json.Marshal(rt)
	ctx := context.Background()
	if err := rdb.Set(ctx, "route:"+rt.Domain, b, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.SAdd(ctx, "sys:index:routes", rt.Domain).Err(); err != nil {
		t.Fatal(err)
	}
}

// mailSink is a Resend endpoint that records the text of every mail.
type mailSink struct {
	mu    sync.Mutex
	texts []string
}

func newMailSink(t *testing.T) (*mailSink, string) {
	m := &mailSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(b, &p)
		m.mu.Lock()
		m.texts = append(m.texts, p.Text)
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(srv.Close)
	return m, srv.URL
}

func (m *mailSink) sent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.texts...)
}

const sweepHoster = `[{"domain":"eu.dylaris.com","validation":"letters"}]`

// sweepCase is one tenant on one pass.
type sweepCase struct {
	storeEnabled bool // false is self-host
	admin        bool
	capSince     *time.Time // user_billing.custom_domain_cap_since
	since        *time.Time // user_billing.overlimit_since
	hoster       *string    // nil = sweepHoster
	setting      string     // gateway_max_custom_domains, "" = never saved (default 50)
	settingErrs  map[string]error
	domains      map[string]*int64
	domainErr    error
	kits         int // link kits held; 1 is allowed
	own          int // route-only routes on their own domains (durable rows)
	ownManaged   int // managed server routes on their own domains (Redis)
	ours         int // route-only routes on our domains
}

type sweepResult struct {
	stamps []overLimitStamp
	mails  []string
	cutoff bool
}

func runSweep(t *testing.T, c sweepCase) sweepResult {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	sink, url := newMailSink(t)
	row := billingRow("u1", c.since, "active")
	row.CustomDomainCapSince = c.capSince
	hoster := sweepHoster
	if c.hoster != nil {
		hoster = *c.hoster
	}
	fs := &customSweepStore{
		overLimitStore: &overLimitStore{billing: []store.UserBilling{row}, kits: c.kits},
		settings: map[string]string{
			HosterDomainsSettingKey:   hoster,
			"mail.provider":           "resend",
			"resend.api_key":          "k",
			"resend.endpoint":         url,
			"smtp.default.from_email": "panel@example.com",
		},
		settingErrs: c.settingErrs,
		domains:     c.domains,
		domainErr:   c.domainErr,
		admin:       c.admin,
		servers:     []models.Server{{ID: 1, UUID: "srv-1", Status: "stopped", DesiredState: "stopped"}},
	}
	if c.setting != "" {
		fs.settings[SettingMaxCustomDomains] = c.setting
	}
	for i := 0; i < c.own; i++ {
		d := fmt.Sprintf("play%d.example.com", i)
		fs.linkRoutes = append(fs.linkRoutes, store.CoreLinkRoute{Domain: d, OwnerID: "u1"})
		publishRoute(t, rdb, GatewayRoute{Domain: d, CoreOwned: true, OwnerID: "u1"})
	}
	for i := 0; i < c.ours; i++ {
		d := fmt.Sprintf("ours%d.eu.dylaris.com", i)
		fs.linkRoutes = append(fs.linkRoutes, store.CoreLinkRoute{Domain: d, OwnerID: "u1"})
		publishRoute(t, rdb, GatewayRoute{Domain: d, CoreOwned: true, OwnerID: "u1"})
	}
	for i := 0; i < c.ownManaged; i++ {
		publishRoute(t, rdb, GatewayRoute{Domain: fmt.Sprintf("mc%d.example.org", i), ServerUUID: "srv-1"})
	}
	// Somebody else's route on their own domain never counts.
	publishRoute(t, rdb, GatewayRoute{Domain: "other.example.net", CoreOwned: true, OwnerID: "u2"})
	// ours counts against the address pool: give it room.
	fs.routeLimit = new(int)
	*fs.routeLimit = 100

	s := &BillingLifecycleService{store: fs, redis: rdb, storeEnabled: c.storeEnabled}
	s.enforceEntitlementLimits(context.Background())
	// The cutoff's first step stamps every server's beam access epoch.
	return sweepResult{stamps: fs.stamps, mails: sink.sent(), cutoff: mr.Exists(beamauth.AccessEpochKey("srv-1"))}
}

func (r sweepResult) stamp(t *testing.T) string {
	t.Helper()
	switch {
	case len(r.stamps) > 1:
		t.Fatalf("stamps = %+v", r.stamps)
	case len(r.stamps) == 1 && r.stamps[0].at != nil:
		return "set"
	case len(r.stamps) == 1:
		return "clear"
	}
	return ""
}

// The sweep counts own-domain routes only for a tenant whose managed plan
// ended (custom_domain_cap_since set), then warns and, after the grace, takes
// the same cutoff it takes for too many links.
func TestOverLimitSweepCountsOwnDomainRoutes(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	ended := time.Now().Add(-100 * time.Hour) // before every stamp below
	stale := time.Now().Add(-OverLimitGrace - time.Hour)
	recent := time.Now().Add(-time.Hour)
	cases := []struct {
		name       string
		c          sweepCase
		wantStamp  string // "set", "clear", "" = no write
		wantMail   bool
		wantCutoff bool
	}{
		{name: "under the default cap: nothing", c: sweepCase{storeEnabled: true, capSince: &ended, own: 3, ours: 1}},
		{name: "at the cap is not over", c: sweepCase{storeEnabled: true, capSince: &ended, setting: "3", own: 2, ownManaged: 1}},
		{name: "over the cap after the plan ended: warned",
			c: sweepCase{storeEnabled: true, capSince: &ended, setting: "2", own: 2, ownManaged: 1}, wantStamp: "set", wantMail: true},
		{name: "still over inside the grace: nothing new", c: sweepCase{storeEnabled: true, capSince: &ended, setting: "2", own: 3, since: &recent}},
		{name: "still over after the grace: the existing cutoff",
			c: sweepCase{storeEnabled: true, capSince: &ended, setting: "2", own: 3, since: &stale}, wantCutoff: true},
		{name: "back under clears the clock", c: sweepCase{storeEnabled: true, capSince: &ended, setting: "2", own: 2, since: &recent}, wantStamp: "clear"},
		{name: "routes on our domains do not count against it", c: sweepCase{storeEnabled: true, capSince: &ended, setting: "1", own: 1, ours: 5}},
		{name: "a user row beats the default",
			c: sweepCase{storeEnabled: true, capSince: &ended, domains: map[string]*int64{"user:u1": n(1)}, own: 2}, wantStamp: "set", wantMail: true},
		{name: "cap 0: any own-domain route is over",
			c: sweepCase{storeEnabled: true, capSince: &ended, domains: map[string]*int64{"user:u1": n(0)}, ownManaged: 1}, wantStamp: "set", wantMail: true},
		{name: "cap 0 and none held: not over", c: sweepCase{storeEnabled: true, capSince: &ended, domains: map[string]*int64{"user:u1": n(0)}, ours: 2}},
		{name: "unlimited row: never over", c: sweepCase{storeEnabled: true, capSince: &ended, setting: "1", domains: map[string]*int64{"user:u1": nil}, own: 40, ownManaged: 40}},
		{name: "unlimited setting: never over", c: sweepCase{storeEnabled: true, capSince: &ended, setting: "unlimited", own: 400}},
		{name: "self-host: never counted", c: sweepCase{storeEnabled: false, capSince: &ended, setting: "1", own: 9}},
		{name: "an administrator skips the cap, as at the doors", c: sweepCase{storeEnabled: true, admin: true, capSince: &ended, setting: "1", own: 9}},

		// No marker: exactly the sweep before own-domain routes were counted.
		{name: "no marker: over the default cap is not counted", c: sweepCase{storeEnabled: true, own: 80, ownManaged: 20}},
		{name: "no marker: a lowered setting is not counted", c: sweepCase{storeEnabled: true, setting: "0", own: 5}},
		{name: "no marker: a lowered user_default row is not counted", c: sweepCase{storeEnabled: true, domains: map[string]*int64{"user_default": n(0)}, own: 5}},
		{name: "no marker: no hoster domains configured counts nothing", c: sweepCase{storeEnabled: true, hoster: new(string), setting: "1", ours: 5}},
		{name: "no marker: an unreadable hoster list changes nothing",
			c: sweepCase{storeEnabled: true, settingErrs: map[string]error{HosterDomainsSettingKey: errors.New("db down")}, setting: "1", ours: 5}},
		{name: "no marker: too many links still warned as before", c: sweepCase{storeEnabled: true, kits: 2, own: 80}, wantStamp: "set", wantMail: true},
		{name: "no marker: too many links still cut off as before", c: sweepCase{storeEnabled: true, kits: 2, own: 80, since: &stale}, wantCutoff: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runSweep(t, tc.c)
			if got := r.stamp(t); got != tc.wantStamp {
				t.Fatalf("stamp = %q, want %q", got, tc.wantStamp)
			}
			if (len(r.mails) == 1) != tc.wantMail || len(r.mails) > 1 {
				t.Fatalf("mails = %q, want one: %v", r.mails, tc.wantMail)
			}
			if r.cutoff != tc.wantCutoff {
				t.Fatalf("cutoff = %v, want %v", r.cutoff, tc.wantCutoff)
			}
			if tc.wantMail && tc.c.kits == 0 && !strings.Contains(r.mails[0], "on your own domains") {
				t.Fatalf("the warning does not name own-domain routes: %q", r.mails[0])
			}
		})
	}
}

// A read that fails must skip the tenant for the pass: no stamp, no mail and
// no cutoff, never a cap nobody set.
func TestOverLimitSweepSkipsATenantOnAReadError(t *testing.T) {
	ended := time.Now().Add(-100 * time.Hour)
	stale := time.Now().Add(-OverLimitGrace - time.Hour)
	boom := errors.New("db down")
	bad := "{not json"
	for name, c := range map[string]sweepCase{
		"custom-domain limit row unreadable": {domainErr: boom},
		"cap setting unreadable":             {settingErrs: map[string]error{SettingMaxCustomDomains: boom}},
		"hoster list unreadable":             {settingErrs: map[string]error{HosterDomainsSettingKey: boom}},
		"hoster list invalid JSON":           {hoster: &bad},
		"unreadable, and stamped long ago":   {settingErrs: map[string]error{SettingMaxCustomDomains: boom}, since: &stale},
	} {
		t.Run(name, func(t *testing.T) {
			c.storeEnabled, c.capSince, c.setting, c.own, c.ours = true, &ended, "1", 60, 5 // over 1 and over the default 50
			r := runSweep(t, c)
			if len(r.stamps) != 0 || len(r.mails) != 0 || r.cutoff {
				t.Fatalf("stamps=%+v mails=%d cutoff=%v, want nothing", r.stamps, len(r.mails), r.cutoff)
			}
		})
	}
}

// Stamped for links at T0, the managed plan ends and own-domain routes are over
// at T0+60h: the clock restarts with a mail naming both, so fixing the links at
// T0+70h does not cut the account off at T0+72h.
func TestOwnDomainOverageRestartsAClockStartedForSomethingElse(t *testing.T) {
	now := time.Now()
	t0 := now.Add(-60 * time.Hour)
	ended := now.Add(-time.Hour) // the plan ended at T0+59h
	r := runSweep(t, sweepCase{storeEnabled: true, capSince: &ended, since: &t0, kits: 2, setting: "1", own: 3})
	if len(r.stamps) != 1 || r.stamps[0].at == nil || r.stamps[0].at.Before(now) {
		t.Fatalf("stamps = %+v, want one restart at now", r.stamps)
	}
	if len(r.mails) != 1 || !strings.Contains(r.mails[0], "route-only locations (allowed 1)") ||
		!strings.Contains(r.mails[0], "3 addresses on your own domains (allowed 1)") {
		t.Fatalf("mails = %q, want one naming both limits", r.mails)
	}
	if r.cutoff {
		t.Fatal("a restart cut the account off")
	}

	// T0+72h: the restarted stamp is 12h old, links fixed, own domains still over.
	restarted := now.Add(-12 * time.Hour)
	endedThen := now.Add(-13 * time.Hour)
	r = runSweep(t, sweepCase{storeEnabled: true, capSince: &endedThen, since: &restarted, kits: 1, setting: "1", own: 3})
	if len(r.stamps) != 0 || len(r.mails) != 0 || r.cutoff {
		t.Fatalf("at T0+72h: stamps=%+v mails=%d cutoff=%v, want nothing (cutoff at T0+132h)", r.stamps, len(r.mails), r.cutoff)
	}

	// Own domains within the cap: a clock from before the plan ended is not
	// restarted; it keeps its old deadline exactly as before.
	r = runSweep(t, sweepCase{storeEnabled: true, capSince: &ended, since: &t0, kits: 2, setting: "5", own: 3})
	if len(r.stamps) != 0 || len(r.mails) != 0 {
		t.Fatalf("not over on own domains: stamps=%+v mails=%d, want nothing", r.stamps, len(r.mails))
	}
}

// The warning has to say which limit it is about.
func TestDescribeNamesOwnDomainRoutes(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	u := tenantUsage{entitledByon: true, entitledRoute: true, custom: 7, customLimit: n(5)}
	if !u.over() {
		t.Fatal("7 own-domain routes against 5 is not over")
	}
	if got, want := u.describe(), "7 addresses on your own domains (allowed 5)"; got != want {
		t.Fatalf("describe() = %q, want %q", got, want)
	}
	// Not over: not named.
	u = tenantUsage{entitledByon: true, entitledRoute: true, nodes: 3, nodeLimit: n(2), custom: 5, customLimit: n(5)}
	if got := u.describe(); strings.Contains(got, "own domains") {
		t.Fatalf("describe() = %q names a limit that is not exceeded", got)
	}
	// nil is no cap: never over, however many.
	if (tenantUsage{entitledByon: true, entitledRoute: true, custom: 1 << 20}).over() {
		t.Fatal("no cap read as over")
	}
}

// The doors keep reading a failed lookup as "no row" and the default; only the
// sweep's variant reports it.
func TestCustomDomainCapForSweepReportsReadErrors(t *testing.T) {
	boom := errors.New("db down")
	fs := &customSweepStore{overLimitStore: &overLimitStore{}, domainErr: boom, settings: map[string]string{}}
	if l, _ := CustomDomainCapFor(fs, "u1"); l == nil || *l != 50 {
		t.Fatalf("door cap = %v, want the default 50 as before", l)
	}
	if _, err := CustomDomainCapForSweep(fs, "u1"); !errors.Is(err, boom) {
		t.Fatalf("sweep cap err = %v, want the read error", err)
	}
	fs = &customSweepStore{overLimitStore: &overLimitStore{}, settings: map[string]string{}}
	if l, err := CustomDomainCapForSweep(fs, "u1"); err != nil || l == nil || *l != 50 {
		t.Fatalf("no rows, no setting: %v, %v; want 50", l, err)
	}
}

// A NULL settings value is "never saved", not a read error: the cap falls back
// to the default and the hoster list to none, as on the doors' lax path.
// Skipping instead would keep a marked tenant skipped while a stamp ages.
func TestOverLimitSweepReadsANullSettingAsUnset(t *testing.T) {
	ended := time.Now().Add(-100 * time.Hour)
	r := runSweep(t, sweepCase{storeEnabled: true, capSince: &ended, own: 60,
		settingErrs: map[string]error{SettingMaxCustomDomains: store.ErrSettingNull}})
	if r.stamp(t) != "set" || len(r.mails) != 1 {
		t.Fatalf("NULL cap setting: stamps=%+v mails=%d, want the default 50 to answer (60 is over)", r.stamps, len(r.mails))
	}
	r = runSweep(t, sweepCase{storeEnabled: true, capSince: &ended, setting: "1", own: 3,
		settingErrs: map[string]error{HosterDomainsSettingKey: store.ErrSettingNull}})
	if r.stamp(t) != "set" || len(r.mails) != 1 {
		t.Fatalf("NULL hoster list: stamps=%+v mails=%d, want it read as no list", r.stamps, len(r.mails))
	}
	fs := &customSweepStore{overLimitStore: &overLimitStore{}, settings: map[string]string{},
		settingErrs: map[string]error{SettingMaxCustomDomains: store.ErrSettingNull}}
	if l, err := CustomDomainCapForSweep(fs, "u1"); err != nil || l == nil || *l != 50 {
		t.Fatalf("NULL setting: %v, %v; want the default 50", l, err)
	}
}

// Valid JSON that is not a list parses to no bases in HosterBaseDomains, which
// would make every address on our domains count as the tenant's own.
func TestOverLimitSweepSkipsAHosterListThatIsNotAList(t *testing.T) {
	ended := time.Now().Add(-100 * time.Hour)
	for _, v := range []string{`{"domain":"eu.dylaris.com"}`, `"eu.dylaris.com"`, `null`} {
		t.Run(v, func(t *testing.T) {
			v := v
			r := runSweep(t, sweepCase{storeEnabled: true, capSince: &ended, hoster: &v, setting: "1", own: 1, ours: 5})
			if len(r.stamps) != 0 || len(r.mails) != 0 || r.cutoff {
				t.Fatalf("stamps=%+v mails=%d cutoff=%v, want the tenant skipped", r.stamps, len(r.mails), r.cutoff)
			}
		})
	}
	// An empty list is a real answer (no hoster domains), as before.
	empty := `[]`
	if r := runSweep(t, sweepCase{storeEnabled: true, capSince: &ended, hoster: &empty, setting: "1", own: 3}); r.stamp(t) != "set" {
		t.Fatalf("empty list: stamps=%+v, want a stamp", r.stamps)
	}
}

// A skipped pass writes nothing, not even the clear a lax read would have
// produced by coming out under the cap.
func TestOverLimitSweepSkipDoesNotClearAStamp(t *testing.T) {
	ended := time.Now().Add(-100 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	boom := errors.New("db down")
	bad := `{"domain":"eu.dylaris.com"}`
	for name, c := range map[string]sweepCase{
		// lax: walks on to the "unlimited" setting, not over -> clear
		"limit row unreadable": {domainErr: boom, setting: "unlimited", own: 3},
		// lax: the default 50, 3 is under -> clear
		"cap setting unreadable": {settingErrs: map[string]error{SettingMaxCustomDomains: boom}, own: 3},
		// lax: no bases, but the cap is unlimited -> clear
		"hoster list not a list": {hoster: &bad, setting: "unlimited", own: 3},
	} {
		t.Run(name, func(t *testing.T) {
			c.storeEnabled, c.capSince, c.since = true, &ended, &recent
			r := runSweep(t, c)
			if len(r.stamps) != 0 || len(r.mails) != 0 || r.cutoff {
				t.Fatalf("stamps=%+v mails=%d cutoff=%v, want no write at all", r.stamps, len(r.mails), r.cutoff)
			}
		})
	}
}
