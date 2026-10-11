package database

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/store"
)

// TestMapLegacyInviteCaps_PerBool asserts the EXACT granular cap list each of
// the eight capability bits maps to, plus that inherit maps to nothing (it is
// the invite.inherit column) and that no bit ever yields a backups.* cap (there
// was no legacy backups bit; migrated friends get no backups caps). The
// mapping itself lives in store.MapLegacyInviteCaps (moved there so the live
// member write path in store/postgres.go can reuse it); this catalog-validity
// assertion stays in the database package because it is the one place that
// already imports both store and authz without a cycle (authz imports store,
// so store itself cannot import authz).
func TestMapLegacyInviteCaps_PerBool(t *testing.T) {
	cases := []struct {
		name string
		perm models.TabPermissions
		want []string
	}{
		{"console", models.TabPermissions{Console: true}, []string{"console.read", "console.send"}},
		{"files", models.TabPermissions{Files: true}, []string{"files.read", "files.write", "files.delete"}},
		{"config", models.TabPermissions{Config: true}, []string{"config.read", "config.write", "mods.read", "mods.write", "mods.delete"}},
		{"power", models.TabPermissions{Power: true}, []string{"power.start", "power.stop", "power.restart", "power.kill", "rcon.exec", "players.read", "players.manage"}},
		{"network", models.TabPermissions{Network: true}, []string{"network.read", "network.write"}},
		{"overview", models.TabPermissions{Overview: true}, []string{"overview.read"}},
		{"members", models.TabPermissions{Members: true}, []string{"members.read", "members.write", "members.delete"}},
		{"setup", models.TabPermissions{Setup: true}, []string{"server.settings.write"}},
		{"players", models.TabPermissions{Players: true}, []string{"players.read", "players.manage"}},
		{"backups", models.TabPermissions{Backups: true}, []string{"backups.read", "backups.create", "backups.delete", "backups.restore"}},
		{"inherit-only maps to no caps", models.TabPermissions{Inherit: true}, []string{}},
		{"all false maps to no caps", models.TabPermissions{}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := store.MapLegacyInviteCaps(tc.perm)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mapLegacyInviteCaps = %v, want %v", got, tc.want)
			}
			for _, c := range got {
				if !tc.perm.Backups && strings.HasPrefix(c, "backups.") {
					t.Errorf("only the backups key may grant a backups cap, got %q", c)
				}
				cap, ok := authz.Get(c)
				if !ok {
					t.Errorf("mapped cap %q is not in the catalog", c)
				} else if cap.Scope != authz.ScopeServer {
					t.Errorf("mapped cap %q has scope %q, want server", c, cap.Scope)
				}
			}
		})
	}
}

// TestMapLegacyInviteCaps_FullBlob checks a fully-granted legacy invite maps to
// the union of all bit sets (order preserved, no duplicates), and never leaks a
// backups cap: a pre-migration blob had no players or backups bit.
func TestMapLegacyInviteCaps_FullBlob(t *testing.T) {
	full := models.TabPermissions{
		Console: true, Files: true, Config: true, Setup: true, Overview: true,
		Power: true, Members: true, Network: true, Inherit: true,
	}
	got := store.MapLegacyInviteCaps(full)
	want := []string{
		"console.read", "console.send",
		"files.read", "files.write", "files.delete",
		"config.read", "config.write", "mods.read", "mods.write", "mods.delete",
		"power.start", "power.stop", "power.restart", "power.kill", "rcon.exec", "players.read", "players.manage",
		"network.read", "network.write",
		"overview.read",
		"members.read", "members.write", "members.delete",
		"server.settings.write",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full-blob map = %v, want %v", got, want)
	}
}

// TestMigrateLegacyServerInvites_GuardAndUpdate pins the forward-only migration
// SQL: the guarded SELECT (server_id IS NOT NULL AND owner_user_id IS NULL AND
// cap_overrides = '{}') and the per-row UPDATE that copies inherit and derives
// owner_user_id from the server's owner via subquery.
func TestMigrateLegacyServerInvites_GuardAndUpdate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, COALESCE(permissions, '{}'::jsonb) FROM server_invites WHERE server_id IS NOT NULL AND owner_user_id IS NULL AND cap_overrides = '{}'::jsonb`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "permissions"}).
			AddRow(5, []byte(`{"console":true,"inherit":true}`)))

	mock.ExpectExec(regexp.QuoteMeta(
		`UPDATE server_invites SET cap_overrides = $1::jsonb, inherit = $2, owner_user_id = (SELECT owner_id FROM servers WHERE servers.id = server_invites.server_id) WHERE id = $3`)).
		WithArgs(sqlmock.AnyArg(), true, 5).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := migrateLegacyServerInvites(db); err != nil {
		t.Fatalf("migrateLegacyServerInvites: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
