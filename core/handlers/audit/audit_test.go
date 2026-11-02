package audit

import (
	"context"
	"net/http/httptest"
	"testing"

	"dylaris-core/handlers/reqctx"
	"dylaris-core/models"
	"dylaris-core/store"
)

type auditStore struct {
	store.Store
	enabled  bool
	server   []*models.ServerAuditEvent
	identity []*models.AuditEventIdentity
}

func (s *auditStore) GetServerAuditState(int) (bool, bool, int, error) {
	return s.enabled, false, 0, nil
}
func (s *auditStore) SetServerAuditEnabled(_ int, on bool) error { s.enabled = on; return nil }
func (s *auditStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	ev.ID = int64(len(s.server) + 1)
	s.server = append(s.server, ev)
	return nil
}
func (s *auditStore) InsertAuditIdentity(ev *models.AuditEventIdentity) error {
	ev.ID = int64(len(s.identity) + 1)
	s.identity = append(s.identity, ev)
	return nil
}

// The writers took *AppState before they moved here and now take the store.
// The API-key actor fields come from the request context through reqctx, so
// a key request keeps being recorded with the key beside its owner, and on a
// server with auditing off it switches auditing on; a panel request there
// stays unrecorded. No store writes nothing and does not panic.
func TestTheWritersRecordTheKeyAfterTheMove(t *testing.T) {
	key := &models.APIKey{ID: 42, Name: "ci"}
	panel := httptest.NewRequest("POST", "/", nil)
	keyed := panel.WithContext(reqctx.WithAPIKey(context.Background(), key, true))

	st := &auditStore{}
	LogServer(st, panel, 7, ServerAuditEventSetup, "u1", "", nil)
	if len(st.server) != 0 || st.enabled {
		t.Fatal("a panel action on a server with auditing off was recorded")
	}
	LogServer(st, keyed, 7, ServerAuditEventSetup, "u1", "", nil)
	if len(st.server) != 1 || !st.enabled {
		t.Fatalf("key action: %d rows, auditing on %v", len(st.server), st.enabled)
	}
	if ev := st.server[0]; ev.ActorAPIKeyID == nil || *ev.ActorAPIKeyID != 42 || ev.ActorAPIKeyName != "ci" || ev.ActorUserID == nil || *ev.ActorUserID != "u1" {
		t.Fatalf("server row lost the key or the owner: %+v", ev)
	}

	LogIdentity(st, keyed, AuditEventAPIKeyUsed, "u1", "u1", nil)
	LogIdentity(st, nil, AuditEvent2FABackupCodeConsumed, "u1", "u1", nil)
	if len(st.identity) != 2 || st.identity[0].ActorAPIKeyID == nil || st.identity[1].ActorAPIKeyID != nil {
		t.Fatalf("identity rows: %+v", st.identity)
	}

	if LogServerAs(nil, keyed, key, 7, ServerAuditEventSetup, "u1", "", nil) != 0 || LogIdentityAs(nil, keyed, key, AuditEventAPIKeyUsed, "u1", "u1", nil) != 0 {
		t.Fatal("no store wrote something")
	}
	EnableServerAuditIfNeeded(nil, 7)
}
