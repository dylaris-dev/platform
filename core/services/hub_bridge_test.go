package services

import (
	"context"
	"encoding/json"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/errlog"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// hubBridgeFakeStore embeds store.Store (nil) so it satisfies the full interface
// at compile time; only the methods RedisGateway touches are overridden. Any
// other method call would panic — no test here makes one. Mirrors the
// warpFakeStore pattern in handlers/warp_test.go.
type hubBridgeFakeStore struct {
	store.Store
	servers  map[int]models.Server
	nodes    map[int]models.Node
	settings map[string]string
	// The durable side of a route-only entry, keyed by domain, behaving like
	// the table: the create path writes it and reads it back to decide who owns
	// a domain when Redis cannot say.
	routes     map[string]store.CoreLinkRoute
	routeErr   error
	upsertErr  error
	upsertCall int
}

func (f *hubBridgeFakeStore) ListCoreLinkRoutes() ([]store.CoreLinkRoute, error) {
	if f.routeErr != nil {
		return nil, f.routeErr
	}
	out := make([]store.CoreLinkRoute, 0, len(f.routes))
	for _, r := range f.routes {
		out = append(out, r)
	}
	return out, nil
}

func (f *hubBridgeFakeStore) UpsertCoreLinkRoute(r store.CoreLinkRoute) error {
	f.upsertCall++
	if f.upsertErr != nil {
		return f.upsertErr
	}
	if f.routes == nil {
		f.routes = map[string]store.CoreLinkRoute{}
	}
	f.routes[r.Domain] = r
	return nil
}

func (f *hubBridgeFakeStore) DeleteCoreLinkRoute(domain string) error {
	delete(f.routes, domain)
	return nil
}

func (f *hubBridgeFakeStore) GetServerByID(id int) (*models.Server, error) {
	if s, ok := f.servers[id]; ok {
		return &s, nil
	}
	return nil, hubBridgeErr("server not found")
}

func (f *hubBridgeFakeStore) GetNodeByID(id int) (*models.Node, error) {
	if n, ok := f.nodes[id]; ok {
		return &n, nil
	}
	return nil, hubBridgeErr("node not found")
}

func (f *hubBridgeFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

type hubBridgeErr string

func (e hubBridgeErr) Error() string { return string(e) }

func newHubBridgeTestGateway(t *testing.T) (*RedisGateway, *redis.Client, *hubBridgeFakeStore) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	fs := &hubBridgeFakeStore{
		servers:  map[int]models.Server{},
		nodes:    map[int]models.Node{},
		settings: map[string]string{},
		routes:   map[string]store.CoreLinkRoute{},
	}
	g := NewRedisGateway(rdb, fs, "test-cluster-secret")
	return g, rdb, fs
}

// readHubQueueMessages pops every message currently on the hub queue list and
// decodes each as a hubQueueMessage for assertion.
func readHubQueueMessages(t *testing.T, rdb *redis.Client) []hubQueueMessage {
	t.Helper()
	ctx := context.Background()
	raw, err := rdb.LRange(ctx, hubQueueKey, 0, -1).Result()
	if err != nil {
		t.Fatalf("LRange %s: %v", hubQueueKey, err)
	}
	out := make([]hubQueueMessage, 0, len(raw))
	for _, r := range raw {
		var m hubQueueMessage
		if err := json.Unmarshal([]byte(r), &m); err != nil {
			t.Fatalf("unmarshal hub queue message %q: %v", r, err)
		}
		out = append(out, m)
	}
	return out
}

func TestCreateServerRoute_PushesQueueMessage(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.nodes[1] = models.Node{ID: 1, Token: "node-tok-1"}
	fs.servers[10] = models.Server{ID: 10, UUID: "srv-uuid-10", NodeID: 1}

	if err := g.CreateServerRoute(10, "owner-1", "sub.example.com", 25566); err != nil {
		t.Fatalf("CreateServerRoute: %v", err)
	}

	msgs := readHubQueueMessages(t, rdb)
	if len(msgs) != 1 {
		t.Fatalf("got %d queue messages, want 1", len(msgs))
	}
	m := msgs[0]
	wantLinkToken := DeriveLinkToken("node-tok-1", "test-cluster-secret")
	if m.Action != "create_route" {
		t.Errorf("Action = %q, want create_route", m.Action)
	}
	if m.Domain != "sub.example.com" {
		t.Errorf("Domain = %q, want sub.example.com", m.Domain)
	}
	if m.TargetIP != "mc_srv-uuid-10" {
		t.Errorf("TargetIP = %q, want mc_srv-uuid-10", m.TargetIP)
	}
	if m.TargetPort != 25566 {
		t.Errorf("TargetPort = %d, want 25566", m.TargetPort)
	}
	if m.LinkToken != wantLinkToken {
		t.Errorf("LinkToken = %q, want %q", m.LinkToken, wantLinkToken)
	}
	if m.ServerUUID != "srv-uuid-10" {
		t.Errorf("ServerUUID = %q, want srv-uuid-10", m.ServerUUID)
	}
	if m.ServerID == nil || *m.ServerID != 10 {
		t.Errorf("ServerID = %v, want 10", m.ServerID)
	}
	if m.OwnerID == nil || *m.OwnerID != "owner-1" {
		t.Errorf("OwnerID = %v, want owner-1", m.OwnerID)
	}
}

func TestCreateServerRoute_MCPortDisabled_NoQueuePush(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.nodes[1] = models.Node{ID: 1, Token: "node-tok-1"}
	fs.servers[10] = models.Server{ID: 10, UUID: "srv-uuid-10", NodeID: 1}
	fs.settings["gateway_port_mc_enabled"] = "false"

	err := g.CreateServerRoute(10, "owner-1", "sub.example.com", 25565)
	if err == nil {
		t.Fatal("expected error when minecraft port routing is disabled")
	}

	n, lerr := rdb.LLen(context.Background(), hubQueueKey).Result()
	if lerr != nil {
		t.Fatalf("LLen: %v", lerr)
	}
	if n != 0 {
		t.Fatalf("queue length = %d, want 0 (nothing should be pushed on rejection)", n)
	}
}

func TestCreateServerRoute_NonMCPort_IgnoresDisabledSetting(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)
	fs.nodes[1] = models.Node{ID: 1, Token: "node-tok-1"}
	fs.servers[10] = models.Server{ID: 10, UUID: "srv-uuid-10", NodeID: 1}
	fs.settings["gateway_port_mc_enabled"] = "false"

	// The disabled flag only gates port 25565; any other port must go through.
	if err := g.CreateServerRoute(10, "owner-1", "sub.example.com", 8080); err != nil {
		t.Fatalf("CreateServerRoute on non-MC port must not be blocked: %v", err)
	}
}

func TestCreateServerRoute_ServerNotFound(t *testing.T) {
	g, _, _ := newHubBridgeTestGateway(t)
	if err := g.CreateServerRoute(999, "owner-1", "sub.example.com", 25566); err == nil {
		t.Fatal("expected error for unknown server")
	}
}

func TestCreateServerRoute_NodeNotFound(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)
	fs.servers[10] = models.Server{ID: 10, UUID: "srv-uuid-10", NodeID: 999}
	if err := g.CreateServerRoute(10, "owner-1", "sub.example.com", 25566); err == nil {
		t.Fatal("expected error for unknown node")
	}
}

func TestCreateRouteViaLink_WritesRouteAndIndex(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	ctx := context.Background()

	if err := g.CreateRouteViaLink("owner-2", "link.example.com", "link-tok-abc", "192.168.1.50", 25565); err != nil {
		t.Fatalf("CreateRouteViaLink: %v", err)
	}

	val, err := rdb.Get(ctx, "route:link.example.com").Result()
	if err != nil {
		t.Fatalf("Get route key: %v", err)
	}
	var r GatewayRoute
	if err := json.Unmarshal([]byte(val), &r); err != nil {
		t.Fatalf("unmarshal route: %v", err)
	}
	if r.Domain != "link.example.com" || r.TargetIP != "192.168.1.50" || r.TargetPort != 25565 || r.TunnelID != "link-tok-abc" || !r.CoreOwned || r.OwnerID != "owner-2" {
		t.Errorf("route = %+v, unexpected fields", r)
	}

	members, err := rdb.SMembers(ctx, "sys:index:routes").Result()
	if err != nil {
		t.Fatalf("SMembers: %v", err)
	}
	if len(members) != 1 || members[0] != "link.example.com" {
		t.Errorf("sys:index:routes = %v, want [link.example.com]", members)
	}
}

func TestCreateRouteViaLink_EmptyLinkToken_Errors(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	err := g.CreateRouteViaLink("owner-2", "link.example.com", "", "192.168.1.50", 25565)
	if err == nil {
		t.Fatal("expected error for empty link token")
	}
	n, _ := rdb.Exists(context.Background(), "route:link.example.com").Result()
	if n != 0 {
		t.Error("route key must not be written when link token is empty")
	}
}

func TestCreateRouteViaLink_MCPortDisabled(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.settings["gateway_port_mc_enabled"] = "false"

	err := g.CreateRouteViaLink("owner-2", "link.example.com", "link-tok-abc", "192.168.1.50", 25565)
	if err == nil {
		t.Fatal("expected error when minecraft port routing is disabled")
	}
	n, _ := rdb.Exists(context.Background(), "route:link.example.com").Result()
	if n != 0 {
		t.Error("route key must not be written when MC routing is disabled")
	}
}

func TestDeleteCoreOwnedRoute_RemovesKeyAndIndex(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	ctx := context.Background()

	if err := g.CreateRouteViaLink("owner-2", "gone.example.com", "link-tok", "10.0.0.1", 25565); err != nil {
		t.Fatalf("seed CreateRouteViaLink: %v", err)
	}
	if err := g.DeleteCoreOwnedRoute("gone.example.com"); err != nil {
		t.Fatalf("DeleteCoreOwnedRoute: %v", err)
	}

	if n, _ := rdb.Exists(ctx, "route:gone.example.com").Result(); n != 0 {
		t.Error("route key should be deleted")
	}
	members, _ := rdb.SMembers(ctx, "sys:index:routes").Result()
	if len(members) != 0 {
		t.Errorf("sys:index:routes should be empty, got %v", members)
	}
}

func TestDeleteRoute_PushesQueueMessage(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)

	if err := g.DeleteRoute("old.example.com"); err != nil {
		t.Fatalf("DeleteRoute: %v", err)
	}

	msgs := readHubQueueMessages(t, rdb)
	if len(msgs) != 1 {
		t.Fatalf("got %d queue messages, want 1", len(msgs))
	}
	if msgs[0].Action != "delete_route" || msgs[0].Domain != "old.example.com" {
		t.Errorf("msg = %+v, want Action=delete_route Domain=old.example.com", msgs[0])
	}
}

func TestMigrateServerRoutes_PushesQueueMessage(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.servers[20] = models.Server{ID: 20, UUID: "srv-uuid-20", NodeID: 1}
	fs.nodes[2] = models.Node{ID: 2, Token: "node-tok-2"}

	if err := g.MigrateServerRoutes(20, 2); err != nil {
		t.Fatalf("MigrateServerRoutes: %v", err)
	}

	msgs := readHubQueueMessages(t, rdb)
	if len(msgs) != 1 {
		t.Fatalf("got %d queue messages, want 1", len(msgs))
	}
	m := msgs[0]
	wantToken := DeriveLinkToken("node-tok-2", "test-cluster-secret")
	if m.Action != "migrate_routes" {
		t.Errorf("Action = %q, want migrate_routes", m.Action)
	}
	if m.ServerUUID != "srv-uuid-20" {
		t.Errorf("ServerUUID = %q, want srv-uuid-20", m.ServerUUID)
	}
	if m.NewLinkToken != wantToken {
		t.Errorf("NewLinkToken = %q, want %q", m.NewLinkToken, wantToken)
	}
}

// A node the Hub says is served by a self-enrolled Link: a route bound to the
// derived token would be dropped by the Hub, because no link carries it.
func TestCreateServerRoute_SendsTheLearnedLinkToken(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.nodes[1] = models.Node{ID: 1, Token: "node-tok-1", LinkToken: "generated-1"}
	fs.servers[10] = models.Server{ID: 10, UUID: "srv-uuid-10", NodeID: 1}

	if err := g.CreateServerRoute(10, "owner-1", "sub.example.com", 25566); err != nil {
		t.Fatalf("CreateServerRoute: %v", err)
	}
	msgs := readHubQueueMessages(t, rdb)
	if len(msgs) != 1 || msgs[0].LinkToken != "generated-1" {
		t.Fatalf("queued %+v, want one create_route with LinkToken generated-1", msgs)
	}
}

// A server moving to a node served by a self-enrolled Link follows THAT link,
// the destination node's, not a token derived from the node.
func TestMigrateServerRoutes_SendsTheDestinationsLearnedLinkToken(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	fs.servers[20] = models.Server{ID: 20, UUID: "srv-uuid-20", NodeID: 1}
	fs.nodes[1] = models.Node{ID: 1, Token: "node-tok-1", LinkToken: "generated-1"}
	fs.nodes[2] = models.Node{ID: 2, Token: "node-tok-2", LinkToken: "generated-2"}

	if err := g.MigrateServerRoutes(20, 2); err != nil {
		t.Fatalf("MigrateServerRoutes: %v", err)
	}
	msgs := readHubQueueMessages(t, rdb)
	if len(msgs) != 1 || msgs[0].NewLinkToken != "generated-2" {
		t.Fatalf("queued %+v, want one migrate_routes with NewLinkToken generated-2", msgs)
	}
}

func TestMigrateServerRoutes_ServerNotFound(t *testing.T) {
	g, _, _ := newHubBridgeTestGateway(t)
	if err := g.MigrateServerRoutes(999, 2); err == nil {
		t.Fatal("expected error for unknown server")
	}
}

func TestMigrateServerRoutes_NewNodeNotFound(t *testing.T) {
	g, _, fs := newHubBridgeTestGateway(t)
	fs.servers[20] = models.Server{ID: 20, UUID: "srv-uuid-20", NodeID: 1}
	if err := g.MigrateServerRoutes(20, 999); err == nil {
		t.Fatal("expected error for unknown target node")
	}
}

// TestDeriveLinkToken_DeriveDiscoveryProof_GoldenVectors pins the exact wire
// format of DeriveLinkToken/DeriveDiscoveryProof: both MUST stay byte-identical
// to the gateway's Link/Hub binaries' own derivation (see the doc comments on
// these functions in hub_bridge.go). A drift here silently breaks Link's edge
// auth and BYON discovery heartbeats. Vectors: SHA256(nodeID+secret) and
// SHA256("discovery:"+nodeID+":"+secret) for nodeID="node-a",
// secret="test-cluster-secret".
func TestDeriveLinkToken_DeriveDiscoveryProof_GoldenVectors(t *testing.T) {
	const nodeID = "node-a"
	const secret = "test-cluster-secret"

	gotToken := DeriveLinkToken(nodeID, secret)
	wantToken := "cccccae015a89f1690d74cfc2af6a2de9236bd7d436f8d08ef316b578ea87195"
	if gotToken != wantToken {
		t.Errorf("DeriveLinkToken vector drift:\n got  %s\n want %s", gotToken, wantToken)
	}
	if DeriveLinkToken("node-b", secret) == gotToken {
		t.Fatal("different node ids must yield different link tokens")
	}
	if DeriveLinkToken(nodeID, "other-secret") == gotToken {
		t.Fatal("different cluster secrets must yield different link tokens")
	}

	gotProof := DeriveDiscoveryProof(nodeID, secret)
	wantProof := "53788162a2d1e0a130e8b026564ba2688d7edc45673e3465202bc331185d07b8"
	if gotProof != wantProof {
		t.Errorf("DeriveDiscoveryProof vector drift:\n got  %s\n want %s", gotProof, wantProof)
	}
	if gotProof == gotToken {
		t.Fatal("DiscoveryProof and LinkToken must differ (distinct domain-separated inputs)")
	}
}

func TestLinkToken_And_DiscoveryProof_MatchPackageFunctions(t *testing.T) {
	g, _, _ := newHubBridgeTestGateway(t)
	if got, want := g.LinkToken("node-x"), DeriveLinkToken("node-x", "test-cluster-secret"); got != want {
		t.Errorf("g.LinkToken = %q, want %q", got, want)
	}
	if got, want := g.DiscoveryProof("node-x"), DeriveDiscoveryProof("node-x", "test-cluster-secret"); got != want {
		t.Errorf("g.DiscoveryProof = %q, want %q", got, want)
	}
}

func TestGetEdgesFromRedis_MergesLatestStats(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	edge := GatewayEdgeInfo{EdgeID: "edge-1", Name: "eu-1", IP: "1.2.3.4"}
	edgeJSON, _ := json.Marshal(edge)
	if err := rdb.Set(ctx, "edge:registry:edge-1", edgeJSON, 0).Err(); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	stats := EdgeLiveStats{CPU: 42.5, ActiveTunnels: 3}
	statsJSON, _ := json.Marshal(stats)
	if _, err := rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: "dylaris:edge:edge-1:stats",
		Values: map[string]interface{}{"data": statsJSON},
	}).Result(); err != nil {
		t.Fatalf("seed stats: %v", err)
	}

	edges := GetEdgesFromRedis(ctx, rdb)
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1", len(edges))
	}
	got := edges[0]
	if got.EdgeID != "edge-1" {
		t.Errorf("EdgeID = %q, want edge-1", got.EdgeID)
	}
	if got.Status != "online" {
		t.Errorf("Status = %q, want online (default when empty)", got.Status)
	}
	if got.Stats == nil || got.Stats.CPU != 42.5 || got.Stats.ActiveTunnels != 3 {
		t.Errorf("Stats = %+v, want merged CPU=42.5 ActiveTunnels=3", got.Stats)
	}
}

func TestGetEdgesFromRedis_CarriesSpliceVersionFields(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	// Seed a raw edge:registry payload exactly as the edge heartbeat marshals it,
	// including the running/latest splice version keys. GetEdgesFromRedis must
	// surface them so the panel can flag a pending bump (running behind latest).
	raw := `{"edge_id":"edge-eu-1","name":"eu-1","region":"eu","splice_version":"1.0","splice_version_latest":"1.1"}`
	if err := rdb.Set(ctx, "edge:registry:edge-eu-1", raw, 0).Err(); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	edges := GetEdgesFromRedis(ctx, rdb)
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1", len(edges))
	}
	got := edges[0]
	if got.Region != "eu" {
		t.Errorf("Region = %q, want eu", got.Region)
	}
	if got.SpliceVersion != "1.0" {
		t.Errorf("SpliceVersion = %q, want 1.0 (running)", got.SpliceVersion)
	}
	if got.SpliceVersionLatest != "1.1" {
		t.Errorf("SpliceVersionLatest = %q, want 1.1 (latest available)", got.SpliceVersionLatest)
	}
}

func TestGetEdgesFromRedis_NoStatsStream_StatsNil(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	edge := GatewayEdgeInfo{EdgeID: "edge-2", Status: "degraded"}
	edgeJSON, _ := json.Marshal(edge)
	if err := rdb.Set(ctx, "edge:registry:edge-2", edgeJSON, 0).Err(); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	edges := GetEdgesFromRedis(ctx, rdb)
	if len(edges) != 1 {
		t.Fatalf("got %d edges, want 1", len(edges))
	}
	if edges[0].Status != "degraded" {
		t.Errorf("Status = %q, want degraded (explicit status preserved)", edges[0].Status)
	}
	if edges[0].Stats != nil {
		t.Errorf("Stats = %+v, want nil when no stats stream exists", edges[0].Stats)
	}
}

func TestGetLinksFromRedis_OnlineViaKeepAliveOrTunnels(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	// link-a: has a live keep-alive key -> online, no tunnels.
	if err := rdb.Set(ctx, "link:tok-a", "1", 0).Err(); err != nil {
		t.Fatalf("seed link-a: %v", err)
	}
	if err := rdb.Set(ctx, "online_link:tok-a", "1", 0).Err(); err != nil {
		t.Fatalf("seed online_link-a: %v", err)
	}

	// link-b: no keep-alive, but the old tunnel-stats hash is present -> still
	// OFFLINE. That hash used to make a link count as online; nothing in either
	// repository has ever written it, so the only way it could appear was a test
	// like this one seeding it. Kept as a regression guard: presence is the
	// keep-alive key and nothing else.
	if err := rdb.Set(ctx, "link:tok-b", "1", 0).Err(); err != nil {
		t.Fatalf("seed link-b: %v", err)
	}
	if err := rdb.HSet(ctx, "sys:stats:tunnels:tok-b", "worker-1", "2", "worker-2", "1").Err(); err != nil {
		t.Fatalf("seed tunnels-b: %v", err)
	}

	// link-c: no keep-alive -> offline.
	if err := rdb.Set(ctx, "link:tok-c", "1", 0).Err(); err != nil {
		t.Fatalf("seed link-c: %v", err)
	}

	links := GetLinksFromRedis(ctx, rdb)
	byToken := map[string]GatewayLinkStatus{}
	for _, l := range links {
		byToken[l.Token] = l
	}
	if len(links) != 3 {
		t.Fatalf("got %d links, want 3: %+v", len(links), links)
	}
	if !byToken["tok-a"].Online {
		t.Errorf("tok-a = %+v, want Online=true (it has a keep-alive key)", byToken["tok-a"])
	}
	if byToken["tok-b"].Online {
		t.Errorf("tok-b = %+v, want Online=false - the tunnel-stats hash must not stand in for a keep-alive", byToken["tok-b"])
	}
	if byToken["tok-c"].Online {
		t.Errorf("tok-c = %+v, want Online=false", byToken["tok-c"])
	}
}

func TestGetRoutesFromRedis_ReadsIndexAndRoutes(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	route := GatewayRoute{TargetIP: "10.0.0.9", TargetPort: 25565, TunnelID: "tun-1"}
	routeJSON, _ := json.Marshal(route)
	if err := rdb.Set(ctx, "route:a.example.com", routeJSON, 0).Err(); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	if err := rdb.SAdd(ctx, "sys:index:routes", "a.example.com").Err(); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	routes := GetRoutesFromRedis(ctx, rdb)
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	// The domain is force-set from the index key, not read from the stored JSON
	// (which never carries a "domain" field for managed routes, per CreateServerRoute).
	if routes[0].Domain != "a.example.com" {
		t.Errorf("Domain = %q, want a.example.com", routes[0].Domain)
	}
	if routes[0].TargetIP != "10.0.0.9" || routes[0].TargetPort != 25565 {
		t.Errorf("route = %+v, unexpected fields", routes[0])
	}
}

func TestGetRoutesFromRedis_SkipsIndexEntryMissingItsKey(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	// Index references a domain whose route:{domain} key was never written /
	// already expired: it must be skipped, not returned as a zero-value route.
	if err := rdb.SAdd(ctx, "sys:index:routes", "stale.example.com").Err(); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	routes := GetRoutesFromRedis(ctx, rdb)
	if len(routes) != 0 {
		t.Fatalf("got %d routes, want 0 for a stale index entry", len(routes))
	}
}

func TestCountRoutesFromRedis(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	if err := rdb.SAdd(ctx, "sys:index:routes", "a.example.com", "b.example.com").Err(); err != nil {
		t.Fatalf("seed index: %v", err)
	}

	if got := CountRoutesFromRedis(ctx, rdb); got != 2 {
		t.Errorf("CountRoutesFromRedis = %d, want 2", got)
	}
}

func TestGetServiceErrorsFromRedis_PerStreamCountFloor(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	seedEntries := func(stream string, n int) {
		for i := 0; i < n; i++ {
			entryJSON, _ := json.Marshal(map[string]string{"ts": "now", "level": "ERROR", "source": stream, "message": "boom"})
			if _, err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, Values: map[string]interface{}{"data": entryJSON}}).Result(); err != nil {
				t.Fatalf("seed %s: %v", stream, err)
			}
		}
	}
	// Two instance streams for "gate": with count=4 and 2 streams, perStream
	// would be 2 (4/2) but the floor of 10 applies, so all 12 total entries
	// across both streams (well within 10-per-stream) must come back.
	seedEntries("dylaris:errors:gate:inst-1", 6)
	seedEntries("dylaris:errors:gate:inst-2", 6)

	entries := GetServiceErrorsFromRedis(rdb, "gate", 4)
	if len(entries) != 12 {
		t.Fatalf("got %d entries, want 12 (floor of 10-per-stream over 2 streams, each has 6)", len(entries))
	}
}

func TestGetServiceErrorsFromRedis_SingleStreamNoFloor(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		entryJSON, _ := json.Marshal(map[string]string{"ts": "now", "level": "ERROR", "source": "link", "message": "x"})
		if _, err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: "dylaris:errors:link:inst-1", Values: map[string]interface{}{"data": entryJSON}}).Result(); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// Single stream: no floor override, count=3 must cap the result at 3.
	entries := GetServiceErrorsFromRedis(rdb, "link", 3)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3 (count applied directly, single stream)", len(entries))
	}
}

func TestGetAllServiceErrorsFromRedis_OnlyNonEmptyServicesIncluded(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	entryJSON, _ := json.Marshal(map[string]string{"ts": "now", "level": "ERROR", "source": "edge", "message": "x"})
	if _, err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: "dylaris:errors:edge:inst-1", Values: map[string]interface{}{"data": entryJSON}}).Result(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// No streams seeded for "link".

	all := GetAllServiceErrorsFromRedis(rdb, 10)
	if _, ok := all["edge"]; !ok {
		t.Error("expected edge to be present")
	}
	if _, ok := all["link"]; ok {
		t.Error("link has no streams and must be omitted, not present with an empty slice")
	}
}

// The reader's service list must BE the shared one, not a copy of it.
//
// This test used to seed dylaris:errors:gate:* and assert "gate" came back,
// which passed for as long as the reader looked for a name no producer had
// written since the gate -> edge rename. It proved the function scans what the
// list says and nothing more - the list itself was never the thing under test,
// so the one error that mattered was invisible to it.
//
// errlog.Services is shared with the producers (and mirrored byte-identically
// into the gateway repo, where errlog's own test holds every errlog.New call
// against it), so pinning to it here closes the loop: a name can no longer be
// added on one side alone.
func TestErrorStreamServicesIsTheSharedList(t *testing.T) {
	if len(ErrorStreamServices) == 0 {
		t.Fatal("ErrorStreamServices is empty; nothing would ever be read")
	}
	for _, svc := range ErrorStreamServices {
		if !errlog.IsKnownService(svc) {
			t.Errorf("ErrorStreamServices has %q, which errlog.Services does not - producers will never write it", svc)
		}
	}
	for _, svc := range errlog.Services {
		found := false
		for _, have := range ErrorStreamServices {
			if have == svc {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("errlog.Services has %q but the reader does not scan it - those entries are written and never read", svc)
		}
	}
	// The rename that started all this, named explicitly so it cannot come back.
	if errlog.IsKnownService("gate") {
		t.Error(`"gate" is back in errlog.Services; the service is called "edge" and no producer writes "gate"`)
	}
}
