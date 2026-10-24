package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// PackCollaborator is one owner's invite of one user, to every pack (PackID
// nil) or to one.
type PackCollaborator struct {
	ID        int       `json:"id"`
	OwnerID   string    `json:"ownerId"`
	OwnerName string    `json:"ownerName,omitempty"`
	UserID    string    `json:"userId"`
	Username  string    `json:"username,omitempty"`
	PackID    *int      `json:"packId"`
	PackName  string    `json:"packName,omitempty"`
	CanCreate bool      `json:"canCreate"`
	CanEdit   bool      `json:"canEdit"`
	CanDelete bool      `json:"canDelete"`
	CreatedAt time.Time `json:"createdAt"`
}

// PackRights is what a collaborator may do on one pack. Found false = no
// invite covers it (not even read).
type PackRights struct {
	Found     bool
	CanCreate bool
	CanEdit   bool
	CanDelete bool
}

// ErrPackCollaboratorNotFound: no such invite for this owner.
var ErrPackCollaboratorNotFound = errors.New("pack collaborator not found")

const packCollabCols = `c.id, c.owner_id, COALESCE(o.username,''), c.user_id, COALESCE(u.username,''),
	c.pack_id, COALESCE(p.internal_name,''), c.can_create, c.can_edit, c.can_delete, c.created_at`
const packCollabFrom = ` FROM pack_collaborators c
	LEFT JOIN users o ON o.id = c.owner_id
	LEFT JOIN users u ON u.id = c.user_id
	LEFT JOIN packs p ON p.id = c.pack_id`

func scanPackCollab(rows interface{ Scan(...any) error }) (PackCollaborator, error) {
	var c PackCollaborator
	var pid sql.NullInt64
	err := rows.Scan(&c.ID, &c.OwnerID, &c.OwnerName, &c.UserID, &c.Username, &pid, &c.PackName,
		&c.CanCreate, &c.CanEdit, &c.CanDelete, &c.CreatedAt)
	if pid.Valid {
		v := int(pid.Int64)
		c.PackID = &v
	}
	return c, err
}

func (s *PostgresStore) listPackCollabs(where string, arg string) ([]PackCollaborator, error) {
	rows, err := s.db.Query(`SELECT `+packCollabCols+packCollabFrom+` WHERE `+where+` ORDER BY c.created_at`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PackCollaborator{}
	for rows.Next() {
		c, err := scanPackCollab(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListPackCollaboratorsByOwner: the invites an owner handed out.
func (s *PostgresStore) ListPackCollaboratorsByOwner(ownerID string) ([]PackCollaborator, error) {
	return s.listPackCollabs(`c.owner_id = $1`, ownerID)
}

// ListPackCollaborationsForUser: the invites a user holds.
func (s *PostgresStore) ListPackCollaborationsForUser(userID string) ([]PackCollaborator, error) {
	return s.listPackCollabs(`c.user_id = $1`, userID)
}

// PackRightsFor merges the owner-wide invite and the one for packID (packID 0
// = only the owner-wide one, for "create a pack on their account").
func (s *PostgresStore) PackRightsFor(ownerID, userID string, packID int) (PackRights, error) {
	var r PackRights
	rows, err := s.db.Query(`SELECT can_create, can_edit, can_delete FROM pack_collaborators
		WHERE owner_id = $1 AND user_id = $2 AND (pack_id IS NULL OR pack_id = $3)`, ownerID, userID, packID)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	for rows.Next() {
		var c, e, d bool
		if err := rows.Scan(&c, &e, &d); err != nil {
			return r, err
		}
		r.Found = true
		r.CanCreate = r.CanCreate || c
		r.CanEdit = r.CanEdit || e
		r.CanDelete = r.CanDelete || d
	}
	return r, rows.Err()
}

// UpsertPackCollaborator creates or replaces the invite for (owner, user,
// pack) and returns its id.
func (s *PostgresStore) UpsertPackCollaborator(c PackCollaborator, createdBy string) (int, error) {
	var pid any
	if c.PackID != nil {
		pid = *c.PackID
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM pack_collaborators WHERE owner_id=$1 AND user_id=$2 AND pack_id IS NOT DISTINCT FROM $3`,
		c.OwnerID, c.UserID, pid); err != nil {
		return 0, err
	}
	var id int
	if err := tx.QueryRow(`INSERT INTO pack_collaborators (owner_id, user_id, pack_id, can_create, can_edit, can_delete, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		c.OwnerID, c.UserID, pid, c.CanCreate && c.PackID == nil, c.CanEdit, c.CanDelete, createdBy).Scan(&id); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// UpdatePackCollaboratorRights changes the rights of one of the owner's invites.
func (s *PostgresStore) UpdatePackCollaboratorRights(ownerID string, id int, canCreate, canEdit, canDelete bool) error {
	res, err := s.db.Exec(`UPDATE pack_collaborators SET can_create = $3 AND pack_id IS NULL, can_edit = $4, can_delete = $5
		WHERE id = $1 AND owner_id = $2`, id, ownerID, canCreate, canEdit, canDelete)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPackCollaboratorNotFound
	}
	return nil
}

// GetPackCollaborator returns one of the owner's invites.
func (s *PostgresStore) GetPackCollaborator(ownerID string, id int) (*PackCollaborator, error) {
	c, err := scanPackCollab(s.db.QueryRow(`SELECT `+packCollabCols+packCollabFrom+` WHERE c.id = $1 AND c.owner_id = $2`, id, ownerID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPackCollaboratorNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// DeletePackCollaborator removes one of the owner's invites.
func (s *PostgresStore) DeletePackCollaborator(ownerID string, id int) error {
	res, err := s.db.Exec(`DELETE FROM pack_collaborators WHERE id = $1 AND owner_id = $2`, id, ownerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPackCollaboratorNotFound
	}
	return nil
}

// PackAuditEvent is one change to an owner's pack.
type PackAuditEvent struct {
	ID        int64           `json:"id"`
	PackID    *int            `json:"packId"`
	PackName  string          `json:"packName"`
	ActorID   string          `json:"actorId"`
	ActorName string          `json:"actorName"`
	Action    string          `json:"action"`
	Detail    json.RawMessage `json:"detail"`
	CreatedAt time.Time       `json:"createdAt"`
}

// AddPackAudit records one change.
func (s *PostgresStore) AddPackAudit(ownerID string, packID int, packName, actorID, action string, detail map[string]any) error {
	b, _ := json.Marshal(detail)
	if detail == nil {
		b = []byte("{}")
	}
	var pid, actor any
	if packID > 0 {
		pid = packID
	}
	if actorID != "" {
		actor = actorID
	}
	_, err := s.db.Exec(`INSERT INTO pack_audit_events (owner_id, pack_id, pack_name, actor_user_id, action, detail)
		VALUES ($1,$2,$3,$4,$5,$6)`, ownerID, pid, packName, actor, action, string(b))
	return err
}

// ListPackAudit returns the newest events of an owner's packs, one pack's
// (packID > 0) or all of them, at most limit.
func (s *PostgresStore) ListPackAudit(ownerID string, packID, limit int) ([]PackAuditEvent, error) {
	rows, err := s.db.Query(`SELECT e.id, e.pack_id, e.pack_name, COALESCE(e.actor_user_id::text,''), COALESCE(u.username,''),
			e.action, e.detail, e.created_at
		FROM pack_audit_events e LEFT JOIN users u ON u.id = e.actor_user_id
		WHERE e.owner_id = $1 AND ($2 = 0 OR e.pack_id = $2)
		ORDER BY e.created_at DESC, e.id DESC LIMIT $3`, ownerID, packID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PackAuditEvent{}
	for rows.Next() {
		var e PackAuditEvent
		var pid sql.NullInt64
		var detail []byte
		if err := rows.Scan(&e.ID, &pid, &e.PackName, &e.ActorID, &e.ActorName, &e.Action, &detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		if pid.Valid {
			v := int(pid.Int64)
			e.PackID = &v
		}
		e.Detail = detail
		out = append(out, e)
	}
	return out, rows.Err()
}
