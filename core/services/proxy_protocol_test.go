package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dylaris-core/models"
)

// A route created for a server that already has the switch on carries it, so
// the hub never publishes it without the flag first.
func TestCreateServerRoute_CarriesTheServersProxyProtocol(t *testing.T) {
	for _, on := range []bool{false, true} {
		g, rdb, fs := newHubBridgeTestGateway(t)
		fs.nodes[1] = models.Node{ID: 1, Token: "node-tok-1"}
		fs.servers[10] = models.Server{ID: 10, UUID: "srv-uuid-10", NodeID: 1, ProxyProtocol: on}
		if err := g.CreateServerRoute(10, "owner-1", "sub.example.com", 25565); err != nil {
			t.Fatalf("CreateServerRoute: %v", err)
		}
		msgs := readHubQueueMessages(t, rdb)
		if len(msgs) != 1 || msgs[0].ProxyProtocol != on {
			t.Fatalf("server proxy_protocol=%v: queued %+v", on, msgs)
		}
	}
}

// The hub reads exactly these fields; false is omitted, which the hub reads as
// false too.
func TestSetServerProxyProtocol_QueuesTheAction(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	if err := g.SetServerProxyProtocol("srv-uuid-10", true); err != nil {
		t.Fatal(err)
	}
	if err := g.SetServerProxyProtocol("srv-uuid-10", false); err != nil {
		t.Fatal(err)
	}
	raw, err := rdb.LRange(context.Background(), hubQueueKey, 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`{"action":"set_server_proxy_protocol","server_uuid":"srv-uuid-10","proxy_protocol":true}`,
		`{"action":"set_server_proxy_protocol","server_uuid":"srv-uuid-10"}`,
	}
	if strings.Join(raw, "\n") != strings.Join(want, "\n") {
		t.Fatalf("queue =\n%s\nwant\n%s", strings.Join(raw, "\n"), strings.Join(want, "\n"))
	}
}

// The route-only switch rewrites the row and the live value, the republisher
// finds nothing to correct, and a later edit of the target keeps the switch.
func TestSetCoreOwnedRouteProxyProtocol_SurvivesRepublishAndEdit(t *testing.T) {
	g, rdb, fs := newHubBridgeTestGateway(t)
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tunnel-1", "127.0.0.1", 25565); err != nil {
		t.Fatalf("CreateRouteViaLink: %v", err)
	}
	if err := g.SetCoreOwnedRouteProxyProtocol("play.example.com", true); err != nil {
		t.Fatalf("SetCoreOwnedRouteProxyProtocol: %v", err)
	}
	if !fs.routes["play.example.com"].ProxyProtocol {
		t.Fatal("stored row not switched on")
	}
	rp := NewRouteRepublisher(fs, rdb)
	if got := rp.republish(context.Background(), fs.routes["play.example.com"]); got != republishUnchanged {
		t.Fatalf("republish after the switch = %v, want unchanged (nothing to correct)", got)
	}
	rp.RunOnce(context.Background())
	if r := liveRoute(t, rp, "play.example.com"); !r.ProxyProtocol || !r.CoreOwned || r.OwnerID != "owner-1" {
		t.Fatalf("live route after republish = %+v, want proxy_protocol kept", r)
	}

	// The tenant changes the port: same domain posted again.
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tunnel-1", "127.0.0.1", 25570); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if r := liveRoute(t, rp, "play.example.com"); !r.ProxyProtocol || r.TargetPort != 25570 {
		t.Fatalf("live route after edit = %+v, want port moved and switch kept", r)
	}

	if err := g.SetCoreOwnedRouteProxyProtocol("play.example.com", false); err != nil {
		t.Fatal(err)
	}
	rp.RunOnce(context.Background())
	if r := liveRoute(t, rp, "play.example.com"); r.ProxyProtocol {
		t.Fatalf("live route after switching off = %+v", r)
	}
}

func TestSetCoreOwnedRouteProxyProtocol_UnknownDomain(t *testing.T) {
	g, _, _ := newHubBridgeTestGateway(t)
	if err := g.SetCoreOwnedRouteProxyProtocol("nope.example.com", true); !errors.Is(err, ErrRouteNotStored) {
		t.Fatalf("err = %v, want ErrRouteNotStored", err)
	}
}

// Another owner's live entry on the domain is never overwritten by the switch.
func TestSetCoreOwnedRouteProxyProtocol_DoesNotTakeAForeignEntry(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	if err := g.CreateRouteViaLink("owner-1", "play.example.com", "tunnel-1", "127.0.0.1", 25565); err != nil {
		t.Fatal(err)
	}
	foreign := `{"domain":"play.example.com","target_ip":"mc_x","target_port":25565,"tunnel_id":"t2","server_uuid":"x"}`
	if err := rdb.Set(context.Background(), "route:play.example.com", foreign, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := g.SetCoreOwnedRouteProxyProtocol("play.example.com", true); err == nil {
		t.Fatal("want an error when the live entry is someone else's")
	}
	if v, _ := rdb.Get(context.Background(), "route:play.example.com").Result(); v != foreign {
		t.Fatalf("foreign entry rewritten: %s", v)
	}
}
