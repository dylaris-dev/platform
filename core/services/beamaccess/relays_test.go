package beamaccess

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func seedRelays(t *testing.T, relays ...BeamRelayInfo) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	for _, r := range relays {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal relay: %v", err)
		}
		if err := rdb.SAdd(ctx, beamRegistrySet, r.BeamID).Err(); err != nil {
			t.Fatalf("SAdd: %v", err)
		}
		if err := rdb.Set(ctx, "beam:registry:"+r.BeamID, raw, 0).Err(); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	return rdb
}

// The relay must sit near the NODE, not the client: the payload path is
// client -> relay -> node, so picking by anything other than the node's region
// can double a transatlantic hop.
func TestPickBeamRelayPrefersTheNodesRegion(t *testing.T) {
	rdb := seedRelays(t,
		BeamRelayInfo{BeamID: "eu-1", Region: "eu-central", PublicHost: "beam.eu.example.com", ClientPort: "25550"},
		BeamRelayInfo{BeamID: "us-1", Region: "us-west", PublicHost: "beam.us.example.com", ClientPort: "25550"},
	)
	ctx := context.Background()

	// Repeated because the choice among equals is random; a region filter that
	// only worked sometimes would pass a single draw.
	for i := 0; i < 25; i++ {
		got, ok := PickBeamRelay(ctx, rdb, "us-west")
		if !ok {
			t.Fatal("PickBeamRelay found no relay")
		}
		if got.BeamID != "us-1" {
			t.Fatalf("picked %q, want us-1", got.BeamID)
		}
	}
}

// A distant relay beats no transfer at all, and because clients re-resolve on
// every connect the next one returns to the right region on its own.
func TestPickBeamRelayFallsBackToAnotherRegion(t *testing.T) {
	rdb := seedRelays(t,
		BeamRelayInfo{BeamID: "eu-1", Region: "eu-central", ClientPort: "25550"},
	)
	got, ok := PickBeamRelay(context.Background(), rdb, "us-west")
	if !ok {
		t.Fatal("PickBeamRelay found no relay; a region with none must fall back, not fail")
	}
	if got.BeamID != "eu-1" {
		t.Fatalf("picked %q, want eu-1", got.BeamID)
	}
}

func TestPickBeamRelayWithoutAPreferredRegion(t *testing.T) {
	rdb := seedRelays(t, BeamRelayInfo{BeamID: "any-1", ClientPort: "25550"})
	if _, ok := PickBeamRelay(context.Background(), rdb, ""); !ok {
		t.Error("PickBeamRelay with no preference found nothing")
	}
}

func TestPickBeamRelayRegionMatchIsCaseInsensitive(t *testing.T) {
	rdb := seedRelays(t,
		BeamRelayInfo{BeamID: "eu-1", Region: "EU-Central", ClientPort: "25550"},
		BeamRelayInfo{BeamID: "us-1", Region: "us-west", ClientPort: "25550"},
	)
	got, ok := PickBeamRelay(context.Background(), rdb, "eu-central")
	if !ok || got.BeamID != "eu-1" {
		t.Errorf("picked %q (ok=%t), want eu-1 - casing must not decide routing", got.BeamID, ok)
	}
}

// A relay that reports its own name knows better than one fleet-wide setting:
// with several regions there is no single correct hostname. The setting stays
// the fallback for relays predating BEAM_PUBLIC_HOST.
func TestResolveRelayPrefersTheRelaysOwnHost(t *testing.T) {
	ctx := context.Background()

	t.Run("relay reports its own host", func(t *testing.T) {
		rdb := seedRelays(t, BeamRelayInfo{
			BeamID: "eu-1", Region: "eu-central",
			PublicHost: "beam.eu.example.com", ClientPort: "25550",
		})
		addr, src := ResolveRelay(ctx, rdb, "", "fleet-wide.example.com", "eu-central")
		if addr != "beam.eu.example.com:25550" {
			t.Errorf("addr = %q, want the relay's own host", addr)
		}
		if src != "discovered" {
			t.Errorf("source = %q, want discovered", src)
		}
	})

	t.Run("pre-region relay falls back to the setting", func(t *testing.T) {
		rdb := seedRelays(t, BeamRelayInfo{BeamID: "old-1", IP: "10.0.0.9", ClientPort: "25550"})
		addr, _ := ResolveRelay(ctx, rdb, "", "fleet-wide.example.com", "")
		if addr != "fleet-wide.example.com:25550" {
			t.Errorf("addr = %q, want the fleet-wide setting - an old relay must keep working", addr)
		}
	})

	t.Run("manual override still wins outright", func(t *testing.T) {
		rdb := seedRelays(t, BeamRelayInfo{
			BeamID: "eu-1", Region: "eu-central",
			PublicHost: "beam.eu.example.com", ClientPort: "25550",
		})
		addr, src := ResolveRelay(ctx, rdb, "incident.example.com:1234", "fleet-wide.example.com", "eu-central")
		if addr != "incident.example.com:1234" || src != "manual" {
			t.Errorf("addr = %q src = %q, want the manual override", addr, src)
		}
	})
}
