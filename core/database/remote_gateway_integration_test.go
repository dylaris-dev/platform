package database

import (
	"strings"
	"testing"
	"time"

	"dylaris-core/store"
)

// A fresh install has the three remote-gateway tables and a second boot of the
// migration is a no-op. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationRemoteGatewayTables(t *testing.T) {
	db, _ := integrationDB(t)
	for _, tbl := range []string{"remote_link_kits", "remote_routes", "gateway_outbox"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1`, tbl).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s: count %d, err %v", tbl, n, err)
		}
	}
	if err := applyRemoteGateway(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func TestIntegrationRemoteKitKeyIsSealedAtRest(t *testing.T) {
	db, st := integrationDB(t)
	st.SetRemoteKitEncryptionKey("cluster-secret-a")
	f := newFixture(t, st)
	id := uint(f.node.ID)
	t.Cleanup(func() { _ = st.DeleteRemoteKit(id) })

	if k, err := st.GetRemoteKit(id); err != nil || k != nil {
		t.Fatalf("GetRemoteKit before save = %+v, %v", k, err)
	}
	const plain = "LINK-KEY-PLAINTEXT-1234567890"
	linkID := "lk_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if err := st.SaveRemoteKit(store.RemoteKit{NodeID: id, LinkID: linkID, LinkKey: plain, Region: "eu"}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.QueryRow(`SELECT link_key FROM remote_link_kits WHERE node_id = $1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || strings.Contains(raw, plain) {
		t.Fatalf("raw column holds the plaintext: %q", raw)
	}
	k, err := st.GetRemoteKit(id)
	if err != nil || k == nil || k.LinkKey != plain || k.LinkID != linkID || k.Region != "eu" {
		t.Fatalf("GetRemoteKit = %+v, %v", k, err)
	}
	// Saving again replaces rather than duplicates.
	if err := st.SaveRemoteKit(store.RemoteKit{NodeID: id, LinkID: linkID, LinkKey: "rolled", Region: "eu"}); err != nil {
		t.Fatal(err)
	}
	if k, _ := st.GetRemoteKit(id); k == nil || k.LinkKey != "rolled" {
		t.Fatalf("after re-save: %+v", k)
	}
	if err := st.DeleteRemoteKit(id); err != nil {
		t.Fatal(err)
	}
	if k, err := st.GetRemoteKit(id); err != nil || k != nil {
		t.Fatalf("after delete: %+v, %v", k, err)
	}
}

// InsertRemoteKit is the race guard of EnsureKit: the second insert for one
// node reports false and leaves the first kit in place.
func TestIntegrationRemoteKitInsertKeepsFirst(t *testing.T) {
	_, st := integrationDB(t)
	st.SetRemoteKitEncryptionKey("cluster-secret-a")
	f := newFixture(t, st)
	id := uint(f.node.ID)
	t.Cleanup(func() { _ = st.DeleteRemoteKit(id) })

	base := "lk_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if ok, err := st.InsertRemoteKit(store.RemoteKit{NodeID: id, LinkID: base + "_a", LinkKey: "key-a"}); err != nil || !ok {
		t.Fatalf("first insert = %v, %v", ok, err)
	}
	if ok, err := st.InsertRemoteKit(store.RemoteKit{NodeID: id, LinkID: base + "_b", LinkKey: "key-b"}); err != nil || ok {
		t.Fatalf("second insert = %v, %v; want false, nil", ok, err)
	}
	if k, err := st.GetRemoteKit(id); err != nil || k == nil || k.LinkID != base+"_a" || k.LinkKey != "key-a" {
		t.Fatalf("stored kit = %+v, %v", k, err)
	}
}

func TestIntegrationRemoteRoutesRoundTrip(t *testing.T) {
	_, st := integrationDB(t)
	dom := "rt-" + uniqueName("d") + ".example.test"
	srv := "11111111-1111-4111-8111-" + uniqueName("s")
	t.Cleanup(func() { _ = st.DeleteRemoteRoute(dom) })

	r := store.RemoteRoute{Domain: dom, ServerUUID: srv, NodeID: 3, Port: 25565}
	if err := st.UpsertRemoteRoute(r); err != nil {
		t.Fatal(err)
	}
	r.Port, r.ProxyProtocol = 25570, true
	if err := st.UpsertRemoteRoute(r); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListRemoteRoutesByServer(srv)
	if len(got) == 1 {
		if got[0].UpdatedAt.IsZero() {
			t.Fatal("updated_at not read")
		}
		got[0].UpdatedAt = time.Time{}
	}
	if err != nil || len(got) != 1 || got[0] != r {
		t.Fatalf("ListRemoteRoutesByServer = %+v, %v", got, err)
	}
	if err := st.DeleteRemoteRoute(dom); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.ListRemoteRoutesByServer(srv); len(got) != 0 {
		t.Fatalf("after delete: %+v", got)
	}
}

// Two claims at the same instant must never hand out the same row: the claim
// pushes next_attempt_at past the lease in the same statement.
func TestIntegrationOutboxClaimIsExclusive(t *testing.T) {
	db, st := integrationDB(t)
	if _, err := db.Exec(`DELETE FROM gateway_outbox`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := st.EnqueueOutbox("create_route", "d"+string(rune('a'+i))+".example.test", map[string]any{"i": i}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	a, err := st.ClaimDueOutbox(now, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.ClaimDueOutbox(now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("claims returned %d and %d rows, want 2 and 2", len(a), len(b))
	}
	seen := map[int64]bool{}
	for _, it := range append(a, b...) {
		if seen[it.ID] {
			t.Fatalf("row %d was claimed twice", it.ID)
		}
		seen[it.ID] = true
	}
	if a[0].ID >= a[1].ID || strings.ReplaceAll(string(a[0].Payload), " ", "") != `{"i":0}` {
		t.Errorf("claim order/payload: %+v", a)
	}
	if rest, _ := st.ClaimDueOutbox(now, 10); len(rest) != 0 {
		t.Fatalf("a third claim got %d rows", len(rest))
	}
	n, oldest, err := st.CountOpenOutbox()
	if err != nil || n != 4 || oldest.IsZero() {
		t.Fatalf("CountOpenOutbox = %d, %v, %v", n, oldest, err)
	}
	for _, it := range a {
		if err := st.FinishOutbox(it.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n, _, _ := st.CountOpenOutbox(); n != 2 {
		t.Fatalf("open after finishing two = %d, want 2", n)
	}
}

func TestIntegrationOutboxFailBacksOff(t *testing.T) {
	db, st := integrationDB(t)
	if _, err := db.Exec(`DELETE FROM gateway_outbox`); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueOutbox("delete_route", "x.example.test", struct{}{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	got, err := st.ClaimDueOutbox(now, 5)
	if err != nil || len(got) != 1 || got[0].Attempts != 0 {
		t.Fatalf("first claim = %+v, %v", got, err)
	}
	if err := st.FailOutbox(got[0].ID, "429 rate limited", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.ClaimDueOutbox(now.Add(10*time.Minute), 5); len(again) != 0 {
		t.Fatalf("row came back before its backoff: %+v", again)
	}
	again, err := st.ClaimDueOutbox(now.Add(2*time.Hour), 5)
	if err != nil || len(again) != 1 {
		t.Fatalf("row not due after the backoff: %+v, %v", again, err)
	}
	if again[0].Attempts != 1 || again[0].LastError != "429 rate limited" {
		t.Errorf("attempts/last_error = %d / %q", again[0].Attempts, again[0].LastError)
	}
}

// The drain's per-domain order, its no-attempt hand-back and the purge.
func TestIntegrationOutboxOrderDeferPurge(t *testing.T) {
	db, st := integrationDB(t)
	if _, err := db.Exec(`DELETE FROM gateway_outbox`); err != nil {
		t.Fatal(err)
	}
	if id, err := st.OldestOpenOutboxID("a.example.test"); err != nil || id != 0 {
		t.Fatalf("empty: %d, %v", id, err)
	}
	for _, d := range []string{"a.example.test", "b.example.test", "a.example.test"} {
		if err := st.EnqueueOutbox("move", d, struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	// The newer a.example.test row waits behind the older one and is not
	// claimed; the unrelated domain is.
	got, err := st.ClaimDueOutbox(now, 10)
	if err != nil || len(got) != 2 || got[0].Domain != "a.example.test" || got[1].Domain != "b.example.test" {
		t.Fatalf("claim = %+v, %v; want the first a and the b", got, err)
	}
	first := got[0]
	if id, err := st.OldestOpenOutboxID("a.example.test"); err != nil || id != first.ID {
		t.Fatalf("oldest = %d, %v; want %d", id, err, first.ID)
	}
	if err := st.FinishOutbox(first.ID); err != nil {
		t.Fatal(err)
	}
	third, err := st.OldestOpenOutboxID("a.example.test")
	if err != nil || third <= first.ID {
		t.Fatalf("oldest after finish = %d, %v", third, err)
	}
	next, err := st.ClaimDueOutbox(now, 10)
	if err != nil || len(next) != 1 || next[0].ID != third {
		t.Fatalf("claim after the head finished = %+v, %v; want id %d", next, err, third)
	}
	if err := st.DeferOutbox(third, now); err != nil {
		t.Fatal(err)
	}
	again, err := st.ClaimDueOutbox(now, 10)
	if err != nil || len(again) != 1 || again[0].ID != third || again[0].Attempts != 0 {
		t.Fatalf("deferred row: %+v, %v; want id %d due now with 0 attempts", again, err, third)
	}
	if n, err := st.PurgeDoneOutbox(now.Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("purge of nothing old = %d, %v", n, err)
	}
	if n, err := st.PurgeDoneOutbox(now.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v; want the one finished row", n, err)
	}
	if n, _, _ := st.CountOpenOutbox(); n != 2 {
		t.Fatalf("open after purge = %d, want 2", n)
	}
}

func TestIntegrationRemoteRouteGet(t *testing.T) {
	_, st := integrationDB(t)
	dom := "get-" + uniqueName("d") + ".example.test"
	t.Cleanup(func() { _ = st.DeleteRemoteRoute(dom) })
	if r, err := st.GetRemoteRoute(dom); err != nil || r != nil {
		t.Fatalf("absent = %+v, %v", r, err)
	}
	want := store.RemoteRoute{Domain: dom, ServerUUID: "s", NodeID: 4, Port: 25565, ProxyProtocol: true}
	if err := st.UpsertRemoteRoute(want); err != nil {
		t.Fatal(err)
	}
	if r, err := st.GetRemoteRoute(dom); err != nil || r == nil || noStamp(*r) != want {
		t.Fatalf("got %+v, %v; want %+v", r, err, want)
	}
}

func TestIntegrationOutboxHasOpen(t *testing.T) {
	db, st := integrationDB(t)
	if _, err := db.Exec(`DELETE FROM gateway_outbox`); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueOutbox("delete", "open.example.test", struct{}{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		domain, op string
		want       bool
	}{
		{"open.example.test", "", true},
		{"open.example.test", "delete", true},
		{"open.example.test", "move", false},
		{"other.example.test", "", false},
	} {
		if got, err := st.HasOpenOutbox(tc.domain, tc.op); err != nil || got != tc.want {
			t.Errorf("HasOpenOutbox(%q, %q) = %v, %v; want %v", tc.domain, tc.op, got, err, tc.want)
		}
	}
	got, err := st.ClaimDueOutbox(time.Now(), 5)
	if err != nil || len(got) != 1 {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	if err := st.FinishOutbox(got[0].ID); err != nil {
		t.Fatal(err)
	}
	if open, err := st.HasOpenOutbox("open.example.test", ""); err != nil || open {
		t.Fatalf("after finish = %v, %v; want false", open, err)
	}
}

// A kit revoke is not a route change: the "N changes waiting" count leaves it out.
func TestIntegrationCountOpenOutboxSkipsKitRevokes(t *testing.T) {
	db, st := integrationDB(t)
	if _, err := db.Exec(`DELETE FROM gateway_outbox`); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueOutbox("revoke_kit", "link-abc", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if n, oldest, err := st.CountOpenOutbox(); err != nil || n != 0 || !oldest.IsZero() {
		t.Fatalf("only a revoke open: %d, %v, %v; want 0 and no oldest", n, oldest, err)
	}
	if err := st.EnqueueOutbox("delete", "count.example.test", struct{}{}); err != nil {
		t.Fatal(err)
	}
	if n, _, err := st.CountOpenOutbox(); err != nil || n != 1 {
		t.Fatalf("with a delete: %d, %v; want 1", n, err)
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'idx_gateway_outbox_open_domain'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("open-domain index: %d, %v", idx, err)
	}
}

// Only open route changes the provider refused (a 4xx other than 429) are
// listed, newest first, capped; waits, done rows and kit revokes are not.
func TestIntegrationListRefusedOutbox(t *testing.T) {
	db, st := integrationDB(t)
	if _, err := db.Exec(`DELETE FROM gateway_outbox`); err != nil {
		t.Fatal(err)
	}
	refusal := "gateway provider answered 409 proxy_protocol_disabled: off"
	for _, c := range []struct{ op, domain, lastErr string }{
		{"proxy", "old.example.test", refusal},
		{"proxy", "wait.example.test", "gateway provider answered 429 rate_limited: slow"},
		{"move", "net.example.test", "remotegw: provider unreachable: dial tcp: connection refused"},
		{"revoke_kit", "link-abc", "gateway provider answered 403 forbidden: no"},
		{"proxy", "done.example.test", refusal},
		{"move", "new.example.test", "gateway provider answered 403 kit_retiring: retiring"},
	} {
		if err := st.EnqueueOutbox(c.op, c.domain, struct{}{}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE gateway_outbox SET last_error = $1 WHERE domain = $2`, c.lastErr, c.domain); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE gateway_outbox SET done_at = NOW() WHERE domain = 'done.example.test'`); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListRefusedOutbox(5)
	if err != nil || len(got) != 2 || got[0].Domain != "new.example.test" || got[1].Domain != "old.example.test" ||
		got[1].Op != "proxy" || got[1].LastError != refusal {
		t.Fatalf("refused = %+v, %v", got, err)
	}
	if got, err := st.ListRefusedOutbox(1); err != nil || len(got) != 1 || got[0].Domain != "new.example.test" {
		t.Fatalf("capped = %+v, %v", got, err)
	}
}

// The claim is the only arbiter of an address between the servers of one
// customer Core: a second server never takes a held domain, and the first
// keeps its row untouched.
func TestIntegrationRemoteRouteClaim(t *testing.T) {
	_, st := integrationDB(t)
	dom := "claim-" + uniqueName("d") + ".example.test"
	t.Cleanup(func() { _ = st.DeleteRemoteRoute(dom) })
	a := store.RemoteRoute{Domain: dom, ServerUUID: "server-a", NodeID: 4, Port: 25565}
	claimed, existing, err := st.ClaimRemoteRoute(a)
	if err != nil || !claimed || existing != nil {
		t.Fatalf("first claim = %v, %+v, %v", claimed, existing, err)
	}
	b := store.RemoteRoute{Domain: dom, ServerUUID: "server-b", NodeID: 5, Port: 25570}
	claimed, existing, err = st.ClaimRemoteRoute(b)
	if err != nil || claimed || existing == nil || noStamp(*existing) != a {
		t.Fatalf("second claim = %v, %+v, %v; want the held row %+v", claimed, existing, err, a)
	}
	if r, err := st.GetRemoteRoute(dom); err != nil || r == nil || noStamp(*r) != a {
		t.Fatalf("row after the refused claim = %+v, %v; want %+v", r, err, a)
	}
}

func noStamp(r store.RemoteRoute) store.RemoteRoute {
	r.UpdatedAt = time.Time{}
	return r
}

func TestIntegrationGetCoreLinkRoute(t *testing.T) {
	_, st := integrationDB(t)
	dom := "get." + uniqueName("clr_") + ".example.test"
	if r, err := st.GetCoreLinkRoute(dom); err != nil || r != nil {
		t.Fatalf("absent = %+v, %v", r, err)
	}
	want := store.CoreLinkRoute{Domain: dom, OwnerID: "owner-x", LinkToken: "t", TargetHost: "127.0.0.1", TargetPort: 25565}
	if err := st.UpsertCoreLinkRoute(want); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.DeleteCoreLinkRoute(dom) })
	at := time.Now()
	if _, err := st.SetCoreLinkRouteSuspended(dom, &at, "abuse"); err != nil {
		t.Fatal(err)
	}
	r, err := st.GetCoreLinkRoute(dom)
	if err != nil || r == nil || r.OwnerID != "owner-x" || r.TargetPort != 25565 || r.SuspendedAt == nil || r.SuspendedReason != "abuse" {
		t.Fatalf("got %+v, %v", r, err)
	}
	all, err := st.ListCoreLinkRoutes()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range all {
		if row.Domain == dom && !reflectEqualCoreRoute(row, *r) {
			t.Fatalf("by domain %+v differs from the list's %+v", *r, row)
		}
	}
}

func reflectEqualCoreRoute(a, b store.CoreLinkRoute) bool {
	return a.Domain == b.Domain && a.OwnerID == b.OwnerID && a.LinkToken == b.LinkToken && a.TargetHost == b.TargetHost &&
		a.TargetPort == b.TargetPort && a.ProxyProtocol == b.ProxyProtocol && a.CreatedAt.Equal(b.CreatedAt) &&
		(a.SuspendedAt == nil) == (b.SuspendedAt == nil) && a.SuspendedReason == b.SuspendedReason
}

// remote_link_kits.retire_at: NULL for a kit in use, stamped only on the kit
// that was retired (not one minted since), read back by both reads, and a
// second boot of the migration is a no-op. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationRemoteKitRetireAt(t *testing.T) {
	db, st := integrationDB(t)
	if err := applyRemoteKitRetire(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	st.SetRemoteKitEncryptionKey("cluster-secret-a")
	f := newFixture(t, st)
	id := uint(f.node.ID)
	t.Cleanup(func() { _ = st.DeleteRemoteKit(id) })

	linkID := "lk_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if err := st.SaveRemoteKit(store.RemoteKit{NodeID: id, LinkID: linkID, LinkKey: "key"}); err != nil {
		t.Fatal(err)
	}
	if k, err := st.GetRemoteKit(id); err != nil || k == nil || k.RetireAt != nil {
		t.Fatalf("fresh kit = %+v, %v; want retire_at NULL", k, err)
	}
	at := time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC)
	if err := st.SetRemoteKitRetireAt(id, linkID+"_other", at); err != nil {
		t.Fatal(err)
	}
	if k, _ := st.GetRemoteKit(id); k == nil || k.RetireAt != nil {
		t.Fatalf("stamp for another link id landed: %+v", k)
	}
	if err := st.SetRemoteKitRetireAt(id, linkID, at); err != nil {
		t.Fatal(err)
	}
	if k, _ := st.GetRemoteKit(id); k == nil || k.RetireAt == nil || !k.RetireAt.Equal(at) {
		t.Fatalf("GetRemoteKit after the stamp = %+v", k)
	}
	kits, err := st.ListRemoteKits()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range kits {
		if k.NodeID == id {
			found = k.RetireAt != nil && k.RetireAt.Equal(at)
		}
	}
	if !found {
		t.Fatalf("ListRemoteKits lacks the stamp: %+v", kits)
	}
}
