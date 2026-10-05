package store

import (
	"database/sql/driver"
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// capOverridesGrant is a sqlmock argument matcher for a jsonb cap_overrides
// column value: it unmarshals the bound arg as CapOverrides and asserts Grant
// is EXACTLY the given set (order-independent, no extras, no omissions).
// Used to pin that CreateInvite/UpdateInvitePermissions derive cap_overrides
// from the SAME permissions blob via MapLegacyInviteCaps, so the resolver
// (which reads cap_overrides, not the legacy blob) sees the write immediately.
type capOverridesGrant struct {
	want []string
}

func (m capOverridesGrant) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	var ov CapOverrides
	if err := json.Unmarshal([]byte(s), &ov); err != nil {
		return false
	}
	if len(ov.Grant) != len(m.want) {
		return false
	}
	got := make(map[string]bool, len(ov.Grant))
	for _, c := range ov.Grant {
		got[c] = true
	}
	for _, c := range m.want {
		if !got[c] {
			return false
		}
	}
	return true
}

// createInviteSQL matches the multi-line CreateInvite INSERT with \s+ standing
// in for the literal indentation whitespace, so the test does not depend on
// the exact tab layout of the source (regexp.QuoteMeta would require a
// byte-exact copy of that whitespace).
const createInviteSQL = `INSERT INTO server_invites \(server_id, user_id, invited_by, permissions, cap_overrides, inherit, owner_user_id\)\s+VALUES \(\$1, \$2, \$3, \$4::jsonb, \$5::jsonb, \$6, \(SELECT owner_id FROM servers WHERE id = \$1\)\)`

const updateInvitePermissionsSQL = `UPDATE server_invites SET permissions = \$1::jsonb,\s+cap_overrides = jsonb_set\(\$2::jsonb, '\{deny\}', COALESCE\(server_invites\.cap_overrides->'deny', '\[\]'::jsonb\)\),\s+inherit = \$3,\s+owner_user_id = \(SELECT owner_id FROM servers WHERE id = \$4\)\s+WHERE server_id = \$4 AND user_id = \$5`

// TestCreateInvite_WritesCapOverrides pins F-2: a brand-new invite must carry
// the mapped caps in cap_overrides immediately (no restart needed for the
// resolver to see them). power:true must map to the power.* + rcon.exec +
// players.* set, matching MapLegacyInviteCaps.
func TestCreateInvite_WritesCapOverrides(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	serverID := 42
	userID := "22222222-2222-2222-2222-222222222222"
	invitedBy := "11111111-1111-1111-1111-111111111111"
	perms := map[string]bool{"power": true}

	wantCaps := []string{"power.start", "power.stop", "power.restart", "power.kill", "rcon.exec", "players.read", "players.manage"}
	mock.ExpectExec(createInviteSQL).
		WithArgs(serverID, userID, invitedBy, sqlmock.AnyArg(), capOverridesGrant{want: wantCaps}, false).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := s.CreateInvite(serverID, userID, invitedBy, perms); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestCreateInvite_NoCapsWhenPermissionsAllFalse pins that an invite with no
// bits set writes an EMPTY cap_overrides.Grant (not the mapped set of some
// other bit), i.e. a bare invite grants nothing until permissions are set.
func TestCreateInvite_NoCapsWhenPermissionsAllFalse(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	serverID := 42
	userID := "22222222-2222-2222-2222-222222222222"
	invitedBy := "11111111-1111-1111-1111-111111111111"
	perms := map[string]bool{"power": false, "console": false}

	mock.ExpectExec(createInviteSQL).
		WithArgs(serverID, userID, invitedBy, sqlmock.AnyArg(), capOverridesGrant{want: []string{}}, false).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := s.CreateInvite(serverID, userID, invitedBy, perms); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestUpdateInvitePermissions_WritesCapOverrides pins F-1 (the grant side): an
// edit that adds a bit must recompute cap_overrides from the NEW blob, and
// inherit must follow the blob's inherit bit too.
func TestUpdateInvitePermissions_WritesCapOverrides(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	serverID := 42
	userID := "22222222-2222-2222-2222-222222222222"
	perms := map[string]bool{"files": true, "inherit": true}

	wantCaps := []string{"files.read", "files.write", "files.delete"}
	mock.ExpectExec(updateInvitePermissionsSQL).
		WithArgs(sqlmock.AnyArg(), capOverridesGrant{want: wantCaps}, true, serverID, userID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.UpdateInvitePermissions(serverID, userID, perms); err != nil {
		t.Fatalf("UpdateInvitePermissions: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestUpdateInvitePermissions_RevokeClearsCapOverrides pins F-1 (the revoke
// side, the security-relevant half): editing an invite down to no bits set
// must write an EMPTY cap_overrides.Grant, so the resolver stops granting the
// previously-held caps immediately. Before this fix, UpdateInvitePermissions
// touched only the legacy permissions blob and cap_overrides silently kept
// the OLD (already-migrated) grant forever - a revoke that never took effect.
func TestUpdateInvitePermissions_RevokeClearsCapOverrides(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	serverID := 42
	userID := "22222222-2222-2222-2222-222222222222"
	perms := map[string]bool{"power": false, "files": false, "console": false}

	mock.ExpectExec(updateInvitePermissionsSQL).
		WithArgs(sqlmock.AnyArg(), capOverridesGrant{want: []string{}}, false, serverID, userID).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.UpdateInvitePermissions(serverID, userID, perms); err != nil {
		t.Fatalf("UpdateInvitePermissions: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
