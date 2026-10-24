package services

import (
	"context"
	"testing"

	"dylaris-core/models"
)

// A move (manual or rebalance) keeps Real player IP only behind a link that
// announced client-addr: a missing key, an old link's "1", or caps without it
// all read as "cannot carry it".
func TestTargetCarriesClientAddr(t *testing.T) {
	g, rdb, _ := newHubBridgeTestGateway(t)
	o := &MigrationOrchestrator{gateway: g, redis: rdb}
	target := &models.Node{ID: 2, Token: "node-2", LinkToken: "link-2"}
	ctx := context.Background()

	if o.TargetCarriesClientAddr(ctx, target) {
		t.Fatal("an offline link carries the address")
	}
	for v, want := range map[string]bool{
		"1":                        false,
		`{"caps":[]}`:              false,
		`{"caps":["client-addr"]}`: true,
	} {
		rdb.Set(ctx, "online_link:link-2", v, 0)
		if got := o.TargetCarriesClientAddr(ctx, target); got != want {
			t.Errorf("presence %q: got %v, want %v", v, got, want)
		}
	}
}
