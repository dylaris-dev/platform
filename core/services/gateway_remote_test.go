package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
)

type remoteOutboxItem struct {
	op, domain string
	payload    any
	id         int64
	next       time.Time
	attempts   int
	lastErr    string
	done       bool
}

// remoteFakeStore embeds store.Store (nil); only the methods RemoteGateway
// touches are overridden.
type remoteFakeStore struct {
	store.Store
	mu      sync.Mutex
	servers map[int]*models.Server
	nodes   map[int]*models.Node
	kits    map[uint]store.RemoteKit
	routes  map[string]store.RemoteRoute
	// outbox items stay open until FinishOutbox; ids are positions + 1.
	outbox    []remoteOutboxItem
	upsertErr error
	// unlisted servers are left out of ListAllServers (a row that failed to
	// scan); uuidLookupErr fails GetServerByUUID.
	unlisted      map[string]bool
	uuidLookupErr error
	// afterListRoutes runs once ListRemoteRoutes has answered: a write by
	// someone else between the reconcile's row read and its provider read.
	afterListRoutes func()

	// holdGets > 0 makes that many GetRemoteKit calls that find no kit wait for
	// each other, so concurrent EnsureKit calls all miss before any inserts.
	holdGets int
	release  chan struct{}
	// stampErr fails SetRemoteKitRetireAt.
	stampErr error
}

func newRemoteFakeStore() *remoteFakeStore {
	return &remoteFakeStore{
		servers: map[int]*models.Server{},
		nodes:   map[int]*models.Node{},
		kits:    map[uint]store.RemoteKit{},
		routes:  map[string]store.RemoteRoute{},
		release: make(chan struct{}),
	}
}

func (f *remoteFakeStore) GetServerByID(id int) (*models.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[id]; ok {
		c := *s
		return &c, nil
	}
	return nil, errors.New("no such server")
}

func (f *remoteFakeStore) GetNodeByID(id int) (*models.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n, ok := f.nodes[id]; ok {
		c := *n
		return &c, nil
	}
	return nil, errors.New("no such node")
}

func (f *remoteFakeStore) GetRemoteKit(nodeID uint) (*store.RemoteKit, error) {
	f.mu.Lock()
	k, ok := f.kits[nodeID]
	wait := false
	if !ok && f.holdGets > 0 {
		f.holdGets--
		if f.holdGets == 0 {
			close(f.release)
		}
		wait = true
	}
	f.mu.Unlock()
	if wait {
		<-f.release
	}
	if !ok {
		return nil, nil
	}
	return &k, nil
}

func (f *remoteFakeStore) InsertRemoteKit(k store.RemoteKit) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.kits[k.NodeID]; ok {
		return false, nil
	}
	f.kits[k.NodeID] = k
	return true, nil
}

func (f *remoteFakeStore) DeleteRemoteKit(nodeID uint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.kits, nodeID)
	return nil
}

func (f *remoteFakeStore) ListRemoteKits() ([]store.RemoteKit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.RemoteKit
	for _, k := range f.kits {
		out = append(out, k)
	}
	return out, nil
}

func (f *remoteFakeStore) UpsertRemoteRoute(r store.RemoteRoute) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.routes[r.Domain] = r
	return nil
}

func (f *remoteFakeStore) ClaimRemoteRoute(r store.RemoteRoute) (bool, *store.RemoteRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		return false, nil, f.upsertErr
	}
	if held, ok := f.routes[r.Domain]; ok {
		return false, &held, nil
	}
	f.routes[r.Domain] = r
	return true, nil, nil
}

func (f *remoteFakeStore) ListRemoteRoutesByServer(uuid string) ([]store.RemoteRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.RemoteRoute
	for _, r := range f.routes {
		if r.ServerUUID == uuid {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *remoteFakeStore) HasOpenOutbox(domain, op string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.outbox {
		if !it.done && it.domain == domain && (op == "" || it.op == op) {
			return true, nil
		}
	}
	return false, nil
}

func (f *remoteFakeStore) EnqueueOutbox(op, domain string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outbox = append(f.outbox, remoteOutboxItem{op: op, domain: domain, payload: payload, id: int64(len(f.outbox) + 1)})
	return nil
}

// ClaimDueOutbox mirrors the Postgres claim: due order (next, id), a lease,
// then sorted by id.
func (f *remoteFakeStore) ClaimDueOutbox(now time.Time, limit int) ([]store.OutboxItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var due []*remoteOutboxItem
	headSeen := map[string]bool{}
	for i := range f.outbox {
		it := &f.outbox[i]
		if it.done {
			continue
		}
		// Not claimed behind an older open item of its domain.
		blocked := headSeen[it.domain]
		headSeen[it.domain] = true
		if !blocked && !it.next.After(now) {
			due = append(due, it)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].next.Before(due[j].next) })
	if len(due) > limit {
		due = due[:limit]
	}
	var out []store.OutboxItem
	for _, it := range due {
		it.next = now.Add(2 * time.Minute)
		b, err := json.Marshal(it.payload)
		if err != nil {
			return nil, err
		}
		out = append(out, store.OutboxItem{ID: it.id, Op: it.op, Domain: it.domain, Payload: b, Attempts: it.attempts, LastError: it.lastErr})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *remoteFakeStore) item(id int64) *remoteOutboxItem {
	return &f.outbox[id-1]
}

func (f *remoteFakeStore) FinishOutbox(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.item(id).done = true
	return nil
}

func (f *remoteFakeStore) FailOutbox(id int64, lastError string, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.item(id)
	it.attempts++
	it.lastErr, it.next = lastError, next
	return nil
}

func (f *remoteFakeStore) DeferOutbox(id int64, next time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.item(id).next = next
	return nil
}

func (f *remoteFakeStore) OldestOpenOutboxID(domain string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.outbox {
		if !it.done && it.domain == domain {
			return it.id, nil
		}
	}
	return 0, nil
}

func (f *remoteFakeStore) PurgeDoneOutbox(time.Time) (int64, error) { return 0, nil }

func (f *remoteFakeStore) GetRemoteRoute(domain string) (*store.RemoteRoute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.routes[domain]; ok {
		return &r, nil
	}
	return nil, nil
}

func (f *remoteFakeStore) ListRemoteRoutes() ([]store.RemoteRoute, error) {
	f.mu.Lock()
	var out []store.RemoteRoute
	for _, r := range f.routes {
		out = append(out, r)
	}
	after := f.afterListRoutes
	f.mu.Unlock()
	if after != nil {
		after()
	}
	return out, nil
}

func (f *remoteFakeStore) ListAllServers() ([]models.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []models.Server
	for _, s := range f.servers {
		if !f.unlisted[s.UUID] {
			out = append(out, *s)
		}
	}
	return out, nil
}

func (f *remoteFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uuidLookupErr != nil {
		return nil, f.uuidLookupErr
	}
	for _, s := range f.servers {
		if s.UUID == uuid {
			c := *s
			return &c, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (f *remoteFakeStore) DeleteRemoteRoute(domain string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.routes, domain)
	return nil
}

const (
	uuidA = "0b6f1c2e-6a3d-4c1b-9e7f-1a2b3c4d5e6f"
	uuidB = "1c7a2d3f-7b4e-4d2c-8f80-2b3c4d5e6f70"
)

func newRemoteTest(t *testing.T) (*RemoteGateway, *remoteFakeStore, *remotegw.Fake) {
	t.Helper()
	fake := remotegw.NewFake(t)
	st := newRemoteFakeStore()
	st.nodes[7] = &models.Node{ID: 7, Name: "node-seven"}
	st.nodes[9] = &models.Node{ID: 9, Name: "node-nine"}
	st.servers[1] = &models.Server{ID: 1, UUID: uuidA, NodeID: 7}
	st.servers[2] = &models.Server{ID: 2, UUID: uuidB, NodeID: 7, ProxyProtocol: true}
	g := NewRemoteGateway(st, func() *remotegw.Client { return remotegw.New(fake.URL(), fake.Key) })
	return g, st, fake
}

func countRemoteCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestRemoteCreateServerRouteMintsOneKitPerNode(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	if err := g.CreateServerRoute(1, "owner", "a.example.com", 25565); err != nil {
		t.Fatal(err)
	}
	if err := g.CreateServerRoute(2, "owner", "b.example.com", 25566); err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if n := countRemoteCalls(calls, "POST /link-kits"); n != 1 {
		t.Fatalf("mints = %d, want 1 (%v)", n, calls)
	}
	if n := countRemoteCalls(calls, "POST /routes"); n != 2 {
		t.Fatalf("route puts = %d, want 2 (%v)", n, calls)
	}
	routes, err := remotegw.New(fake.URL(), fake.Key).Routes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a.example.com": "mc-" + uuidA, "b.example.com": "mc-" + uuidB}
	for _, r := range routes {
		if want[r.Domain] != r.TargetHost || r.LinkID != st.kits[7].LinkID {
			t.Fatalf("route %+v, want host %q on %q", r, want[r.Domain], st.kits[7].LinkID)
		}
	}
	if len(routes) != 2 || len(st.routes) != 2 || st.routes["b.example.com"].Port != 25566 || st.routes["b.example.com"].NodeID != 7 {
		t.Fatalf("provider %v, local %v", routes, st.routes)
	}
	// Server 2 has the real-IP switch on: it follows through the outbox.
	if len(st.outbox) != 1 || st.outbox[0].op != OutboxOpProxy || st.outbox[0].domain != "b.example.com" || st.outbox[0].payload != (OutboxProxy{Enabled: true}) {
		t.Fatalf("outbox = %+v", st.outbox)
	}
}

func TestRemoteCreatePassesProviderRefusalThrough(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	if _, err := g.EnsureKit(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	fake.FailNext(1, 409, "domain_taken", 0)
	err := g.CreateServerRoute(1, "owner", "taken.example.com", 25565)
	if !remotegw.IsCode(err, "domain_taken") {
		t.Fatalf("err = %v, want domain_taken", err)
	}
	if len(st.routes) != 0 {
		t.Fatalf("rows = %v, want none", st.routes)
	}
}

func TestRemoteCreateRefusesNonCanonicalServerID(t *testing.T) {
	for _, tc := range []struct{ name, uuid string }{
		{"too long for a DNS label", strings.Repeat("a", 70)},
		{"trailing underscore becomes a trailing dash", "abcdefgh_"},
		{"invalid characters", "abc/../defgh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, st, fake := newRemoteTest(t)
			st.servers[3] = &models.Server{ID: 3, UUID: tc.uuid, NodeID: 7}
			err := g.CreateServerRoute(3, "owner", "x.example.com", 25565)
			if !errors.Is(err, ErrGatewayTargetName) || err.Error() != "server id cannot be used as a gateway target" {
				t.Fatalf("err = %v", err)
			}
			if c := fake.Calls(); len(c) != 0 || len(st.routes) != 0 {
				t.Fatalf("calls %v, rows %v", c, st.routes)
			}
		})
	}
}

func seedRemoteRoutes(st *remoteFakeStore) {
	st.routes["a1.example.com"] = store.RemoteRoute{Domain: "a1.example.com", ServerUUID: uuidA, NodeID: 7, Port: 25565}
	st.routes["a2.example.com"] = store.RemoteRoute{Domain: "a2.example.com", ServerUUID: uuidA, NodeID: 7, Port: 25570}
	st.routes["b1.example.com"] = store.RemoteRoute{Domain: "b1.example.com", ServerUUID: uuidB, NodeID: 7, Port: 25565}
}

func outboxByDomain(st *remoteFakeStore) map[string]remoteOutboxItem {
	out := map[string]remoteOutboxItem{}
	for _, it := range st.outbox {
		out[it.domain] = it
	}
	return out
}

func TestRemoteMigrateEnqueuesOneMovePerDomain(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	seedRemoteRoutes(st)
	if err := g.MigrateServerRoutes(1, 9); err != nil {
		t.Fatal(err)
	}
	kit, ok := st.kits[9]
	if !ok || countRemoteCalls(fake.Calls(), "POST /link-kits") != 1 {
		t.Fatalf("kit for node 9 = %+v, calls %v", kit, fake.Calls())
	}
	got := outboxByDomain(st)
	if len(st.outbox) != 2 {
		t.Fatalf("outbox = %+v", st.outbox)
	}
	for _, tc := range []struct {
		domain string
		port   int
	}{{"a1.example.com", 25565}, {"a2.example.com", 25570}} {
		want := OutboxMove{LinkID: kit.LinkID, NodeID: 9, TargetHost: "mc-" + uuidA, Port: tc.port}
		if it := got[tc.domain]; it.op != OutboxOpMove || it.payload != want {
			t.Fatalf("%s: %+v, want %+v", tc.domain, it, want)
		}
	}
	if st.routes["a1.example.com"].NodeID != 7 || countRemoteCalls(fake.Calls(), "POST /routes") != 0 {
		t.Fatalf("migrate wrote directly: rows %v, calls %v", st.routes, fake.Calls())
	}
}

func TestRemoteDeleteServerRoutesEnqueues(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	seedRemoteRoutes(st)
	if err := g.DeleteServerRoutes(uuidA); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteServerRoutes(""); err != nil {
		t.Fatal(err)
	}
	got := outboxByDomain(st)
	if len(st.outbox) != 2 || got["a1.example.com"].op != OutboxOpDelete || got["a2.example.com"].op != OutboxOpDelete {
		t.Fatalf("outbox = %+v", st.outbox)
	}
	if len(st.routes) != 3 || len(fake.Calls()) != 0 {
		t.Fatalf("rows %v, calls %v", st.routes, fake.Calls())
	}
}

func TestRemoteProxyProtocolEnqueues(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	seedRemoteRoutes(st)
	if err := g.SetServerProxyProtocol(uuidA, true); err != nil {
		t.Fatal(err)
	}
	got := outboxByDomain(st)
	for _, d := range []string{"a1.example.com", "a2.example.com"} {
		if it := got[d]; it.op != OutboxOpProxy || it.payload != (OutboxProxy{Enabled: true}) {
			t.Fatalf("%s: %+v", d, it)
		}
	}
	// The provider has neither route, so the first one's synchronous ask
	// answered not_found and fell back to the queue.
	if len(st.outbox) != 2 || st.routes["a1.example.com"].ProxyProtocol || countRemoteCalls(fake.Calls(), "PATCH ") != 1 || len(fake.Calls()) != 1 {
		t.Fatalf("outbox %+v, rows %v, calls %v", st.outbox, st.routes, fake.Calls())
	}
}

func TestRemoteRouteOnlyMethodsRefuse(t *testing.T) {
	g, _, fake := newRemoteTest(t)
	for name, call := range map[string]func() error{
		"CreateRouteViaLink":             func() error { return g.CreateRouteViaLink("o", "d.example.com", "tok", "h", 1) },
		"DeleteCoreOwnedRoute":           func() error { return g.DeleteCoreOwnedRoute("d.example.com") },
		"SetCoreOwnedRouteProxyProtocol": func() error { return g.SetCoreOwnedRouteProxyProtocol("d.example.com", true) },
		"SuspendCoreOwnedRoute":          func() error { _, err := g.SuspendCoreOwnedRoute("d.example.com", "r"); return err },
		"UnsuspendCoreOwnedRoute":        func() error { _, err := g.UnsuspendCoreOwnedRoute("d.example.com"); return err },
		"SetNodeLinkEnabled":             func() error { return g.SetNodeLinkEnabled("tok", false) },
	} {
		if err := call(); !errors.Is(err, ErrGatewayRemote) {
			t.Errorf("%s: err = %v, want ErrGatewayRemote", name, err)
		}
	}
	if g.LinkToken("7") != "" || g.DiscoveryProof("7") != "" {
		t.Fatal("LinkToken/DiscoveryProof must be empty in remote mode")
	}
	if c := fake.Calls(); len(c) != 0 {
		t.Fatalf("provider saw %v", c)
	}
}

func TestRemoteEnsureKitIsRaceSafe(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	st.holdGets = 2
	var wg sync.WaitGroup
	got := make([]store.RemoteKit, 2)
	errs := make([]error, 2)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = g.EnsureKit(context.Background(), 7)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	kept := st.kits[7]
	if got[0] != kept || got[1] != kept {
		t.Fatalf("callers got %+v and %+v, stored %+v", got[0], got[1], kept)
	}
	calls := fake.Calls()
	if countRemoteCalls(calls, "POST /link-kits") != 2 || countRemoteCalls(calls, "DELETE /link-kits/") != 1 || countRemoteCalls(calls, "DELETE /link-kits/"+kept.LinkID) != 0 {
		t.Fatalf("calls = %v, kept %s", calls, kept.LinkID)
	}
	kits, err := remotegw.New(fake.URL(), fake.Key).Kits(context.Background())
	if err != nil || len(kits) != 1 || kits[0].LinkID != kept.LinkID {
		t.Fatalf("provider kits = %+v, %v", kits, err)
	}
}

func TestRemoteKitStatusCachesThirtySeconds(t *testing.T) {
	g, _, fake := newRemoteTest(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	kit, err := g.EnsureKit(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	fake.SetKitOnline(kit.LinkID, "voice")
	gets := func() int { return countRemoteCalls(fake.Calls(), "GET /link-kits") }

	st, err := g.KitStatus(context.Background())
	if err != nil || st[7].LinkID != kit.LinkID || st[7].Online == nil || !*st[7].Online || len(st) != 1 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if _, err := g.KitStatus(context.Background()); err != nil || gets() != 1 {
		t.Fatalf("second call within ttl: err %v, gets %d", err, gets())
	}
	now = now.Add(31 * time.Second)
	fake.FailNext(1, 503, "unavailable", 0)
	if _, err := g.KitStatus(context.Background()); err == nil {
		t.Fatal("want the provider error")
	}
	if _, err := g.KitStatus(context.Background()); err == nil || gets() != 2 {
		t.Fatalf("failure inside 10 s must come from the cache: err %v, gets %d", err, gets())
	}
	now = now.Add(11 * time.Second)
	if _, err := g.KitStatus(context.Background()); err != nil || gets() != 3 {
		t.Fatalf("after the failure expired: err %v, gets %d", err, gets())
	}
}

func TestRemoteCreateRefusesWhilePreviousChangePending(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	if err := g.DeleteRoute("x.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := g.CreateServerRoute(1, "owner", "x.example.com", 25565); !errors.Is(err, ErrGatewayChangePending) {
		t.Fatalf("err = %v, want ErrGatewayChangePending", err)
	}
	if c := fake.Calls(); len(c) != 0 || len(st.routes) != 0 {
		t.Fatalf("calls %v, rows %v", c, st.routes)
	}
	// A queued move counts too: any open item for the domain blocks the create.
	st.outbox = []remoteOutboxItem{{op: OutboxOpMove, domain: "x.example.com"}}
	if err := g.CreateServerRoute(1, "owner", "x.example.com", 25565); !errors.Is(err, ErrGatewayChangePending) {
		t.Fatalf("with a move queued: err = %v", err)
	}
	// Another domain is not held up.
	if err := g.CreateServerRoute(1, "owner", "y.example.com", 25565); err != nil {
		t.Fatal(err)
	}
}

// Another server's address answers taken even while its owner has a change
// queued for it: "pending" would tell a stranger about an address it does not
// own and send it back to try again.
func TestRemoteCreateOnAnotherServersPendingAddressIsTaken(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	held := store.RemoteRoute{Domain: "x.example.com", ServerUUID: uuidB, NodeID: 7, Port: 25565}
	st.routes[held.Domain] = held
	st.outbox = []remoteOutboxItem{{op: OutboxOpProxy, domain: held.Domain, id: 1}}
	if err := g.CreateServerRoute(1, "owner", held.Domain, 25565); !errors.Is(err, ErrGatewayDomainTaken) {
		t.Fatalf("err = %v, want ErrGatewayDomainTaken", err)
	}
	// The holder itself still waits for its change.
	if err := g.CreateServerRoute(2, "owner", held.Domain, 25565); !errors.Is(err, ErrGatewayChangePending) {
		t.Fatalf("holder: err = %v, want ErrGatewayChangePending", err)
	}
	if c := fake.Calls(); len(c) != 0 || st.routes[held.Domain] != held {
		t.Fatalf("calls %v, row %+v", c, st.routes[held.Domain])
	}
}

// Turning real IP on asks the provider for the first route at once. A refusal
// reaches the caller and leaves nothing behind; a failure that may pass is
// queued as before; off is never sent synchronously.
func TestRemoteProxyProtocolOnAsksTheProviderFirst(t *testing.T) {
	setup := func(t *testing.T) (*RemoteGateway, *remoteFakeStore, *remotegw.Fake) {
		g, st, fake := newRemoteTest(t)
		if err := g.CreateServerRoute(1, "owner", "a.example.com", 25565); err != nil {
			t.Fatal(err)
		}
		return g, st, fake
	}
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"provider feature off", 409, "proxy_protocol_disabled"},
		{"link without client-addr", 409, "link_without_client_addr"},
		{"route suspended", 403, "route_suspended"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, st, fake := setup(t)
			fake.FailNext(1, tc.status, tc.code, 0)
			err := g.SetServerProxyProtocol(uuidA, true)
			if !remotegw.IsCode(err, tc.code) {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
			if len(st.outbox) != 0 || st.routes["a.example.com"].ProxyProtocol {
				t.Fatalf("refusal left outbox %+v, row %+v", st.outbox, st.routes["a.example.com"])
			}
		})
	}
	t.Run("provider error queues", func(t *testing.T) {
		g, st, fake := setup(t)
		fake.FailNext(1, 503, "unavailable", 0)
		if err := g.SetServerProxyProtocol(uuidA, true); err != nil {
			t.Fatal(err)
		}
		if len(st.outbox) != 1 || st.outbox[0].op != OutboxOpProxy || st.routes["a.example.com"].ProxyProtocol {
			t.Fatalf("outbox %+v, row %+v", st.outbox, st.routes["a.example.com"])
		}
	})
	t.Run("network failure queues", func(t *testing.T) {
		_, st, fake := setup(t)
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		g := NewRemoteGateway(st, func() *remotegw.Client { return remotegw.New(dead.URL, fake.Key) })
		if err := g.SetServerProxyProtocol(uuidA, true); err != nil {
			t.Fatal(err)
		}
		if len(st.outbox) != 1 || st.outbox[0].op != OutboxOpProxy {
			t.Fatalf("outbox %+v", st.outbox)
		}
	})
	t.Run("a queued change is not overtaken", func(t *testing.T) {
		g, st, fake := setup(t)
		st.outbox = []remoteOutboxItem{{op: OutboxOpMove, domain: "a.example.com", id: 1}}
		before := len(fake.Calls())
		if err := g.SetServerProxyProtocol(uuidA, true); err != nil {
			t.Fatal(err)
		}
		if len(fake.Calls()) != before || len(st.outbox) != 2 {
			t.Fatalf("calls %v, outbox %+v", fake.Calls()[before:], st.outbox)
		}
	})
	t.Run("off is only queued", func(t *testing.T) {
		g, st, fake := setup(t)
		fake.FailNext(1, 409, "proxy_protocol_disabled", 0)
		before := len(fake.Calls())
		if err := g.SetServerProxyProtocol(uuidA, false); err != nil {
			t.Fatal(err)
		}
		if len(fake.Calls()) != before || len(st.outbox) != 1 {
			t.Fatalf("calls %v, outbox %+v", fake.Calls()[before:], st.outbox)
		}
	})
}

func TestRemoteMigrateSkipsDomainsBeingDeleted(t *testing.T) {
	g, st, _ := newRemoteTest(t)
	seedRemoteRoutes(st)
	if err := g.DeleteRoute("a1.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := g.MigrateServerRoutes(1, 9); err != nil {
		t.Fatal(err)
	}
	if err := g.SetServerProxyProtocol(uuidA, true); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range st.outbox {
		got = append(got, it.op+" "+it.domain)
	}
	want := []string{"delete a1.example.com", "move a2.example.com", "proxy a2.example.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("outbox = %v, want %v", got, want)
	}
}

func TestRemoteServerDomainsHidesPendingDelete(t *testing.T) {
	g, st, _ := newRemoteTest(t)
	seedRemoteRoutes(st)
	if err := g.DeleteRoute("a1.example.com"); err != nil {
		t.Fatal(err)
	}
	got, err := g.ServerDomains(uuidA)
	if err != nil || len(got) != 1 || got[0] != "a2.example.com" {
		t.Fatalf("ServerDomains = %v, %v", got, err)
	}
	if _, ok := st.routes["a1.example.com"]; !ok {
		t.Fatal("the row must stay until the delete finishes")
	}
}

func TestRemoteDeleteIsNotQueuedTwice(t *testing.T) {
	g, st, _ := newRemoteTest(t)
	seedRemoteRoutes(st)
	for i := 0; i < 2; i++ {
		if err := g.DeleteRoute("a1.example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.DeleteServerRoutes(uuidA); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, it := range st.outbox {
		got[it.op+" "+it.domain]++
	}
	if len(st.outbox) != 2 || got["delete a1.example.com"] != 1 || got["delete a2.example.com"] != 1 {
		t.Fatalf("outbox = %+v", st.outbox)
	}
}

// The claim is written before the provider is asked, so a failed save means
// the provider never sees the route.
func TestRemoteCreateSaveFailureAsksNoProvider(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	st.upsertErr = errors.New("db down")
	if err := g.CreateServerRoute(1, "owner", "a.example.com", 25565); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("err = %v, want the save error", err)
	}
	if c := fake.Calls(); len(c) != 0 {
		t.Fatalf("calls = %v, want none", c)
	}
}

func TestRemoteKitStatusShowsFreshKitAtOnce(t *testing.T) {
	g, _, _ := newRemoteTest(t)
	if _, err := g.EnsureKit(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if st, err := g.KitStatus(context.Background()); err != nil || len(st) != 1 {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if _, err := g.EnsureKit(context.Background(), 9); err != nil {
		t.Fatal(err)
	}
	if st, err := g.KitStatus(context.Background()); err != nil || len(st) != 2 {
		t.Fatalf("a kit minted inside the TTL must show at once: %+v, %v", st, err)
	}
}

// Every server here shares one provider account, and the provider reads a
// second put of a domain by the same account as an edit: the local claim must
// stop a second server before the provider is asked.
func TestRemoteCreateRefusesAnotherServersAddress(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	if err := g.CreateServerRoute(1, "tenant-a", "play.example.com", 25565); err != nil {
		t.Fatal(err)
	}
	puts := countRemoteCalls(fake.Calls(), "POST /routes")
	before := st.routes["play.example.com"]
	if err := g.CreateServerRoute(2, "tenant-b", "play.example.com", 25570); !errors.Is(err, ErrGatewayDomainTaken) {
		t.Fatalf("err = %v, want ErrGatewayDomainTaken", err)
	}
	if n := countRemoteCalls(fake.Calls(), "POST /routes"); n != puts {
		t.Fatalf("provider puts = %d, want %d (no put for the refused create)", n, puts)
	}
	if got := st.routes["play.example.com"]; got != before || got.ServerUUID != uuidA {
		t.Fatalf("row = %+v, want it unchanged %+v", got, before)
	}
	routes, err := remotegw.New(fake.URL(), fake.Key).Routes(context.Background())
	if err != nil || len(routes) != 1 || routes[0].TargetHost != "mc-"+uuidA {
		t.Fatalf("provider routes = %+v, %v", routes, err)
	}
}

func TestRemoteCreateConcurrentClaimsHaveOneWinner(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	if _, err := g.EnsureKit(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = g.CreateServerRoute(uint(i+1), "owner", "race.example.com", 25565)
		}(i)
	}
	wg.Wait()
	won, lost := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrGatewayDomainTaken):
			lost++
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("won %d lost %d, want 1 and 1", won, lost)
	}
	winner := st.routes["race.example.com"].ServerUUID
	routes, err := remotegw.New(fake.URL(), fake.Key).Routes(context.Background())
	if err != nil || len(routes) != 1 || routes[0].TargetHost != "mc-"+winner || countRemoteCalls(fake.Calls(), "POST /routes") != 1 {
		t.Fatalf("provider routes = %+v, %v (winner %s, calls %v)", routes, err, winner, fake.Calls())
	}
}

func TestRemoteCreateProviderFailureReleasesOnlyItsOwnClaim(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	if _, err := g.EnsureKit(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	fake.FailNext(1, 503, "unavailable", 0)
	if err := g.CreateServerRoute(1, "owner", "fresh.example.com", 25565); err == nil {
		t.Fatal("want the provider error")
	}
	if _, ok := st.routes["fresh.example.com"]; ok {
		t.Fatal("a fresh claim must be released when the provider refuses")
	}
	// A row that was already the server's stays when its re-create fails.
	if err := g.CreateServerRoute(1, "owner", "kept.example.com", 25565); err != nil {
		t.Fatal(err)
	}
	fake.FailNext(1, 503, "unavailable", 0)
	if err := g.CreateServerRoute(1, "owner", "kept.example.com", 25565); err == nil {
		t.Fatal("want the provider error")
	}
	if _, ok := st.routes["kept.example.com"]; !ok {
		t.Fatal("a pre-existing row was released")
	}
}

func TestRemoteCreateAgainBySameServerIsIdempotent(t *testing.T) {
	g, st, fake := newRemoteTest(t)
	for i := 0; i < 2; i++ {
		if err := g.CreateServerRoute(1, "owner", "again.example.com", 25565+i); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if n := countRemoteCalls(fake.Calls(), "POST /routes"); n != 2 || len(st.routes) != 1 || st.routes["again.example.com"].Port != 25566 {
		t.Fatalf("puts %d, rows %+v", n, st.routes)
	}
}

// A provider that fails is asked once per 10 s, however many callers poll.
func TestRemoteKitStatusFailureIsSharedAndCached(t *testing.T) {
	g, _, fake := newRemoteTest(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	var nowMu sync.Mutex
	g.now = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
	gets := func() int { return countRemoteCalls(fake.Calls(), "GET /link-kits") }
	fake.FailNext(1, 503, "unavailable", 0)
	var wg sync.WaitGroup
	errs := make([]error, 20)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = g.KitStatus(context.Background())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("caller %d got no error", i)
		}
	}
	if gets() != 1 {
		t.Fatalf("provider calls = %d, want 1", gets())
	}
	nowMu.Lock()
	now = now.Add(5 * time.Second)
	nowMu.Unlock()
	if _, err := g.KitStatus(context.Background()); err == nil || gets() != 1 {
		t.Fatalf("inside 10 s: err %v, calls %d", err, gets())
	}
	nowMu.Lock()
	now = now.Add(6 * time.Second)
	nowMu.Unlock()
	if _, err := g.KitStatus(context.Background()); err != nil || gets() != 2 {
		t.Fatalf("after 10 s: err %v, calls %d", err, gets())
	}
}

// A provider that does not answer: a waiting caller leaves with its own
// context instead of queueing behind the call.
func TestRemoteCacheCallerLeavesWithItsOwnContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	g := NewRemoteGateway(newRemoteFakeStore(), func() *remotegw.Client { return remotegw.New(srv.URL, func() string { return "k" }) })
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		start := time.Now()
		_, err := g.Regions(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
			t.Fatalf("call %d: err %v after %v, want its own deadline", i, err, time.Since(start))
		}
	}
}

func TestRemoteCheckDomainIsCachedTenSeconds(t *testing.T) {
	g, _, fake := newRemoteTest(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	checks := func() int { return countRemoteCalls(fake.Calls(), "GET /check-domain") }
	for i := 0; i < 3; i++ {
		if ok, _, err := g.CheckDomain(context.Background(), "c.example.com"); err != nil || !ok {
			t.Fatalf("check %d: %v %v", i, ok, err)
		}
	}
	if checks() != 1 {
		t.Fatalf("provider checks = %d, want 1", checks())
	}
	if _, _, err := g.CheckDomain(context.Background(), "other.example.com"); err != nil || checks() != 2 {
		t.Fatalf("another domain: %v, checks %d", err, checks())
	}
	now = now.Add(11 * time.Second)
	if _, _, err := g.CheckDomain(context.Background(), "c.example.com"); err != nil || checks() != 3 {
		t.Fatalf("after 10 s: %v, checks %d", err, checks())
	}
}

// A deleted node's route row stays while a move is queued for it: the move
// re-homes the route and rewrites the row.
func TestRemoteForgetNodeKeepsRowsWithAQueuedMove(t *testing.T) {
	g, st, _ := newRemoteTest(t)
	seedRemoteRoutes(st)
	if err := st.EnqueueOutbox(OutboxOpMove, "a2.example.com", OutboxMove{NodeID: 9}); err != nil {
		t.Fatal(err)
	}
	g.ForgetNode(context.Background(), 7)
	if _, ok := st.routes["a2.example.com"]; !ok || len(st.routes) != 1 {
		t.Fatalf("rows = %v, want only the moving a2.example.com", st.routes)
	}
}
