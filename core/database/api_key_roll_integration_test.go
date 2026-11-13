package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// The two roll columns round-trip through the real store: the new hash wins,
// the previous one is accepted only inside its grace and only while the key is
// live, and a second roll keeps just one old hash.
func TestIntegrationAPIKeyRollColumns(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	ctx := context.Background()

	h0, h1, h2 := uniqueName("h0_"), uniqueName("h1_"), uniqueName("h2_")
	id, err := st.CreateAPIKey(&models.APIKey{UserID: f.user.ID, Name: "bot", KeyHash: h0, RatePerMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RollAPIKey(ctx, id, f.user.ID, h0, h1, time.Hour); err != nil {
		t.Fatal(err)
	}
	cur, err := st.GetAPIKeyByHash(h1)
	if err != nil || cur.ID != id || cur.ViaPrevious {
		t.Fatalf("new hash: %+v %v", cur, err)
	}
	old, err := st.GetAPIKeyByHash(h0)
	if err != nil || old.ID != id || !old.ViaPrevious {
		t.Fatalf("previous hash inside grace: %+v %v", old, err)
	}

	if err := st.RollAPIKey(ctx, id, f.user.ID, h1, h2, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAPIKeyByHash(h0); err == nil {
		t.Fatal("the key before the previous one is still accepted")
	}
	if k, err := st.GetAPIKeyByHash(h1); err != nil || !k.ViaPrevious {
		t.Fatalf("previous after second roll: %+v %v", k, err)
	}

	// Grace over.
	if _, err := db.Exec(`UPDATE api_keys SET prev_key_expires_at = NOW() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAPIKeyByHash(h1); err == nil {
		t.Fatal("previous hash accepted after its grace")
	}
	if k, err := st.GetAPIKeyByHash(h2); err != nil || k.ViaPrevious {
		t.Fatalf("current hash after grace: %+v %v", k, err)
	}

	// A stale expected hash or a foreign owner changes nothing.
	if err := st.RollAPIKey(ctx, id, f.user.ID, h1, uniqueName("x_"), time.Hour); !errors.Is(err, store.ErrAPIKeyRollConflict) {
		t.Fatalf("stale hash: %v, want conflict", err)
	}
	if err := st.RollAPIKey(ctx, id, "00000000-0000-0000-0000-000000000000", h2, uniqueName("x_"), time.Hour); !errors.Is(err, store.ErrAPIKeyRollConflict) {
		t.Fatalf("foreign owner: %v, want conflict", err)
	}
	if k, err := st.GetAPIKeyByHash(h2); err != nil || k.ViaPrevious || k.ID != id {
		t.Fatalf("rejected rolls changed the key: %+v %v", k, err)
	}

	// A revoked or expired key's previous hash is dead too, and it cannot be rolled.
	h3 := uniqueName("h3_")
	if err := st.RollAPIKey(ctx, id, f.user.ID, h2, h3, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeAPIKey(id, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetAPIKeyByHash(h2); err == nil {
		t.Fatal("previous hash of a revoked key accepted")
	}
	if err := st.RollAPIKey(ctx, id, f.user.ID, h3, uniqueName("h4_"), time.Hour); err == nil {
		t.Fatal("a revoked key was rolled")
	}

	e0 := uniqueName("e0_")
	id2, err := st.CreateAPIKey(&models.APIKey{UserID: f.user.ID, Name: "bot2", KeyHash: e0, RatePerMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	e1 := uniqueName("e1_")
	if err := st.RollAPIKey(ctx, id2, f.user.ID, e0, e1, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE api_keys SET expires_at = NOW() - interval '1 second' WHERE id = $1`, id2); err != nil {
		t.Fatal(err)
	}
	var prev string
	if err := db.QueryRow(`SELECT prev_key_hash FROM api_keys WHERE id = $1`, id2).Scan(&prev); err != nil || prev == "" {
		t.Fatalf("prev_key_hash column: %q %v", prev, err)
	}
	if _, err := st.GetAPIKeyByHash(prev); err == nil {
		t.Fatal("previous hash of an expired key accepted")
	}
}

// The mint answer shows the key as stored, so its createdAt is the row's, not
// the zero time.
func TestIntegrationCreateAPIKeyReturnsCreatedAt(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	k := &models.APIKey{UserID: f.user.ID, Name: "created", KeyHash: uniqueName("hc_"), RatePerMin: 60}
	id, err := st.CreateAPIKey(k)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetAPIKeyByHash(k.KeyHash)
	if err != nil || stored.ID != id {
		t.Fatalf("stored: %+v %v", stored, err)
	}
	if k.CreatedAt.IsZero() || !k.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("createdAt = %v, stored %v", k.CreatedAt, stored.CreatedAt)
	}
}
