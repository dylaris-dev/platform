package billing

import (
	"testing"

	"dylaris-core/store"
)

// What a pool IS, which is the same question as what an allowance covers.
//
// The stored breakdown is finer than the thing being judged: it carries a
// product, and for non-regional kinds a region as well. Every dimension left in
// the key is an allowance handed out again, so this is the function to look at
// first if a tenant is ever seen with twice the ceiling they bought.
func TestFoldTrafficCells(t *testing.T) {
	const gb = int64(1_000_000_000)

	t.Run("the two products share one pool", func(t *testing.T) {
		// The load-bearing one. Player traffic from a tenant's own node and from
		// their protected address is the same edge, the same region and the same
		// allowance - they only differ in who moved it. A product left in the
		// key would resolve the SAME limit twice and give this tenant two
		// ceilings where they bought one.
		got := foldTrafficCells([]store.RegionUsage{
			{Region: "eu-central", Kind: store.TrafficKindEdge, Product: store.TrafficProductBYON, Bytes: 5 * gb},
			{Region: "eu-central", Kind: store.TrafficKindEdge, Product: store.TrafficProductRoute, Bytes: 3 * gb},
		})
		if len(got) != 1 {
			t.Fatalf("got %d pools, want 1 - each extra pool is another full allowance", len(got))
		}
		if got[0].bytes != 8*gb {
			t.Errorf("pool total = %d GB, want 8", got[0].bytes/gb)
		}
		if got[0].byProduct[store.TrafficProductBYON] != 5*gb || got[0].byProduct[store.TrafficProductRoute] != 3*gb {
			t.Errorf("the split was lost in the fold: %+v", got[0].byProduct)
		}
	})

	t.Run("a non-regional kind folds onto one region", func(t *testing.T) {
		// File transfers hold a single global pool, so two regions in the stored
		// rows must not become two allowances.
		got := foldTrafficCells([]store.RegionUsage{
			{Region: "eu-central", Kind: store.TrafficKindRelay, Product: store.TrafficProductBYON, Bytes: 2 * gb},
			{Region: "us-east", Kind: store.TrafficKindRelay, Product: store.TrafficProductBYON, Bytes: 4 * gb},
		})
		if len(got) != 1 || got[0].bytes != 6*gb {
			t.Fatalf("got %+v, want one pool of 6 GB", got)
		}
	})

	t.Run("player traffic stays split per region", func(t *testing.T) {
		// The opposite property, and it has to hold in the same function: edge
		// traffic IS capped per region, because a terabyte does not cost the
		// same in both. Folding these would hide the region that stops them.
		got := foldTrafficCells([]store.RegionUsage{
			{Region: "eu-central", Kind: store.TrafficKindEdge, Bytes: 2 * gb},
			{Region: "us-east", Kind: store.TrafficKindEdge, Bytes: 4 * gb},
		})
		if len(got) != 2 {
			t.Fatalf("got %d pools, want 2 - one per region", len(got))
		}
	})

	t.Run("player and data traffic never share a pool", func(t *testing.T) {
		got := foldTrafficCells([]store.RegionUsage{
			{Region: "eu-central", Kind: store.TrafficKindEdge, Bytes: gb},
			{Region: "eu-central", Kind: store.TrafficKindRelay, Bytes: gb},
		})
		if len(got) != 2 {
			t.Fatalf("got %d pools, want 2 - they are priced and capped separately", len(got))
		}
	})

	t.Run("order follows first appearance, so the busiest pool is drawn first", func(t *testing.T) {
		got := foldTrafficCells([]store.RegionUsage{
			{Region: "us-east", Kind: store.TrafficKindEdge, Bytes: 9 * gb},
			{Region: "eu-central", Kind: store.TrafficKindEdge, Bytes: gb},
			{Region: "us-east", Kind: store.TrafficKindEdge, Product: store.TrafficProductRoute, Bytes: gb},
		})
		if len(got) != 2 || got[0].region != "us-east" {
			t.Fatalf("got %+v, want us-east first", got)
		}
	})

	t.Run("nothing stored is no pools, not a zero pool", func(t *testing.T) {
		if got := foldTrafficCells(nil); len(got) != 0 {
			t.Errorf("got %+v, want none", got)
		}
	})
}

// A pool with no traffic this month still has to be a pool.
//
// The breakdown table only gets a row once bytes have moved, so a tenant who
// has transferred no files had no relay cell at all - and the panel, drawing
// what it is given, dropped the whole data-traffic section. "You have used
// none" and "this is broken" then looked identical, which is the report this
// came from.
func TestWithIdleAllowances(t *testing.T) {
	const gb = int64(1_000_000_000)

	t.Run("the data-traffic pool exists even with nothing in it", func(t *testing.T) {
		got := withIdleAllowances([]trafficCell{
			{region: "eu-central", kind: store.TrafficKindEdge, bytes: 2 * gb},
		})
		var relay *trafficCell
		for i := range got {
			if got[i].kind == store.TrafficKindRelay {
				relay = &got[i]
			}
		}
		if relay == nil {
			t.Fatal("no data-traffic pool; the panel would show nothing at all for it")
		}
		if relay.bytes != 0 || relay.region != "*" {
			t.Errorf("idle pool = %+v, want the global region and no bytes", *relay)
		}
	})

	t.Run("real usage is never restated", func(t *testing.T) {
		// The one thing this must not do. Adding a second relay cell would put a
		// zero bar beside the real one and make the section contradict itself.
		got := withIdleAllowances([]trafficCell{
			{region: "*", kind: store.TrafficKindRelay, bytes: 7 * gb},
		})
		if len(got) != 1 || got[0].bytes != 7*gb {
			t.Fatalf("got %+v, want the one real pool untouched", got)
		}
	})

	t.Run("regional kinds are not invented", func(t *testing.T) {
		// A tenant does not HOLD a region: players either connect through it or
		// they do not. Seeding these would draw a bar for every region an
		// operator has ever priced.
		for _, c := range withIdleAllowances(nil) {
			if c.kind == store.TrafficKindEdge {
				t.Errorf("a player-traffic pool was invented for %q", c.region)
			}
		}
	})
}
