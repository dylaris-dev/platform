package database

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/store"
	"sync"
	"testing"
)

func TestIntegrationModrinthWatchDeliveryAndIsolation(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	ctx := context.Background()
	u := &models.User{Username: uniqueName("watch_"), Password: "test", Email: uniqueName("watch_") + "@example.test"}
	other := &models.User{Username: uniqueName("other_"), Password: "test", Email: uniqueName("other_") + "@example.test"}
	for _, user := range []*models.User{u, other} {
		if err := st.CreateUser(user); err != nil {
			t.Fatal(err)
		}
	}
	favorite := models.ModrinthFavorite{ProjectID: "project", Title: "Public mod"}
	if err := st.SetModrinthFavorite(ctx, u.ID, favorite, false); err != nil {
		t.Fatal(err)
	}
	if rows, err := st.ListModrinthFavorites(ctx, other.ID); err != nil || len(rows) != 0 {
		t.Fatalf("favorite leaked: %v %v", rows, err)
	}
	w := models.ModrinthWatch{UserID: u.ID, Title: "My pack / 1.0", Loader: "forge", Current: "26.3", Future: true, Targets: []string{"1.21.11"}, Projects: []models.ModrinthWatchProject{{ProjectID: "project", Title: "Public mod"}}}
	if err := st.CreateModrinthWatch(ctx, &w); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListModrinthWatches(ctx, u.ID, false)
	if err != nil || len(rows) != 1 || rows[0].ID != w.ID || rows[0].UserID != u.ID || rows[0].NextCheck.IsZero() {
		t.Fatalf("watch metadata did not roundtrip: %+v %v", rows, err)
	}
	if err := st.DeleteModrinthWatch(ctx, w.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	ready := []models.ModrinthWatchVersion{{Minecraft: "1.21.11", Available: 1, Missing: []string{}}}
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := st.CompleteModrinthWatch(ctx, w, ready, ""); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	notices, err := st.ListNotifications(u.ID, true, 100)
	if err != nil || len(notices) != 1 {
		t.Fatalf("expected exactly one notification, got %d: %v", len(notices), err)
	}
	if err := st.MarkNotificationRead(notices[0].ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if notices, err = st.ListNotifications(u.ID, true, 100); err != nil || len(notices) != 1 {
		t.Fatal("reading removed the alert")
	}
	if err := st.DeleteNotification(notices[0].ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if notices, err = st.ListNotifications(u.ID, true, 100); err != nil || len(notices) != 1 {
		t.Fatal("another user removed the alert")
	}
	if err := st.DeleteNotification(notices[0].ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteModrinthWatch(ctx, w, ready, ""); err != nil {
		t.Fatal(err)
	}
	if notices, err = st.ListNotifications(u.ID, true, 100); err != nil || len(notices) != 0 {
		t.Fatal("deleted alert was re-created for an already delivered MC version")
	}
	rows, err = st.ListModrinthWatches(ctx, u.ID, false)
	if err != nil || len(rows) != 1 || len(rows[0].Delivered) != 1 {
		t.Fatal("future release watch did not persist")
	}
	if err := st.DeleteModrinthWatch(ctx, w.ID, u.ID); err != nil {
		t.Fatal(err)
	}
	w.ID = 0
	w.Future = false
	if err := st.CreateModrinthWatch(ctx, &w); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteModrinthWatch(ctx, w, ready, ""); err != nil {
		t.Fatal(err)
	}
	if rows, err = st.ListModrinthWatches(ctx, u.ID, false); err != nil || len(rows) != 0 {
		t.Fatal("fulfilled single-target job was not deleted")
	}
	if favorites, err := st.ListModrinthFavorites(ctx, u.ID); err != nil || len(favorites) != 1 {
		t.Fatal("completing a job removed the favorite")
	}
}
