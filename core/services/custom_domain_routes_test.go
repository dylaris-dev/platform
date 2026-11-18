package services

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type ownerServers map[string][]models.Server

func (o ownerServers) ListServersByOwner(ownerID string) ([]models.Server, error) {
	return o[ownerID], nil
}

type recordingDeleter struct{ deleted []string }

func (d *recordingDeleter) DeleteRoute(domain string) error {
	d.deleted = append(d.deleted, domain)
	return nil
}

// Routes in the shape each publisher actually writes. The hub publishes a
// managed route with tunnel_id/target_ip/target_port/server_uuid and NO
// owner_id (gateway/hub/pkg/hub/service.go); only Core's route-only entries
// carry one. A fixture that gave managed routes an owner_id is what hid this.
func seedRoutes(t *testing.T, rdb *redis.Client, routes map[string]map[string]interface{}) {
	t.Helper()
	ctx := context.Background()
	for domain, fields := range routes {
		b, _ := json.Marshal(fields)
		rdb.Set(ctx, "route:"+domain, b, 0)
		rdb.SAdd(ctx, "sys:index:routes", domain)
	}
}

func TestAnUnprovenDomainLosesItsManagedRoutesToo(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	seedRoutes(t, rdb, map[string]map[string]interface{}{
		// Tenant u1's managed server route on the domain they failed to prove.
		"play.victim.net": {"tunnel_id": "tok", "target_ip": "mc_srv-u1", "target_port": 25565, "server_uuid": "srv-u1"},
		// u1's routes UNDER it have claims of their own and are judged by those:
		// one might be verified.
		"eu.play.victim.net":  {"tunnel_id": "tok", "target_ip": "mc_srv-u1", "server_uuid": "srv-u1"},
		"hub.play.victim.net": {"tunnel_id": "tok", "core_owned": true, "owner_id": "u1"},
		// u1's route on a different domain.
		"play.other.net": {"tunnel_id": "tok", "target_ip": "mc_srv-u1", "server_uuid": "srv-u1"},
	})
	servers := ownerServers{"u1": {{UUID: "srv-u1"}}, "u2": {{UUID: "srv-u2"}}}
	del := &recordingDeleter{}

	if err := NewCustomDomainRouteRemover(rdb, del, servers, noRows{}, nil, nil, nil).DeleteRoutesForDomain(context.Background(), "u1", "Play.Victim.net"); err != nil {
		t.Fatal(err)
	}
	sort.Strings(del.deleted)
	want := []string{"play.victim.net"}
	if len(del.deleted) != len(want) {
		t.Fatalf("deleted %v, want %v", del.deleted, want)
	}
	for i := range want {
		if del.deleted[i] != want[i] {
			t.Fatalf("deleted %v, want %v", del.deleted, want)
		}
	}
}

func TestRouteHeldBy(t *testing.T) {
	owned := map[string]bool{"srv-1": true}
	cases := []struct {
		name string
		rt   GatewayRoute
		want bool
	}{
		{"route-only, own", GatewayRoute{OwnerID: "u1", CoreOwned: true}, true},
		{"route-only, foreign", GatewayRoute{OwnerID: "u2", CoreOwned: true}, false},
		{"managed, own server", GatewayRoute{ServerUUID: "srv-1"}, true},
		{"managed, foreign server", GatewayRoute{ServerUUID: "srv-2"}, false},
		{"managed, legacy target only", GatewayRoute{TargetIP: "mc_srv-1"}, true},
		{"owner-less route-only entry", GatewayRoute{CoreOwned: true, ServerUUID: "srv-1"}, false},
		{"no server at all", GatewayRoute{TargetIP: "10.0.0.5"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RouteHeldBy(tc.rt, "u1", owned); got != tc.want {
				t.Fatalf("RouteHeldBy = %v, want %v", got, tc.want)
			}
		})
	}
}

// Another tenant's route on the name is never this tenant's to remove, whichever
// kind it is.
func TestTheRemoverLeavesOtherTenantsAlone(t *testing.T) {
	for name, fields := range map[string]map[string]interface{}{
		"managed":    {"tunnel_id": "tok", "target_ip": "mc_srv-u2", "server_uuid": "srv-u2"},
		"route-only": {"tunnel_id": "tok", "core_owned": true, "owner_id": "u2"},
	} {
		t.Run(name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			seedRoutes(t, rdb, map[string]map[string]interface{}{"play.victim.net": fields})
			del := &recordingDeleter{}
			servers := ownerServers{"u1": {{UUID: "srv-u1"}}, "u2": {{UUID: "srv-u2"}}}
			if err := NewCustomDomainRouteRemover(rdb, del, servers, noRows{}, nil, nil, nil).DeleteRoutesForDomain(context.Background(), "u1", "play.victim.net"); err != nil {
				t.Fatal(err)
			}
			if len(del.deleted) != 0 {
				t.Fatalf("removed another tenant's route: %v", del.deleted)
			}
		})
	}
}

// An unreadable Redis is an error, not an empty routing table.
func TestTheRemoverFailsWhenRedisCannotBeRead(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	mr.Close()
	err := NewCustomDomainRouteRemover(rdb, &recordingDeleter{}, ownerServers{}, noRows{}, nil, nil, nil).DeleteRoutesForDomain(context.Background(), "u1", "play.victim.net")
	if err == nil {
		t.Fatal("an unreadable Redis reported success")
	}
}

// noRows is an account with no route-only rows.
type noRows struct{}

func (noRows) ListCoreLinkRoutesByOwner(string) ([]store.CoreLinkRoute, error) { return nil, nil }

type ownerRows []store.CoreLinkRoute

func (o ownerRows) ListCoreLinkRoutesByOwner(ownerID string) ([]store.CoreLinkRoute, error) {
	var out []store.CoreLinkRoute
	for _, r := range o {
		if r.OwnerID == ownerID {
			out = append(out, r)
		}
	}
	return out, nil
}

// A suspended route-only route has no Redis key, only its row. A claim that
// fails or lapses must still take it, or an unsuspend publishes it unproven.
func TestTheRemoverDeletesStoredRoutesRedisCannotSee(t *testing.T) {
	suspended := time.Now()
	cases := []struct {
		name string
		rows ownerRows
		want []string
	}{
		{"suspended own row", ownerRows{{Domain: "play.victim.net", OwnerID: "u1", SuspendedAt: &suspended}}, []string{"play.victim.net"}},
		{"another tenant's row", ownerRows{{Domain: "play.victim.net", OwnerID: "u2", SuspendedAt: &suspended}}, nil},
		{"own row under the domain", ownerRows{{Domain: "eu.play.victim.net", OwnerID: "u1"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			rdb.SAdd(context.Background(), "sys:index:routes", "unrelated.net")
			del := &recordingDeleter{}
			if err := NewCustomDomainRouteRemover(rdb, del, ownerServers{}, tc.rows, nil, nil, nil).DeleteRoutesForDomain(context.Background(), "u1", "Play.Victim.net"); err != nil {
				t.Fatal(err)
			}
			if strings.Join(del.deleted, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("deleted %v, want %v", del.deleted, tc.want)
			}
		})
	}
}
