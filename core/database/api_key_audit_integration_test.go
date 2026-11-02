package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// The key columns of both audit tables round-trip, and a bundled entry's count
// is set in place.
func TestIntegrationAuditRowsCarryTheAPIKey(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	keyID := 42

	ev := &models.ServerAuditEvent{
		ServerID: f.server.ID, EventType: "console.read", ActorUserID: &f.user.ID,
		ActorAPIKeyID: &keyID, ActorAPIKeyName: "deploy-bot", Metadata: map[string]interface{}{"count": 1},
	}
	if err := st.InsertServerAudit(ev); err != nil || ev.ID == 0 {
		t.Fatalf("InsertServerAudit: id %d, %v", ev.ID, err)
	}
	if err := st.SetServerAuditCount(ev.ID, 7); err != nil {
		t.Fatal(err)
	}
	plain := &models.ServerAuditEvent{ServerID: f.server.ID, EventType: "power_action", ActorUserID: &f.user.ID}
	if err := st.InsertServerAudit(plain); err != nil {
		t.Fatal(err)
	}
	rows, _, err := st.ListServerAudit(f.server.ID, "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	var keyed, unkeyed *models.ServerAuditEvent
	for i := range rows {
		switch rows[i].ID {
		case ev.ID:
			keyed = &rows[i]
		case plain.ID:
			unkeyed = &rows[i]
		}
	}
	if keyed == nil || keyed.ActorAPIKeyID == nil || *keyed.ActorAPIKeyID != 42 || keyed.ActorAPIKeyName != "deploy-bot" ||
		keyed.Metadata["count"] != float64(7) || keyed.ActorName != f.user.Username {
		t.Fatalf("keyed row %+v", keyed)
	}
	if unkeyed == nil || unkeyed.ActorAPIKeyID != nil || unkeyed.ActorAPIKeyName != "" {
		t.Fatalf("row without a key %+v", unkeyed)
	}

	id := &models.AuditEventIdentity{
		EventType: "api_key_read", ActorUserID: &f.user.ID, TargetUserID: &f.user.ID,
		ActorAPIKeyID: &keyID, ActorAPIKeyName: "deploy-bot", Metadata: map[string]interface{}{"count": 1},
	}
	if err := st.InsertAuditIdentity(id); err != nil || id.ID == 0 {
		t.Fatalf("InsertAuditIdentity: id %d, %v", id.ID, err)
	}
	if err := st.SetAuditIdentityCount(id.ID, 3); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListAuditIdentity(&f.user.ID, "api_key_read", 10)
	if err != nil || len(got) != 1 || got[0].ActorAPIKeyName != "deploy-bot" || got[0].Metadata["count"] != float64(3) {
		t.Fatalf("identity rows %+v (%v)", got, err)
	}
}

// G-4: an anonymized account kept live keys, and an account that only
// automates read as inactive.
func TestIntegrationAnonymizeRevokesKeysAndKeyUseIsActivity(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	if _, err := db.Exec(`UPDATE users SET last_login_at = NOW() - interval '400 days', created_at = NOW() - interval '400 days' WHERE id = $1`, f.user.ID); err != nil {
		t.Fatal(err)
	}
	keyID, err := st.CreateAPIKey(&models.APIKey{UserID: f.user.ID, Name: "bot", KeyHash: uniqueName("h_"), RatePerMin: 60})
	if err != nil {
		t.Fatal(err)
	}
	idleSince := time.Now().Add(-365 * 24 * time.Hour)
	isCandidate := func() bool {
		cands, err := st.ListInactiveCandidates(idleSince)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cands {
			if c.ID == f.user.ID {
				return true
			}
		}
		return false
	}
	if !isCandidate() {
		t.Fatal("an idle account with an unused key is not a candidate")
	}
	if err := st.TouchAPIKey(keyID); err != nil {
		t.Fatal(err)
	}
	if isCandidate() {
		t.Fatal("an account whose key was just used is still a candidate for auto-delete")
	}

	if err := st.AnonymizeUser(f.user.ID); err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListAPIKeysByUser(f.user.ID)
	if err != nil || len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Fatalf("keys after anonymize %+v (%v), want the key revoked", keys, err)
	}
}
