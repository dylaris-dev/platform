package servers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/handlers"
	"dylaris-core/models"
	"dylaris-core/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// Auto-placement chooses a machine that nobody named. It must not choose one
// belonging to a customer.
//
// A tenant's own request has been owner-scoped since BYON shipped. An ADMIN's
// had no scope at all, so the candidate list was the whole fleet - and a
// customer's BYON box is often the emptiest machine in its region, which makes
// it the one this scheduler prefers. The customer has root on that hardware.
func TestPickNodePlatformOnlyKeepsAdminPlacementOffTenantHardware(t *testing.T) {
	tenant := "tenant-a"
	// The tenant node is listed FIRST and is otherwise ideal, so nothing but
	// ownership can be what excludes it.
	nodes := []models.Node{
		ownedNode(1, "tenant-box", &tenant),
		ownedNode(2, "platform-box", nil),
	}
	h := newPlacementHandler(t, nodes)

	t.Run("without the flag every node is a candidate", func(t *testing.T) {
		got := h.pickNode(context.Background(), PickNodeRequest{RAMMB: 1024})
		if !got.Success || got.Picked == nil {
			t.Fatalf("no pick: %s", got.Reason)
		}
		if len(got.Candidates) != 2 {
			t.Errorf("candidates = %d, want both nodes for the preview endpoint", len(got.Candidates))
		}
	})

	t.Run("with the flag the tenant node is not considered at all", func(t *testing.T) {
		got := h.pickNode(context.Background(), PickNodeRequest{RAMMB: 1024, PlatformOnly: true})
		if !got.Success || got.Picked == nil {
			t.Fatalf("no pick: %s", got.Reason)
		}
		if got.Picked.NodeID != 2 {
			t.Errorf("picked node %d, want the platform node", got.Picked.NodeID)
		}
		for _, c := range got.Candidates {
			if c.NodeID == 1 {
				t.Error("the tenant node was scored as a candidate; it must not be considered")
			}
		}
	})

	// With only tenant hardware around, the answer is "nowhere", not "their box
	// will do".
	t.Run("nothing platform-owned means no pick", func(t *testing.T) {
		other := "tenant-b"
		h := newPlacementHandler(t, []models.Node{
			ownedNode(1, "a-box", &tenant),
			ownedNode(2, "b-box", &other),
		})
		got := h.pickNode(context.Background(), PickNodeRequest{RAMMB: 1024, PlatformOnly: true})
		if got.Success || got.Picked != nil {
			t.Fatalf("picked %+v, want no eligible node", got.Picked)
		}
		// And it has to SAY why. An operator looking at two online nodes with
		// room, told only "no node available", goes looking for a capacity
		// problem that is not there.
		if !strings.Contains(got.Reason, "customer-owned") {
			t.Errorf("reason = %q, want it to name the nodes that were passed over", got.Reason)
		}
	})

	// The tenant path must be untouched: their own scope still finds their own
	// machine, and the two flags are not the same switch.
	t.Run("a tenant still reaches their own node", func(t *testing.T) {
		got := h.pickNode(context.Background(), PickNodeRequest{RAMMB: 1024, OwnerScope: &tenant})
		if !got.Success || got.Picked == nil || got.Picked.NodeID != 1 {
			t.Fatalf("picked %+v, want the tenant's own node", got.Picked)
		}
	})
}

// The scheduler honouring PlatformOnly proves the flag works. It does not
// prove anything SETS it, and that gap is how a fix ships green and inert.
// This drives the decision itself, once per caller shape.
func TestApplyPlacementScopeNamesTheCallerParty(t *testing.T) {
	byonOn := &placementFakeStore{settings: map[string]string{"feature_byon_enabled": "true"}}
	byonOff := &placementFakeStore{settings: map[string]string{}}

	request := func(userID string, admin bool) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/servers", nil)
		// Plain string keys, because that is what the production code reads
		// (utils.go IsAdmin, tenancy.go byonCallerID) and what every other
		// handler test here uses. A typed key would compile and match nothing.
		//nolint:staticcheck // SA1029: the codebase's own convention
		ctx := context.WithValue(r.Context(), "userID", userID)
		//nolint:staticcheck // SA1029
		ctx = context.WithValue(ctx, "isAdmin", admin)
		return r.WithContext(ctx)
	}

	t.Run("an admin auto-placing is fenced to platform nodes", func(t *testing.T) {
		st := &handlers.AppState{Store: byonOn, FeatureFlags: services.NewFeatureFlags(byonOn)}
		var req PickNodeRequest
		applyPlacementScope(st, request("admin-1", true), &req)
		if !req.PlatformOnly {
			t.Error("PlatformOnly is false for an admin; a customer's machine is a candidate again")
		}
		if req.OwnerScope != nil {
			t.Errorf("OwnerScope = %v, want nil for an admin", *req.OwnerScope)
		}
	})

	t.Run("a tenant is scoped to their own nodes", func(t *testing.T) {
		st := &handlers.AppState{Store: byonOn, FeatureFlags: services.NewFeatureFlags(byonOn)}
		var req PickNodeRequest
		applyPlacementScope(st, request("tenant-a", false), &req)
		if req.OwnerScope == nil || *req.OwnerScope != "tenant-a" {
			t.Errorf("OwnerScope = %v, want tenant-a", req.OwnerScope)
		}
		if req.PlatformOnly {
			t.Error("PlatformOnly is set for a tenant; they would lose their own node")
		}
	})

	// With BYON off no node has an owner, so scoping would only be a way to
	// break the single-operator panel.
	t.Run("BYON off scopes nothing", func(t *testing.T) {
		st := &handlers.AppState{Store: byonOff, FeatureFlags: services.NewFeatureFlags(byonOff)}
		for _, admin := range []bool{true, false} {
			var req PickNodeRequest
			applyPlacementScope(st, request("someone", admin), &req)
			if req.PlatformOnly || req.OwnerScope != nil {
				t.Errorf("admin=%v: scoped with BYON off (%v, %v)", admin, req.PlatformOnly, req.OwnerScope)
			}
		}
	})
}

// The wizard calls the preview endpoint to show which node the create is about
// to choose - "so admins aren't deploying blind", in its own comment. So the
// two have to answer the same question. Scoping only the create would have
// produced a preview naming a machine the create then refuses to use, which is
// worse than either behaviour on its own.
func TestPickNodeEndpointPreviewsTheSameNodeCreateWouldUse(t *testing.T) {
	tenant := "tenant-a"
	nodes := []models.Node{
		ownedNode(1, "tenant-box", &tenant),
		ownedNode(2, "platform-box", nil),
	}
	fs := &placementFakeStore{nodes: nodes, settings: map[string]string{
		"routing_mode":         "gateway",
		"feature_byon_enabled": "true",
	}}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	h := &PlacementHandler{state: &handlers.AppState{
		Store: fs, Redis: rdb, FeatureFlags: services.NewFeatureFlags(fs),
	}}

	r := httptest.NewRequest(http.MethodPost, "/api/placement/pick",
		strings.NewReader(`{"ramMb":1024}`))
	//nolint:staticcheck // SA1029: the codebase's own context-key convention
	r = r.WithContext(context.WithValue(r.Context(), "isAdmin", true))
	rw := httptest.NewRecorder()
	h.PickNode(rw, r)

	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rw.Code, rw.Body.String())
	}
	var got PickNodeResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Picked == nil || got.Picked.NodeID != 2 {
		t.Fatalf("preview picked %+v, want the platform node - the create will pick that one", got.Picked)
	}
	for _, c := range got.Candidates {
		if c.NodeID == 1 {
			t.Error("the preview offered a customer's node that the create would not use")
		}
	}
}
