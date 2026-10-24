package billing

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

// TestProvision_RoutePool pins where the purchased ADDRESS pool lands. It is not
// a user_billing column: routes were already capped by gateway_route_limits, and
// keeping a second cap for the same thing is how the two drift apart. The scope
// string matters - effectiveRouteLimit resolves "user:<id>" then "user_default"
// then "global", so a wrong scope does not fail, it silently falls through to the
// platform default and hands the tenant the wrong number of addresses.
func TestProvision_RoutePool(t *testing.T) {
	const uid = "u1"

	t.Run("a purchased pool is written to the per-user scope", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{uid: {ID: uid}}}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{
			"uuid": uid, "action": "activate", "maxNodes": 2, "maxRoutes": 12,
		}, storeLinkTestKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.routeLimitSets) != 1 {
			t.Fatalf("routeLimitSets = %+v, want exactly 1", fs.routeLimitSets)
		}
		if got := fs.routeLimitSets[0]; got.scope != "user:"+uid || got.max == nil || *got.max != 12 {
			t.Fatalf("wrote %+v, want {user:%s 12}", got, uid)
		}
	})

	t.Run("an omitted pool touches nothing", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{uid: {ID: uid}}}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{
			"uuid": uid, "action": "activate", "maxNodes": 2,
		}, storeLinkTestKey))
		if len(fs.routeLimitSets) != 0 || len(fs.routeLimitDeletes) != 0 {
			t.Fatalf("sets=%+v deletes=%+v, want neither", fs.routeLimitSets, fs.routeLimitDeletes)
		}
	})

	// An explicit NULL - and only that - deletes the row. The scope chain treats a
	// missing per-user row as "use the platform default", which is what "the store
	// has nothing to say about this account" should mean.
	t.Run("a null pool deletes the row", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{uid: {ID: uid}}}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{
			"uuid": uid, "action": "activate", "maxRoutes": nil,
		}, storeLinkTestKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.routeLimitSets) != 0 {
			t.Fatalf("wrote %+v, want a delete instead", fs.routeLimitSets)
		}
		if len(fs.routeLimitDeletes) != 1 || fs.routeLimitDeletes[0] != "user:"+uid {
			t.Fatalf("deletes = %+v, want [user:%s]", fs.routeLimitDeletes, uid)
		}
	})

	// A ZERO is written, and this subtest used to assert the opposite.
	//
	// The reasoning it carried was that "a stored 0 would read as a hard zero and
	// leave the tenant unable to point a single address anywhere". Two things are
	// wrong with that. A hard zero is precisely what this table's user scope
	// already means by 0 - GetUserRouteLimit reports mode "disabled" for it - and
	// the tenant is not stranded: the refusal itself says "Point your own domain at
	// us instead - that is unlimited", and custom domains are neither capped nor
	// counted.
	//
	// What the delete actually did was fall through to user_default and then to
	// global, and with neither configured that is NO LIMIT. So the one number
	// meaning "this account gets no addresses on our domains" produced unlimited
	// addresses - the inverse of the zero-means-unlimited trap, arriving because
	// max_routes was parsed with max_nodes' convention.
	//
	// The store only ever sends 0 here when the tenant HAS bought something and the
	// operator has set the per-unit allowances to zero, i.e. "my products include
	// no addresses". That is an instruction, not an absence, and JSON already tells
	// the two apart: 0 against null.
	t.Run("a zero pool is written, because zero is an answer here", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{uid: {ID: uid}}}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{
			"uuid": uid, "action": "activate", "maxRoutes": 0,
		}, storeLinkTestKey))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if len(fs.routeLimitDeletes) != 0 {
			t.Fatalf("deleted %+v; the tenant then falls through to the platform default, which is unlimited when none is set", fs.routeLimitDeletes)
		}
		got := fs.routeLimitSets
		if len(got) != 1 || got[0].scope != "user:"+uid || got[0].max == nil || *got[0].max != 0 {
			t.Fatalf("sets = %+v, want one write of a cap of 0 on user:%s", got, uid)
		}
	})

	t.Run("suspend never rewrites the pool", func(t *testing.T) {
		fs := &storeLinkFakeStore{users: map[string]*models.User{uid: {ID: uid}}}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{
			"uuid": uid, "action": "suspend", "maxRoutes": 99,
		}, storeLinkTestKey))
		if len(fs.routeLimitSets) != 0 || len(fs.routeLimitDeletes) != 0 {
			t.Fatalf("sets=%+v deletes=%+v, want neither", fs.routeLimitSets, fs.routeLimitDeletes)
		}
	})

	t.Run("a failed pool write is reported, not swallowed", func(t *testing.T) {
		fs := &storeLinkFakeStore{
			users:         map[string]*models.User{uid: {ID: uid}},
			routeLimitErr: errStoreLinkEntitlement,
		}
		h := newStoreLinkHandler(fs, newStoreLinkRedis(t), true)
		rec := httptest.NewRecorder()
		h.Provision(rec, storeLinkPost("/api/store/provision", map[string]interface{}{
			"uuid": uid, "action": "activate", "maxRoutes": 12,
		}, storeLinkTestKey))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
	})
}
