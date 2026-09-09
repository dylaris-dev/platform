package store

import (
	"database/sql"
	"encoding/json"
	"time"
)

// CapOverrides is the grant/deny override set stored as JSONB on
// users.panel_cap_overrides and server_invites.cap_overrides. Grant adds caps
// on top of a role; Deny removes caps a role would otherwise give.
type CapOverrides struct {
	Grant []string `json:"grant"`
	Deny  []string `json:"deny"`
}

// PanelRole is a named bundle of PANEL-scope capabilities (level-1 staff role).
// The system 'admin' role short-circuits and is not stored as an assignable
// capability list; is_system marks non-editable seeds (phase 3).
type PanelRole struct {
	ID           int
	Name         string
	Capabilities []string
	IsSystem     bool
	CreatedBy    *string
	CreatedAt    time.Time
}

// ServerRole is an owner-scoped, reusable bundle of SERVER + OWNER caps that an
// owner assigns to invited friends (phase 4).
type ServerRole struct {
	ID           int
	OwnerUserID  string
	Name         string
	Capabilities []string
	CreatedAt    time.Time
}

// ServerGrant is the reworked server_invites row read by the resolver. ServerID
// nil means an account-wide grant (all the owner's servers + owner tools).
// ServerRoleID nil means "overrides only" (no role assigned).
type ServerGrant struct {
	ServerID     *int
	UserID       string
	OwnerUserID  string
	ServerRoleID *int
	CapOverrides CapOverrides
	Inherit      bool
}

// GetPanelRole returns the panel role by id. sql.ErrNoRows when absent.
func (s *PostgresStore) GetPanelRole(id int) (*PanelRole, error) {
	var pr PanelRole
	var capsJSON []byte
	var createdBy sql.NullString
	err := s.db.QueryRow(
		`SELECT id, name, COALESCE(capabilities, '[]'::jsonb), is_system, created_by, created_at
		 FROM panel_roles WHERE id = $1`, id).
		Scan(&pr.ID, &pr.Name, &capsJSON, &pr.IsSystem, &createdBy, &pr.CreatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(capsJSON, &pr.Capabilities)
	if createdBy.Valid {
		pr.CreatedBy = &createdBy.String
	}
	return &pr, nil
}

// GetServerRole returns the owner-scoped server-role by id.
func (s *PostgresStore) GetServerRole(id int) (*ServerRole, error) {
	var sr ServerRole
	var capsJSON []byte
	err := s.db.QueryRow(
		`SELECT id, owner_user_id, name, COALESCE(capabilities, '[]'::jsonb), created_at
		 FROM server_roles WHERE id = $1`, id).
		Scan(&sr.ID, &sr.OwnerUserID, &sr.Name, &capsJSON, &sr.CreatedAt)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(capsJSON, &sr.Capabilities)
	return &sr, nil
}

// GetUserPanelAuthz returns the user's panel_role_id (nil when NULL) and their
// per-user panel cap overrides. A missing user row returns the query error.
func (s *PostgresStore) GetUserPanelAuthz(userID string) (*int, CapOverrides, error) {
	var roleID sql.NullInt64
	var ovJSON []byte
	err := s.db.QueryRow(
		`SELECT panel_role_id, COALESCE(panel_cap_overrides, '{}'::jsonb)
		 FROM users WHERE id = $1`, userID).
		Scan(&roleID, &ovJSON)
	if err != nil {
		return nil, CapOverrides{}, err
	}
	var ov CapOverrides
	_ = json.Unmarshal(ovJSON, &ov)
	if roleID.Valid {
		r := int(roleID.Int64)
		return &r, ov, nil
	}
	return nil, ov, nil
}

// PanelAssignment is one user's level-1 panel authz, for the assignments list.
//
// It carries no user detail on purpose - no name, no email, not even the legacy
// role. The screen that reads it already holds the user list it is allowed to
// see, and joins on the id. Returning user data here would make a
// panelroles.read endpoint a second way to enumerate accounts, which is exactly
// what putting this under panelroles.read instead of on /users was avoiding.
type PanelAssignment struct {
	UserID       string
	PanelRoleID  *int
	CapOverrides CapOverrides
}

// ListUserPanelAssignments returns every user who holds a panel role or a
// per-user capability override.
//
// Users with neither are left out rather than returned empty: the caller is
// asking who has been GIVEN something, and one row per account would make the
// answer a copy of the user table that has to be filtered again on the other
// side. It replaces N calls to GetUserPanelAuthz, which is what the panel did
// when it needed this for more than one person at a time - it opened the editor
// per user, so it could not show the state of anybody it had not clicked.
func (s *PostgresStore) ListUserPanelAssignments() ([]PanelAssignment, error) {
	rows, err := s.db.Query(
		`SELECT id, panel_role_id, COALESCE(panel_cap_overrides, '{}'::jsonb)
		 FROM users
		 WHERE panel_role_id IS NOT NULL OR panel_cap_overrides IS NOT NULL
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PanelAssignment{}
	for rows.Next() {
		var a PanelAssignment
		var roleID sql.NullInt64
		var ovJSON []byte
		if err := rows.Scan(&a.UserID, &roleID, &ovJSON); err != nil {
			return nil, err
		}
		if roleID.Valid {
			r := int(roleID.Int64)
			a.PanelRoleID = &r
		}
		_ = json.Unmarshal(ovJSON, &a.CapOverrides)
		// The WHERE clause can only test whether the column is NULL, and
		// clearing both lists in the editor writes {"grant":[],"deny":[]} rather
		// than NULL. So a user whose overrides were removed still matches the
		// query and has to be dropped here, or they would be listed as holding
		// something forever after having it taken away.
		if a.PanelRoleID == nil && len(a.CapOverrides.Grant) == 0 && len(a.CapOverrides.Deny) == 0 {
			continue
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetServerGrant returns the reworked invite for (server_id, user_id).
// sql.ErrNoRows when the user has no direct grant on that server.
func (s *PostgresStore) GetServerGrant(serverID int, userID string) (*ServerGrant, error) {
	return s.scanGrant(
		`SELECT server_id, user_id, owner_user_id, server_role_id,
		        COALESCE(cap_overrides, '{}'::jsonb), COALESCE(inherit, FALSE)
		 FROM server_invites WHERE server_id = $1 AND user_id = $2`, serverID, userID)
}

// GetAccountGrant returns the account-wide grant (server_id IS NULL) for a
// friend on a specific owner's realm. sql.ErrNoRows when none exists.
func (s *PostgresStore) GetAccountGrant(ownerUserID, userID string) (*ServerGrant, error) {
	return s.scanGrant(
		`SELECT server_id, user_id, owner_user_id, server_role_id,
		        COALESCE(cap_overrides, '{}'::jsonb), COALESCE(inherit, FALSE)
		 FROM server_invites WHERE server_id IS NULL AND owner_user_id = $1 AND user_id = $2`,
		ownerUserID, userID)
}

// OwnerGrant is a joined server_invites row for the owner-facing grants list
// (GET /api/grants). ServerID nil = account-wide; ServerRoleName / ServerName
// are "" when there is no role / for account-wide rows.
type OwnerGrant struct {
	Username       string
	ServerID       *int
	ServerName     string
	ServerRoleID   *int
	ServerRoleName string
	CapOverrides   CapOverrides
	Inherit        bool
}

// ListGrantsByOwner returns every grant in an owner's realm (account-wide
// server_id NULL + per-server), joined to the friend's username and (LEFT
// JOIN) the server name + server-role name. Ordered by username for a stable UI.
func (s *PostgresStore) ListGrantsByOwner(ownerUserID string) ([]OwnerGrant, error) {
	rows, err := s.db.Query(
		`SELECT u.username, si.server_id, COALESCE(sv.name, ''), si.server_role_id,
		        COALESCE(sr.name, ''), COALESCE(si.cap_overrides, '{}'::jsonb),
		        COALESCE(si.inherit, FALSE)
		 FROM server_invites si
		 JOIN users u ON si.user_id = u.id
		 LEFT JOIN servers sv ON si.server_id = sv.id
		 LEFT JOIN server_roles sr ON si.server_role_id = sr.id
		 WHERE si.owner_user_id = $1
		 ORDER BY u.username ASC`, ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OwnerGrant
	for rows.Next() {
		var g OwnerGrant
		var sid, roleID sql.NullInt64
		var ovJSON []byte
		if err := rows.Scan(&g.Username, &sid, &g.ServerName, &roleID, &g.ServerRoleName, &ovJSON, &g.Inherit); err != nil {
			return nil, err
		}
		if sid.Valid {
			v := int(sid.Int64)
			g.ServerID = &v
		}
		if roleID.Valid {
			v := int(roleID.Int64)
			g.ServerRoleID = &v
		}
		_ = json.Unmarshal(ovJSON, &g.CapOverrides)
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *PostgresStore) scanGrant(query string, args ...interface{}) (*ServerGrant, error) {
	var g ServerGrant
	var sid sql.NullInt64
	var ownerUID sql.NullString
	var roleID sql.NullInt64
	var ovJSON []byte
	err := s.db.QueryRow(query, args...).Scan(&sid, &g.UserID, &ownerUID, &roleID, &ovJSON, &g.Inherit)
	if err != nil {
		return nil, err
	}
	if sid.Valid {
		v := int(sid.Int64)
		g.ServerID = &v
	}
	if ownerUID.Valid {
		g.OwnerUserID = ownerUID.String
	}
	if roleID.Valid {
		v := int(roleID.Int64)
		g.ServerRoleID = &v
	}
	_ = json.Unmarshal(ovJSON, &g.CapOverrides)
	return &g, nil
}
