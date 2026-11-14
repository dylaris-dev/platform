package database

import (
	"strings"
	"testing"
	"time"

	"dylaris-core/store"
)

// Phase 66 on a database that predates it: a route row from then reads
// custom_domain = false after the migration, and a second boot is a no-op.
// Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationRemoteOwnDomainsSchema(t *testing.T) {
	db, st := integrationDB(t)
	domain := "old-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + ".example.com"
	t.Cleanup(func() { _ = st.DeleteRemoteRoute(domain) })

	// Back to the shape before phase 66, with a row written then.
	if _, err := db.Exec(`ALTER TABLE remote_routes DROP COLUMN IF EXISTS custom_domain`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_custom_domain_claims_domain`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO remote_routes (domain, server_uuid, node_id, port) VALUES ($1, 'srv-old', 1, 25565)`, domain); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := applyRemoteOwnDomains(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	r, err := st.GetRemoteRoute(domain)
	if err != nil || r == nil || r.CustomDomain {
		t.Fatalf("old row = %+v, %v; want custom_domain false", r, err)
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'idx_custom_domain_claims_domain'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("index count %d, err %v", idx, err)
	}
}

// Upsert and claim write the flag, every read returns it, and an upsert can
// clear it again.
func TestIntegrationRemoteRouteKeepsCustomDomainFlag(t *testing.T) {
	_, st := integrationDB(t)
	base := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	own, hoster := "play."+base+".own.net", base+".example.com"
	t.Cleanup(func() {
		_ = st.DeleteRemoteRoute(own)
		_ = st.DeleteRemoteRoute(hoster)
	})

	if ok, _, err := st.ClaimRemoteRoute(store.RemoteRoute{Domain: own, ServerUUID: "srv-a", NodeID: 1, Port: 25565, CustomDomain: true}); err != nil || !ok {
		t.Fatalf("claim = %v, %v", ok, err)
	}
	if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: hoster, ServerUUID: "srv-a", NodeID: 1, Port: 25565}); err != nil {
		t.Fatal(err)
	}
	check := func(stage string, want map[string]bool) {
		t.Helper()
		got, err := st.GetRemoteRoute(own)
		if err != nil || got == nil || got.CustomDomain != want[own] {
			t.Fatalf("%s: get = %+v, %v", stage, got, err)
		}
		for _, list := range [][]store.RemoteRoute{mustRoutes(t, st.ListRemoteRoutes), mustRoutes(t, func() ([]store.RemoteRoute, error) { return st.ListRemoteRoutesByServer("srv-a") })} {
			seen := 0
			for _, r := range list {
				if w, ok := want[r.Domain]; ok {
					seen++
					if r.CustomDomain != w {
						t.Fatalf("%s: listed %+v, want custom %v", stage, r, w)
					}
				}
			}
			if seen != 2 {
				t.Fatalf("%s: saw %d of the two rows", stage, seen)
			}
		}
	}
	check("written", map[string]bool{own: true, hoster: false})

	// A claim that loses hands back the holder with its flag.
	if ok, held, err := st.ClaimRemoteRoute(store.RemoteRoute{Domain: own, ServerUUID: "srv-b", NodeID: 2, Port: 25565}); err != nil || ok || held == nil || !held.CustomDomain {
		t.Fatalf("losing claim = %v, %+v, %v", ok, held, err)
	}
	r, _ := st.GetRemoteRoute(own)
	r.CustomDomain = false
	if err := st.UpsertRemoteRoute(*r); err != nil {
		t.Fatal(err)
	}
	check("cleared by upsert", map[string]bool{own: false, hoster: false})
}

func mustRoutes(t *testing.T, fn func() ([]store.RemoteRoute, error)) []store.RemoteRoute {
	t.Helper()
	rs, err := fn()
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// Every user's claim counts but the excepted one's, and only while it may
// still need the domain: pending or verified.
func TestIntegrationCountLiveCustomDomainClaims(t *testing.T) {
	_, st := integrationDB(t)
	stamp := time.Now().Format("150405.000000000")
	domain := "live-" + stamp + ".own.net"
	users := []string{"live-a-" + stamp, "live-b-" + stamp, "live-c-" + stamp}
	t.Cleanup(func() {
		for _, u := range users {
			_, _ = st.DeleteCustomDomainClaim(u, domain)
		}
		_, _ = st.DeleteCustomDomainClaim(users[0], "other-"+domain)
	})
	count := func(stage, except string, want int) {
		t.Helper()
		if n, err := st.CountLiveCustomDomainClaims(domain, except); err != nil || n != want {
			t.Fatalf("%s (except %q): %d, %v; want %d", stage, except, n, err, want)
		}
	}
	count("no claims", "", 0)

	a, err := st.RequestCustomDomainClaim(users[0], domain)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkCustomDomainVerified(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RequestCustomDomainClaim(users[1], domain); err != nil {
		t.Fatal(err)
	}
	c, err := st.StartCustomDomainClaim(users[2], domain, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.FailCustomDomainClaim(c.ID); err != nil {
		t.Fatal(err)
	}
	// Another domain of the same user does not count.
	if _, err := st.RequestCustomDomainClaim(users[0], "other-"+domain); err != nil {
		t.Fatal(err)
	}

	count("verified + pending, blocked left out", "", 2)
	count("verified one excepted", users[0], 1)
	count("pending one excepted", users[1], 1)
	count("blocked one excepted", users[2], 2)
}

// OpenOutboxDomains lists each domain with an open item of the op once, and
// leaves out finished items and other ops.
func TestIntegrationOpenOutboxDomains(t *testing.T) {
	db, st := integrationDB(t)
	stamp := strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	open, twice, done, other := "open-"+stamp+".own.net", "twice-"+stamp+".own.net", "done-"+stamp+".own.net", "other-"+stamp+".own.net"
	t.Cleanup(func() {
		for _, d := range []string{open, twice, done, other} {
			_, _ = db.Exec(`DELETE FROM gateway_outbox WHERE domain = $1`, d)
		}
	})
	const op = "delete_custom_domain"
	for _, d := range []string{open, twice, twice, done} {
		if err := st.EnqueueOutbox(op, d, struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.EnqueueOutbox("delete", other, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE gateway_outbox SET done_at = NOW() WHERE domain = $1`, done); err != nil {
		t.Fatal(err)
	}
	got, err := st.OpenOutboxDomains(op)
	if err != nil {
		t.Fatal(err)
	}
	if !got[open] || !got[twice] || got[done] || got[other] {
		t.Fatalf("got %v", got)
	}
}

// RequestCustomDomainClaimCreated says whether THIS call inserted the claim:
// the first call does, a second returns the same row and does not.
func TestIntegrationRequestCustomDomainClaimCreated(t *testing.T) {
	_, st := integrationDB(t)
	stamp := time.Now().Format("150405.000000000")
	user, domain := "created-"+stamp, "created-"+stamp+".own.net"
	t.Cleanup(func() { _, _ = st.DeleteCustomDomainClaim(user, domain) })
	a, created, err := st.RequestCustomDomainClaimCreated(user, domain)
	if err != nil || !created || a == nil || a.State != store.ClaimPending {
		t.Fatalf("first: %+v %v %v", a, created, err)
	}
	b, created, err := st.RequestCustomDomainClaimCreated(user, domain)
	if err != nil || created || b == nil || b.ID != a.ID {
		t.Fatalf("second: %+v %v %v, want the same row not created", b, created, err)
	}
}
