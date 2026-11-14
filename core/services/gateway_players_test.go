package services

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestSumFreshPlayers(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	got := sumFreshPlayers([]edgePlayersSample{
		{at: now.Add(-2 * time.Second), players: map[string]int{"a": 2, "b": 1}},
		{at: now.Add(-8 * time.Second), players: map[string]int{"a": 1}},
		// A dead edge's last sample: its players are on the sibling by now.
		{at: now.Add(-30 * time.Second), players: map[string]int{"a": 5, "c": 4}},
		{at: now, players: map[string]int{"d": 0}},
		{at: now, players: nil},
	}, now)
	if want := map[string]int{"a": 3, "b": 1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestStreamIDTime(t *testing.T) {
	at, ok := streamIDTime("1729700000123-4")
	if !ok || at.UnixMilli() != 1729700000123 {
		t.Fatalf("got %v %v", at, ok)
	}
	if _, ok := streamIDTime("garbage"); ok {
		t.Fatal("a malformed id parsed")
	}
}

// seedEdgeSample registers an edge and puts one stats sample in its stream, the
// way the edge's publisher does, aged by ago.
func seedEdgeSample(t *testing.T, rdb *redis.Client, edge, raw string, ago time.Duration) {
	t.Helper()
	ctx := context.Background()
	if err := rdb.Set(ctx, "edge:registry:"+edge, `{"edge_id":"`+edge+`"}`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(time.Now().Add(-ago).UnixMilli(), 10) + "-0"
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: "dylaris:edge:" + edge + ":stats", ID: id, Values: map[string]interface{}{"data": raw}}).Err(); err != nil {
		t.Fatal(err)
	}
}

func resetGatewayPlayersCache() {
	gatewayPlayersCache.Lock()
	gatewayPlayersCache.val, gatewayPlayersCache.routes, gatewayPlayersCache.at = nil, nil, time.Time{}
	gatewayPlayersCache.Unlock()
}

type playersSeed struct {
	edge, raw string
	ago       time.Duration
}

var playersCases = []struct {
	name       string
	samples    []playersSeed
	wantServer map[string]int
	wantRoute  map[string]RoutePlayers
}{
	{"edge from before the field", []playersSeed{{"e1", `{"players_by_server":{"s1":2}}`, time.Second}},
		map[string]int{"s1": 2}, map[string]RoutePlayers{}},
	{"new edge with both maps", []playersSeed{{"e1", `{"players_by_server":{"s1":2},"players_by_route":{"play.x":{"j":3,"b":1}}}`, time.Second}},
		map[string]int{"s1": 2}, map[string]RoutePlayers{"play.x": {Java: 3, Bedrock: 1}}},
	{"new edge with routes only", []playersSeed{{"e1", `{"players_by_route":{"play.x":{"b":2}}}`, time.Second}},
		map[string]int{}, map[string]RoutePlayers{"play.x": {Bedrock: 2}}},
	{"stale sample counts for neither", []playersSeed{{"e1", `{"players_by_server":{"s1":2},"players_by_route":{"play.x":{"j":3}}}`, 30 * time.Second}},
		map[string]int{}, map[string]RoutePlayers{}},
	{"two edges are summed, an old one beside a new one", []playersSeed{
		{"e1", `{"players_by_server":{"s1":1},"players_by_route":{"play.x":{"j":1,"b":1},"play.y":{"j":2}}}`, time.Second},
		{"e2", `{"players_by_server":{"s1":1}}`, 2 * time.Second},
		{"e3", `{"players_by_route":{"play.x":{"j":4}},"players_by_server":{"s2":5}}`, 3 * time.Second}},
		map[string]int{"s1": 2, "s2": 5}, map[string]RoutePlayers{"play.x": {Java: 5, Bedrock: 1}, "play.y": {Java: 2}}},
	{"wrong type for the route field keeps the server counts", []playersSeed{{"e1", `{"players_by_server":{"s1":2},"players_by_route":"oops"}`, time.Second}},
		map[string]int{"s1": 2}, map[string]RoutePlayers{}},
	{"wrong type inside an entry keeps the server counts", []playersSeed{{"e1", `{"players_by_server":{"s1":2},"players_by_route":{"play.x":{"j":"many"}}}`, time.Second}},
		map[string]int{"s1": 2}, map[string]RoutePlayers{}},
	{"negative numbers are ignored", []playersSeed{{"e1", `{"players_by_route":{"play.x":{"j":-3,"b":2},"play.y":{"j":-1}}}`, time.Second}},
		map[string]int{}, map[string]RoutePlayers{"play.x": {Bedrock: 2}}},
	{"zero and negative entries are dropped", []playersSeed{{"e1", `{"players_by_route":{"play.x":{"j":0,"b":-1},"play.y":{"j":1}}}`, time.Second}},
		map[string]int{}, map[string]RoutePlayers{"play.y": {Java: 1}}},
}

func TestGatewayPlayersCapsRouteEntriesPerSample(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"players_by_server":{"s1":1},"players_by_route":{`)
	for i := 0; i < maxRouteEntriesPerSample+500; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"d` + strconv.Itoa(i) + `":{"j":1}`)
	}
	b.WriteString(`}}`)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	resetGatewayPlayersCache()
	t.Cleanup(resetGatewayPlayersCache)
	seedEdgeSample(t, rdb, "e1", b.String(), time.Second)
	if got := len(GatewayPlayersByRoute(context.Background(), rdb)); got != maxRouteEntriesPerSample {
		t.Errorf("kept %d route entries, want the cap %d", got, maxRouteEntriesPerSample)
	}
	if got := GatewayPlayersByServer(context.Background(), rdb); got["s1"] != 1 {
		t.Errorf("server counts = %v", got)
	}
}

func TestGatewayPlayersToleratesOldAndNewSamples(t *testing.T) {
	for _, c := range playersCases {
		t.Run(c.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			resetGatewayPlayersCache()
			t.Cleanup(resetGatewayPlayersCache)
			for _, s := range c.samples {
				seedEdgeSample(t, rdb, s.edge, s.raw, s.ago)
			}
			if got := GatewayPlayersByServer(context.Background(), rdb); !reflect.DeepEqual(got, c.wantServer) {
				t.Errorf("by server = %v, want %v", got, c.wantServer)
			}
			if got := GatewayPlayersByRoute(context.Background(), rdb); !reflect.DeepEqual(got, c.wantRoute) {
				t.Errorf("by route = %v, want %v", got, c.wantRoute)
			}
		})
	}
}

// The server map is what the panel's player counts have always read: the same
// input must give the same answer as before the route map existed, and one read
// of the streams must serve both.
func TestGatewayPlayersByServerUnchanged(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	resetGatewayPlayersCache()
	t.Cleanup(resetGatewayPlayersCache)
	seedEdgeSample(t, rdb, "e1", `{"players_by_server":{"a":2,"b":1}}`, time.Second)
	seedEdgeSample(t, rdb, "e2", `{"players_by_server":{"a":1}}`, 8*time.Second)
	seedEdgeSample(t, rdb, "e3", `{"players_by_server":{"a":5,"c":4}}`, 30*time.Second)
	want := map[string]int{"a": 3, "b": 1}
	if got := GatewayPlayersByServer(context.Background(), rdb); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Served from the shared cache: a new sample does not show before the TTL.
	seedEdgeSample(t, rdb, "e4", `{"players_by_server":{"z":9}}`, 0)
	GatewayPlayersByRoute(context.Background(), rdb)
	if got := GatewayPlayersByServer(context.Background(), rdb); !reflect.DeepEqual(got, want) {
		t.Fatalf("second read = %v, want the cached %v", got, want)
	}
}
