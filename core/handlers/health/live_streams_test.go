package health

import (
	"testing"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"
)

type countingScopeStore struct {
	eventScopeFakeStore
	lookups int
}

func (c *countingScopeStore) GetServerByID(id int) (*models.Server, error) {
	c.lookups++
	return c.eventScopeFakeStore.GetServerByID(id)
}

// One stream resolves a server once, not once per event: a modpack install
// publishes an event per mod to every open panel, each costing several queries.
func TestSystemEventsResolveOncePerStream(t *testing.T) {
	st := &countingScopeStore{eventScopeFakeStore: eventScopeFakeStore{server: &models.Server{ID: 7, UUID: "srv", OwnerID: "owner-1"}}}
	h := &SystemEventsHandler{d: Deps{Authz: authz.NewResolver(st)}}
	visible := map[int]cachedVisibility{}
	for range 50 {
		if !h.mayReceiveCached(eventScopeRequest("owner-1", false), `{"type":"server_mods.changed","payload":{"serverId":7}}`, visible) {
			t.Fatal("the owner lost an event")
		}
		if h.mayReceiveCached(eventScopeRequest("owner-1", false), `{"type":"x","payload":{"serverId":7}}`, visible) != true {
			t.Fatal("cached answer changed")
		}
	}
	if st.lookups == 0 || st.lookups > 3 {
		t.Fatalf("%d server lookups for 100 events on one server", st.lookups)
	}
	// An expired answer is asked again, so an invite shows within the TTL.
	visible[7] = cachedVisibility{visible: false, until: time.Now().Add(-time.Second)}
	if !h.mayReceiveCached(eventScopeRequest("owner-1", false), `{"type":"x","payload":{"serverId":7}}`, visible) {
		t.Fatal("an expired cached answer was still used")
	}
}
