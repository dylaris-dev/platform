package handlers

import (
	"context"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

type packEventStore struct{ store.Store }

func (packEventStore) GetPack(id int) (*models.Pack, error) {
	return &models.Pack{ID: id, OwnerID: "owner-1"}, nil
}

// Pack events carried only the pack or build id, so every signed-in session
// learned the ids of every tenant's packs and when each was edited. They now
// name the owner, and the stream hands them to the owner alone.
func TestPackEventsReachOnlyTheOwner(t *testing.T) {
	rdb := newServerPowerRedis(t)
	state := &AppState{Store: packEventStore{}, Redis: rdb, Events: services.NewSystemEventsPublisher(rdb)}
	sub := rdb.Subscribe(context.Background(), services.SystemEventsChannel)
	defer sub.Close()
	if _, err := sub.Receive(context.Background()); err != nil {
		t.Fatal(err)
	}

	publishPackEvent(httptest.NewRequest("POST", "/x", nil), state, "pack_builds.changed", 5, map[string]interface{}{"packId": 5})
	msg, err := sub.ReceiveMessage(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	h := &SystemEventsHandler{state: state}
	as := func(userID string) bool {
		r := httptest.NewRequest("GET", "/api/system/events", nil)
		ctx := context.WithValue(r.Context(), "userID", userID)
		ctx = context.WithValue(ctx, "isAdmin", false)
		return h.mayReceive(r.WithContext(ctx), msg.Payload)
	}
	if !as("owner-1") {
		t.Fatalf("the owner did not receive their own pack event: %s", msg.Payload)
	}
	if as("someone-else") {
		t.Fatalf("another tenant received the pack event: %s", msg.Payload)
	}
}
