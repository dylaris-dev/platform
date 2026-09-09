package store

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// ListUserPanelAssignments answers "who has been given something" for the Roles
// screen. Two things about that are easy to get wrong in a way nothing else
// notices, and both are about a row EXISTING versus a privilege existing.
func TestListUserPanelAssignments(t *testing.T) {
	tests := []struct {
		name      string
		roleID    interface{}
		overrides string
		want      bool
		why       string
	}{
		{
			name: "a panel role", roleID: int64(3), overrides: `{}`, want: true,
		},
		{
			name: "an override and no role", roleID: nil,
			overrides: `{"grant":["nodes.read"]}`, want: true,
			why: "they can do something a default user cannot, role or no role",
		},
		{
			name: "a deny override only", roleID: nil,
			overrides: `{"deny":["servers.delete"]}`, want: true,
			why: "a deny is a privilege decision, and the one nobody thinks to look for",
		},
		{
			// The reason the Go-side filter exists. The WHERE clause can only
			// ask whether the column is NULL, and the editor writes empty lists
			// rather than NULL when both are cleared - so this row still comes
			// back from the query and would list someone as holding something
			// after it was taken away.
			name: "overrides that were cleared, not removed", roleID: nil,
			overrides: `{"grant":[],"deny":[]}`, want: false,
			why: "an empty override set is not a privilege",
		},
		{
			name: "a NULL-ish empty object", roleID: nil, overrides: `{}`, want: false,
			why: "same, for a row that never held anything",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			s := NewPostgresStore(db)

			mock.ExpectQuery("FROM users").
				WillReturnRows(sqlmock.NewRows([]string{"id", "panel_role_id", "panel_cap_overrides"}).
					AddRow("u1", tt.roleID, []byte(tt.overrides)))

			out, err := s.ListUserPanelAssignments()
			if err != nil {
				t.Fatalf("ListUserPanelAssignments: %v", err)
			}
			if got := len(out) == 1; got != tt.want {
				t.Fatalf("listed = %v, want %v (%s)", got, tt.want, tt.why)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet sqlmock expectations: %v", err)
			}
		})
	}
}

// A role id must survive as a POINTER. Flattening it to 0 would make "no role"
// and "role 0" the same value, and the screen renders one of those as a badge.
func TestListUserPanelAssignments_RoleIDIsNullable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery("FROM users").
		WillReturnRows(sqlmock.NewRows([]string{"id", "panel_role_id", "panel_cap_overrides"}).
			AddRow("with-role", int64(4), []byte(`{}`)).
			AddRow("overrides-only", nil, []byte(`{"grant":["users.read"]}`)))

	out, err := s.ListUserPanelAssignments()
	if err != nil {
		t.Fatalf("ListUserPanelAssignments: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d rows, want 2", len(out))
	}
	if out[0].PanelRoleID == nil || *out[0].PanelRoleID != 4 {
		t.Errorf("role id = %v, want 4", out[0].PanelRoleID)
	}
	if out[1].PanelRoleID != nil {
		t.Errorf("role id = %v, want nil for an overrides-only row", *out[1].PanelRoleID)
	}
	if len(out[1].CapOverrides.Grant) != 1 {
		t.Errorf("grant caps = %v, want the one that was stored", out[1].CapOverrides.Grant)
	}
}

// An empty result is a list, not nil: the handler encodes it straight to JSON,
// and a nil slice becomes `null` where the panel expects `[]`.
func TestListUserPanelAssignments_EmptyIsAList(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery("FROM users").
		WillReturnRows(sqlmock.NewRows([]string{"id", "panel_role_id", "panel_cap_overrides"}))

	out, err := s.ListUserPanelAssignments()
	if err != nil {
		t.Fatalf("ListUserPanelAssignments: %v", err)
	}
	if out == nil {
		t.Fatal("returned nil, want an empty slice")
	}
	if len(out) != 0 {
		t.Fatalf("got %d rows, want 0", len(out))
	}
}
