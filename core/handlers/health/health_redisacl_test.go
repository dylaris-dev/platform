package health

import (
	"context"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	mrserver "github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
)

// aclHealthStore serves the two lookups redisACLComponent makes. Embeds
// store.Store so it satisfies the interface; anything else would panic, and
// nothing else is called.
type aclHealthStore struct {
	store.Store
	nodes   []models.Node
	servers map[int][]models.Server
}

func (f *aclHealthStore) ListNodes() ([]models.Node, error) { return f.nodes, nil }
func (f *aclHealthStore) ListServersByNode(id int) ([]models.Server, error) {
	return f.servers[id], nil
}

// aclHealthHandler builds a handler over miniredis, with the given ACL usernames
// already present.
//
// miniredis has no ACL command, so "ACL USERS" is stubbed. That is honest here:
// the behaviour under test is the comparison between what Core expects and what
// the server reports, not Valkey's own ACL engine, which has its own coverage in
// services/redisacl.
func aclHealthHandler(t *testing.T, existing []string, nodes []models.Node, servers map[int][]models.Server) *HealthHandler {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	reply := make([]any, 0, len(existing))
	for _, u := range existing {
		reply = append(reply, u)
	}
	mr.Server().SetPreHook(func(p *mrserver.Peer, cmd string, args ...string) bool {
		if !strings.EqualFold(cmd, "ACL") || len(args) == 0 || !strings.EqualFold(args[0], "USERS") {
			return false
		}
		p.WriteLen(len(reply))
		for _, u := range reply {
			p.WriteBulk(u.(string))
		}
		return true
	})
	return NewHealthHandler(Deps{
		Store: &aclHealthStore{nodes: nodes, servers: servers},
		Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()}),
	})
}

var aclHealthNodes = []models.Node{{ID: 1, Name: "node-a", Token: "tok-a"}}
var aclHealthServers = map[int][]models.Server{1: {{ID: 10, UUID: "srv-1"}, {ID: 11, UUID: "srv-2"}}}

// The happy path: every user the provisioner is supposed to have created is
// there, including ONE shipper user per server. That per-server count is the
// isolation itself - a single shared shipper user would be granted every
// server's keys on the machine, and dylaris:server:<uuid>:input is a stdin
// bridge into the JVM.
func TestRedisACLComponentUpWhenEveryScopedUserExists(t *testing.T) {
	all := []string{
		redisacl.NodeUsername("tok-a"),
		redisacl.LinkUsername("tok-a"),
		redisacl.ShipperUsername("tok-a", "srv-1"),
		redisacl.ShipperUsername("tok-a", "srv-2"),
	}
	h := aclHealthHandler(t, all, aclHealthNodes, aclHealthServers)

	comp := h.redisACLComponent(context.Background(), true)
	if comp.Status != "up" {
		t.Fatalf("Status = %q (%s / %s), want up", comp.Status, comp.Detail, comp.Reason)
	}
	if !strings.Contains(comp.Detail, "4") {
		t.Errorf("Detail = %q, want it to count all four scoped users", comp.Detail)
	}
}

// The failure this exists for. The prune sweep only ever looked for users that
// should NOT exist; a user that should exist and does not has no symptom at all
// on the Core side. The shipper for that one server gets NOPERM, buffers, and
// retries forever - so the console goes blank in the panel while the container
// stays Up and every other health signal stays green.
func TestRedisACLComponentNamesTheMissingShipperUser(t *testing.T) {
	gone := redisacl.ShipperUsername("tok-a", "srv-2")
	h := aclHealthHandler(t, []string{
		redisacl.NodeUsername("tok-a"),
		redisacl.LinkUsername("tok-a"),
		redisacl.ShipperUsername("tok-a", "srv-1"),
	}, aclHealthNodes, aclHealthServers)

	comp := h.redisACLComponent(context.Background(), true)
	if comp.Status != "degraded" {
		t.Fatalf("Status = %q, want degraded", comp.Status)
	}
	if comp.Cause != "acl_users_missing" {
		t.Errorf("Cause = %q, want acl_users_missing", comp.Cause)
	}
	// Named, not counted: an operator has to know WHICH server lost its console.
	if !strings.Contains(comp.Reason, gone) {
		t.Errorf("Reason = %q, want it to name %q", comp.Reason, gone)
	}
	if len(comp.Items) != 1 || comp.Items[0].Name != gone {
		t.Errorf("Items = %+v, want exactly the missing user as a row", comp.Items)
	}
}

// Redis being down is already its own component and its own row. Repeating it
// here as a second failure would double-count one outage and bury the specific
// signal this component exists to give.
func TestRedisACLComponentStaysQuietWhileRedisIsDown(t *testing.T) {
	h := aclHealthHandler(t, nil, aclHealthNodes, aclHealthServers)
	comp := h.redisACLComponent(context.Background(), false)
	if comp.Status != "disabled" {
		t.Fatalf("Status = %q, want disabled while Redis is unreachable", comp.Status)
	}
}
