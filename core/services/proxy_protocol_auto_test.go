package services

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"dylaris-core/store"
)

func TestProxyAutoWant(t *testing.T) {
	for _, c := range []struct {
		name            string
		ready           string
		platformOn      bool
		routes, withCap int
		want, ok        bool
	}{
		{"not reported: change nothing", "", true, 1, 1, false, false},
		{"expects the header, capable link: forward", "1", true, 1, 1, true, true},
		{"does not expect it: stop", "0", true, 1, 1, false, true},
		{"expects it, but a link cannot write it: do not", "1", true, 2, 1, false, true},
		{"expects it, no route at all: do not", "1", true, 0, 0, false, true},
		{"expects it, platform switch off: do not", "1", false, 1, 1, false, true},
	} {
		want, ok := proxyAutoWant(c.ready, c.platformOn, c.routes, c.withCap)
		if want != c.want || ok != c.ok {
			t.Errorf("%s: = %v, %v; want %v, %v", c.name, want, ok, c.want, c.ok)
		}
	}
}

type proxyAutoFake struct {
	servers []store.ProxyAutoServer
	applied map[int]bool
}

func (f *proxyAutoFake) ListProxyAutoServers() ([]store.ProxyAutoServer, error) {
	return f.servers, nil
}
func (f *proxyAutoFake) SetServerProxyProtocol(id int, enabled bool) error {
	f.applied[id] = enabled
	return nil
}

type proxyHubFake struct{ sent map[string]bool }

func (h *proxyHubFake) SetServerProxyProtocol(uuid string, enabled bool) error {
	h.sent[uuid] = enabled
	return nil
}

// The reconcile turns forwarding on for a server whose ping showed it expects
// the header, off for one that showed it does not, and leaves alone what the
// node has not reported - never guessing, since a wrong guess either locks
// players out or drops their address.
func TestReconcileProxyAutoFollowsTheServer(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()
	route := func(domain, uuid, tunnel string) {
		mr.SAdd("sys:index:routes", domain)
		mr.Set("route:"+domain, `{"target_ip":"mc_`+uuid+`","target_port":25565,"tunnel_id":"`+tunnel+`","server_uuid":"`+uuid+`"}`)
	}
	route("a.eu.example", "srv-a", "link-ok")
	route("b.eu.example", "srv-b", "link-ok")
	route("c.eu.example", "srv-c", "link-ok")
	route("d.eu.example", "srv-d", "link-old")
	mr.Set("online_link:link-ok", `{"caps":["client-addr"]}`)
	mr.Set("online_link:link-old", `{"caps":[]}`)
	mr.Set(ServerProxyReadyKey("srv-a"), "1") // expects it, off now -> on
	mr.Set(ServerProxyReadyKey("srv-b"), "0") // does not, on now -> off
	// srv-c: nothing reported, on now -> stays on
	mr.Set(ServerProxyReadyKey("srv-d"), "1") // expects it, but its link is old -> stays off

	st := &proxyAutoFake{applied: map[int]bool{}, servers: []store.ProxyAutoServer{
		{ID: 1, UUID: "srv-a", Active: false},
		{ID: 2, UUID: "srv-b", Active: true},
		{ID: 3, UUID: "srv-c", Active: true},
		{ID: 4, UUID: "srv-d", Active: false},
	}}
	hub := &proxyHubFake{sent: map[string]bool{}}
	ReconcileProxyAuto(ctx, st, rdb, hub, true)

	if len(st.applied) != 2 || st.applied[1] != true || st.applied[2] != false {
		t.Fatalf("applied %v, want server 1 on and server 2 off, nothing else", st.applied)
	}
	if hub.sent["srv-a"] != true || hub.sent["srv-b"] != false || len(hub.sent) != 2 {
		t.Fatalf("hub got %v", hub.sent)
	}
	if v, _ := mr.Get(ServerProxyProtocolKey("srv-a")); v != "1" {
		t.Fatalf("node key for srv-a = %q, want 1 so its ping sends the header first", v)
	}
	if mr.Exists(ServerProxyProtocolKey("srv-b")) {
		t.Fatal("node key for srv-b left behind")
	}
}
