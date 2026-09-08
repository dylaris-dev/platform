package store

import (
	"database/sql"
	"dylaris-core/models"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ==========================================
// TICKETS
// ==========================================
// All implementations live here so postgres.go stays at a manageable
// size. The patterns mirror the older code: COALESCE on nullable columns at
// SELECT time, NULLIF on empty strings at INSERT time, JSONB round-tripped
// via []byte + json.Marshal/Unmarshal.

// ── Categories ────────────────────────────────────────────────────────

func scanTicketCategory(scan func(dest ...interface{}) error) (*models.TicketCategory, error) {
	var (
		c        models.TicketCategory
		team     sql.NullString
		color    sql.NullString
		createAt sql.NullTime
	)
	err := scan(&c.ID, &c.Name, &c.Description, &c.RequiresServer, &c.DefaultPriority,
		&team, &color, &c.Enabled, &c.Position, &createAt)
	if err != nil {
		return nil, err
	}
	if team.Valid {
		c.DefaultAssigneeTeam = team.String
	}
	if color.Valid {
		c.Color = color.String
	}
	if createAt.Valid {
		c.CreatedAt = createAt.Time
	}
	return &c, nil
}

const ticketCategoryCols = `id, name, COALESCE(description, ''), requires_server, default_priority,
	default_assignee_team, color, enabled, position, created_at`

func (s *PostgresStore) ListTicketCategories(includeDisabled bool) ([]models.TicketCategory, error) {
	query := `SELECT ` + ticketCategoryCols + ` FROM ticket_categories`
	if !includeDisabled {
		query += ` WHERE enabled = TRUE`
	}
	query += ` ORDER BY position ASC, name ASC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.TicketCategory
	for rows.Next() {
		c, err := scanTicketCategory(rows.Scan)
		if err != nil {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *PostgresStore) GetTicketCategory(id int) (*models.TicketCategory, error) {
	return scanTicketCategory(s.db.QueryRow(
		`SELECT `+ticketCategoryCols+` FROM ticket_categories WHERE id = $1`, id,
	).Scan)
}

func (s *PostgresStore) CreateTicketCategory(c *models.TicketCategory) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO ticket_categories
			(name, description, requires_server, default_priority,
			 default_assignee_team, color, enabled, position)
		 VALUES ($1, $2, $3, $4, NULLIF($5,''), NULLIF($6,''), $7, $8)
		 RETURNING id`,
		c.Name, c.Description, c.RequiresServer, c.DefaultPriority,
		c.DefaultAssigneeTeam, c.Color, c.Enabled, c.Position,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpdateTicketCategory(c *models.TicketCategory) error {
	_, err := s.db.Exec(
		`UPDATE ticket_categories
		    SET name = $1, description = $2, requires_server = $3,
		        default_priority = $4, default_assignee_team = NULLIF($5,''),
		        color = NULLIF($6,''), enabled = $7, position = $8
		  WHERE id = $9`,
		c.Name, c.Description, c.RequiresServer, c.DefaultPriority,
		c.DefaultAssigneeTeam, c.Color, c.Enabled, c.Position, c.ID,
	)
	return err
}

func (s *PostgresStore) DeleteTicketCategory(id int) error {
	// FK is ON DELETE RESTRICT on tickets — categories with tickets can't
	// be deleted. Handler should fall back to soft-disable (enabled=FALSE).
	_, err := s.db.Exec(`DELETE FROM ticket_categories WHERE id = $1`, id)
	return err
}

// ── Tickets ──────────────────────────────────────────────────────────

// category_name comes from the ticket's own column, not from the join: the
// category may have been deleted since, and the ticket keeps what it was filed
// under. COALESCE onto c.name covers a row written before the snapshot column
// existed; COALESCE on the id turns a deleted category into 0 rather than
// failing the scan.
const ticketSelectCols = `t.id, t.region, COALESCE(t.category_id, 0),
	COALESCE(NULLIF(t.category_name, ''), c.name, '') AS category_name,
	t.user_id, u.username AS user_name,
	COALESCE(t.server_uuid, ''), COALESCE(t.server_region, ''),
	COALESCE(t.subject_kind, ''), COALESCE(t.subject_ref, ''),
	t.title, t.status, t.priority,
	t.assigned_user_id, COALESCE(au.username, ''), COALESCE(t.assigned_team, ''),
	t.created_at, t.updated_at, t.closed_at`

const ticketBaseFrom = `FROM tickets t
	LEFT JOIN ticket_categories c ON t.category_id = c.id
	JOIN users u ON t.user_id = u.id
	LEFT JOIN users au ON t.assigned_user_id = au.id`

func scanTicket(scan func(dest ...interface{}) error) (*models.Ticket, error) {
	var (
		t                                                    models.Ticket
		assignedID                                           sql.NullString
		serverUUID, serverRegion, assignedName, assignedTeam string
		closedAt                                             sql.NullTime
	)
	err := scan(&t.ID, &t.Region, &t.CategoryID, &t.CategoryName,
		&t.UserID, &t.Username,
		&serverUUID, &serverRegion,
		&t.SubjectKind, &t.SubjectRef,
		&t.Title, &t.Status, &t.Priority,
		&assignedID, &assignedName, &assignedTeam,
		&t.CreatedAt, &t.UpdatedAt, &closedAt)
	if err != nil {
		return nil, err
	}
	if assignedID.Valid {
		n := assignedID.String
		t.AssignedUserID = &n
	}
	t.ServerUUID = serverUUID
	t.ServerRegion = serverRegion
	t.AssignedName = assignedName
	t.AssignedTeam = assignedTeam
	if closedAt.Valid {
		ct := closedAt.Time
		t.ClosedAt = &ct
	}
	return &t, nil
}

func (s *PostgresStore) CreateTicket(t *models.Ticket) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO tickets
			(region, category_id, category_name, user_id, server_uuid, server_region,
			 subject_kind, subject_ref,
			 title, status, priority, assigned_user_id, assigned_team)
		 VALUES ($1, $2, $3, $4, NULLIF($5,''), NULLIF($6,''),
		         $7, $8,
		         $9, $10, $11, $12, NULLIF($13,''))
		 RETURNING id`,
		t.Region, t.CategoryID, t.CategoryName, t.UserID, t.ServerUUID, t.ServerRegion,
		t.SubjectKind, t.SubjectRef,
		t.Title, t.Status, t.Priority, t.AssignedUserID, t.AssignedTeam,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) GetTicket(id int) (*models.Ticket, error) {
	query := `SELECT ` + ticketSelectCols + ` ` + ticketBaseFrom + ` WHERE t.id = $1`
	t, err := scanTicket(s.db.QueryRow(query, id).Scan)
	if err != nil {
		return nil, err
	}
	// Best-effort server name lookup — keeps the row cheap when the ticket
	// has no server attached and the join would be wasted.
	if t.ServerUUID != "" {
		var name string
		var id int
		s.db.QueryRow(`SELECT id, name FROM servers WHERE uuid = $1`, t.ServerUUID).Scan(&id, &name)
		t.ServerName = name
		t.ServerID = id
	}
	return t, nil
}

func (s *PostgresStore) ListTickets(f TicketFilter) ([]models.Ticket, error) {
	// Build WHERE incrementally. positional args ($1, $2, ...) tracked
	// alongside the slice for sql.DB.Query.
	conds := []string{"1=1"}
	args := []interface{}{}
	add := func(cond string, val interface{}) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}

	if f.UserID != nil {
		add("t.user_id = $%d", *f.UserID)
	}
	if f.AssignedUserID != nil {
		add("t.assigned_user_id = $%d", *f.AssignedUserID)
	}
	if f.AssignedTeam != "" {
		add("t.assigned_team = $%d", f.AssignedTeam)
	}
	if f.WatcherUserID != nil {
		add("EXISTS (SELECT 1 FROM ticket_watchers w WHERE w.ticket_id = t.id AND w.user_id = $%d)", *f.WatcherUserID)
	}
	if len(f.Status) > 0 {
		args = append(args, statusArrayArg(f.Status))
		conds = append(conds, fmt.Sprintf("t.status = ANY($%d)", len(args)))
	}
	if len(f.Priority) > 0 {
		args = append(args, statusArrayArg(f.Priority))
		conds = append(conds, fmt.Sprintf("t.priority = ANY($%d)", len(args)))
	}
	if f.CategoryID != nil {
		add("t.category_id = $%d", *f.CategoryID)
	}
	if f.ServerUUID != "" {
		add("t.server_uuid = $%d", f.ServerUUID)
	}
	if f.Region != "" {
		add("t.region = $%d", f.Region)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	query := `SELECT ` + ticketSelectCols + ` ` + ticketBaseFrom +
		` WHERE ` + strings.Join(conds, " AND ") +
		` ORDER BY t.updated_at DESC ` +
		fmt.Sprintf("LIMIT %d OFFSET %d", limit, offset)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Ticket
	var ids []int
	for rows.Next() {
		t, err := scanTicket(rows.Scan)
		if err != nil {
			continue
		}
		out = append(out, *t)
		ids = append(ids, t.ID)
	}

	// One-shot message-count fetch keyed by ticket id. Avoids N+1 in the
	// inbox view where the count is rendered as a badge per row.
	if len(ids) > 0 {
		counts := map[int]int{}
		rows2, err := s.db.Query(
			`SELECT ticket_id, COUNT(*) FROM ticket_messages
			 WHERE ticket_id = ANY($1) GROUP BY ticket_id`,
			intSliceArrayArg(ids),
		)
		if err == nil {
			for rows2.Next() {
				var tid, c int
				if err := rows2.Scan(&tid, &c); err == nil {
					counts[tid] = c
				}
			}
			rows2.Close()
		}
		for i := range out {
			out[i].MessageCount = counts[out[i].ID]
		}
	}

	return out, nil
}

// UpdateTicketStatus moves a ticket to status, maintaining closed_at:
// closing stamps it, resolving leaves it, and reopening clears it so reopen
// analytics stay accurate.
//
// One statement per case rather than a CASE expression, because the CASE
// version could never run. It read:
//
//	SET status = $1, closed_at = CASE WHEN $1 = 'closed' THEN NOW() ELSE closed_at END
//
// and Postgres refused it every time:
//
//	ERROR: inconsistent types deduced for parameter $1
//	DETAIL: text versus character varying
//
// $1 was used twice - once against the varchar column, once against an untyped
// literal that makes it text - and the driver declares no type, so the two
// deductions conflict and the statement never executes. Both branches that
// reached it are the two a ticket system exists to reach: resolved and closed.
// Neither was reachable; the handler turned the driver error into a plain
// "Update failed" 500.
func (s *PostgresStore) UpdateTicketStatus(id int, status string) error {
	var query string
	switch status {
	case "closed":
		query = `UPDATE tickets SET status = $1, updated_at = NOW(), closed_at = NOW() WHERE id = $2`
	case "resolved":
		query = `UPDATE tickets SET status = $1, updated_at = NOW() WHERE id = $2`
	default:
		query = `UPDATE tickets SET status = $1, updated_at = NOW(), closed_at = NULL WHERE id = $2`
	}
	_, err := s.db.Exec(query, status, id)
	return err
}

func (s *PostgresStore) UpdateTicketPriority(id int, priority string) error {
	_, err := s.db.Exec(
		`UPDATE tickets SET priority = $1, updated_at = NOW() WHERE id = $2`,
		priority, id,
	)
	return err
}

func (s *PostgresStore) UpdateTicketAssignment(id int, assignedUserID *string, assignedTeam string) error {
	_, err := s.db.Exec(
		`UPDATE tickets
		    SET assigned_user_id = $1,
		        assigned_team = NULLIF($2, ''),
		        updated_at = NOW()
		  WHERE id = $3`,
		assignedUserID, assignedTeam, id,
	)
	return err
}

func (s *PostgresStore) TouchTicketUpdated(id int) error {
	_, err := s.db.Exec(`UPDATE tickets SET updated_at = NOW() WHERE id = $1`, id)
	return err
}

// ── Messages ─────────────────────────────────────────────────────────

func (s *PostgresStore) AddTicketMessage(m *models.TicketMessage) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO ticket_messages (ticket_id, user_id, body, is_internal)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		m.TicketID, m.UserID, m.Body, m.IsInternal,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) ListTicketMessages(ticketID int, includeInternal bool) ([]models.TicketMessage, error) {
	query := `SELECT m.id, m.ticket_id, m.user_id, COALESCE(u.username, ''),
	                 COALESCE(u.role, 'user'), m.body, m.is_internal, m.created_at
	          FROM ticket_messages m
	          LEFT JOIN users u ON m.user_id = u.id
	          WHERE m.ticket_id = $1`
	if !includeInternal {
		query += ` AND m.is_internal = FALSE`
	}
	query += ` ORDER BY m.created_at ASC`
	rows, err := s.db.Query(query, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.TicketMessage
	for rows.Next() {
		var m models.TicketMessage
		// user_id is NULL once the author's account is deleted (the FK is
		// ON DELETE SET NULL, which is why the join above is a LEFT JOIN and
		// the name is COALESCEd). Scanning it into a plain string would fail,
		// and the old `continue` would have dropped the message from the
		// thread without a word - the same silent-skip that hid the spark
		// profile list being empty.
		var userID sql.NullString
		if err := rows.Scan(&m.ID, &m.TicketID, &userID, &m.Username, &m.UserRole, &m.Body, &m.IsInternal, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.UserID = userID.String
		out = append(out, m)
	}
	return out, nil
}

// ── Watchers ─────────────────────────────────────────────────────────

func (s *PostgresStore) ListTicketWatchers(ticketID int) ([]models.TicketWatcher, error) {
	rows, err := s.db.Query(
		`SELECT w.ticket_id, w.user_id, COALESCE(u.username, ''), w.can_reply, w.added_at, w.added_by
		 FROM ticket_watchers w
		 LEFT JOIN users u ON w.user_id = u.id
		 WHERE w.ticket_id = $1
		 ORDER BY w.added_at ASC`,
		ticketID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.TicketWatcher
	for rows.Next() {
		var w models.TicketWatcher
		var addedBy sql.NullString
		if err := rows.Scan(&w.TicketID, &w.UserID, &w.Username, &w.CanReply, &w.AddedAt, &addedBy); err != nil {
			continue
		}
		if addedBy.Valid {
			n := addedBy.String
			w.AddedBy = &n
		}
		out = append(out, w)
	}
	return out, nil
}

func (s *PostgresStore) AddTicketWatcher(w *models.TicketWatcher) error {
	_, err := s.db.Exec(
		`INSERT INTO ticket_watchers (ticket_id, user_id, can_reply, added_by)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (ticket_id, user_id) DO UPDATE SET can_reply = EXCLUDED.can_reply`,
		w.TicketID, w.UserID, w.CanReply, w.AddedBy,
	)
	return err
}

// RemoveTicketWatcher reports whether a row actually went away. The caller
// needs that: a DELETE matching nothing is indistinguishable from a successful
// removal by its error alone, and the handler writes an audit event on the
// strength of it.
func (s *PostgresStore) RemoveTicketWatcher(ticketID int, userID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM ticket_watchers WHERE ticket_id = $1 AND user_id = $2`, ticketID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *PostgresStore) IsTicketWatcher(ticketID int, userID string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM ticket_watchers WHERE ticket_id = $1 AND user_id = $2`,
		ticketID, userID,
	).Scan(&n)
	return n > 0, err
}

// ── Audit ────────────────────────────────────────────────────────────

func (s *PostgresStore) PurgeTicketAuditOlderThan(cutoff time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM ticket_audit_events WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *PostgresStore) InsertTicketAudit(ev *models.TicketAuditEvent) error {
	var metaJSON []byte
	if ev.Metadata != nil {
		metaJSON, _ = json.Marshal(ev.Metadata)
	}
	_, err := s.db.Exec(
		`INSERT INTO ticket_audit_events (ticket_id, event_type, actor_user_id, metadata)
		 VALUES ($1, $2, $3, NULLIF($4,'')::jsonb)`,
		ev.TicketID, ev.EventType, ev.ActorUserID, string(metaJSON),
	)
	return err
}

func (s *PostgresStore) ListTicketAudit(ticketID int) ([]models.TicketAuditEvent, error) {
	rows, err := s.db.Query(
		`SELECT e.id, e.ticket_id, e.event_type, e.actor_user_id, COALESCE(u.username, ''),
		        e.metadata, e.created_at
		 FROM ticket_audit_events e
		 LEFT JOIN users u ON e.actor_user_id = u.id
		 WHERE e.ticket_id = $1
		 ORDER BY e.created_at ASC`,
		ticketID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.TicketAuditEvent
	for rows.Next() {
		var ev models.TicketAuditEvent
		var actor sql.NullString
		var metaJSON []byte
		if err := rows.Scan(&ev.ID, &ev.TicketID, &ev.EventType, &actor, &ev.ActorName, &metaJSON, &ev.CreatedAt); err != nil {
			continue
		}
		if actor.Valid {
			n := actor.String
			ev.ActorUserID = &n
		}
		if len(metaJSON) > 0 {
			_ = json.Unmarshal(metaJSON, &ev.Metadata)
		}
		out = append(out, ev)
	}
	return out, nil
}

// ── Sidebar: servers reachable via active ticket assignments ─────────

// ListServersViaActiveTickets returns servers attached to active tickets
// (open/in_progress/waiting_user) that the given support user is assigned
// to. Drives the "Via tickets" sidebar tab.
func (s *PostgresStore) ListServersViaActiveTickets(supportUserID string) ([]models.Server, error) {
	query := `
		SELECT DISTINCT
			s.id, s.uuid, s.name, n.name AS node_name, u.username AS owner_name,
			s.port, s.status, COALESCE(s.desired_state, 'stopped'), s.game_image,
			s.is_fixed, COALESCE(s.active_sub_server, ''), s.created_at,
			COALESCE(s.region, 'default')
		FROM tickets t
		JOIN servers s ON s.uuid = t.server_uuid
		JOIN nodes n   ON s.node_id = n.id
		JOIN users u   ON s.owner_id = u.id
		WHERE t.assigned_user_id = $1
		  AND t.server_uuid IS NOT NULL
		  AND t.status IN ('open', 'in_progress', 'waiting_user')
		ORDER BY s.id ASC
	`
	rows, err := s.db.Query(query, supportUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Server
	for rows.Next() {
		var srv models.Server
		if err := rows.Scan(&srv.ID, &srv.UUID, &srv.Name, &srv.NodeName, &srv.OwnerName,
			&srv.Port, &srv.Status, &srv.DesiredState, &srv.GameImage,
			&srv.IsFixed, &srv.ActiveSubServer, &srv.CreatedAt, &srv.Region); err != nil {
			continue
		}
		srv.Role = "via_ticket"
		out = append(out, srv)
	}
	return out, nil
}

// ── pq array helpers ─────────────────────────────────────────────────
// The lib/pq driver supports ARRAY parameters via the pq.Array wrapper. To
// keep this file's imports minimal we build a Postgres text-format array
// literal directly — the values are validated/whitelisted at the handler
// level so injection isn't a concern.

func statusArrayArg(in []string) string {
	cleaned := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || strings.ContainsAny(s, `,{}"\`) {
			continue
		}
		cleaned = append(cleaned, s)
	}
	return "{" + strings.Join(cleaned, ",") + "}"
}

func intSliceArrayArg(in []int) string {
	parts := make([]string, len(in))
	for i, v := range in {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// ==========================================
// TICKETS PHASE 3 — attachments, canned responses, notifications, auto-close
// ==========================================

// ── Attachments ───────────────────────────────────────────────────────

func (s *PostgresStore) AddTicketAttachment(a *models.TicketAttachment) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO ticket_attachments
			(ticket_id, message_id, filename, mime, size_bytes, storage_key, uploaded_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		a.TicketID, a.MessageID, a.Filename, a.Mime, a.SizeBytes, a.StorageKey, a.UploadedBy,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) GetTicketAttachment(id int) (*models.TicketAttachment, error) {
	var a models.TicketAttachment
	var msgID sql.NullInt64
	var uploadedBy sql.NullString
	err := s.db.QueryRow(
		`SELECT id, ticket_id, message_id, filename, mime, size_bytes, storage_key, uploaded_by, created_at
		   FROM ticket_attachments WHERE id = $1`,
		id,
	).Scan(&a.ID, &a.TicketID, &msgID, &a.Filename, &a.Mime, &a.SizeBytes, &a.StorageKey, &uploadedBy, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	if msgID.Valid {
		n := int(msgID.Int64)
		a.MessageID = &n
	}
	if uploadedBy.Valid {
		n := uploadedBy.String
		a.UploadedBy = &n
	}
	return &a, nil
}

func (s *PostgresStore) ListTicketAttachments(ticketID int) ([]models.TicketAttachment, error) {
	rows, err := s.db.Query(
		`SELECT a.id, a.ticket_id, a.message_id, a.filename, a.mime, a.size_bytes,
		        a.storage_key, a.uploaded_by, COALESCE(u.username, ''), a.created_at
		   FROM ticket_attachments a
		   LEFT JOIN users u ON a.uploaded_by = u.id
		   WHERE a.ticket_id = $1
		   ORDER BY a.created_at ASC`,
		ticketID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.TicketAttachment
	for rows.Next() {
		var a models.TicketAttachment
		var msgID sql.NullInt64
		var uploadedBy sql.NullString
		if err := rows.Scan(&a.ID, &a.TicketID, &msgID, &a.Filename, &a.Mime, &a.SizeBytes, &a.StorageKey, &uploadedBy, &a.Username, &a.CreatedAt); err != nil {
			continue
		}
		if msgID.Valid {
			n := int(msgID.Int64)
			a.MessageID = &n
		}
		if uploadedBy.Valid {
			n := uploadedBy.String
			a.UploadedBy = &n
		}
		out = append(out, a)
	}
	return out, nil
}

func (s *PostgresStore) DeleteTicketAttachment(id int) error {
	_, err := s.db.Exec(`DELETE FROM ticket_attachments WHERE id = $1`, id)
	return err
}

func (s *PostgresStore) SumAttachmentBytesByTicket(ticketID int) (int64, error) {
	var sum sql.NullInt64
	err := s.db.QueryRow(
		`SELECT SUM(size_bytes) FROM ticket_attachments WHERE ticket_id = $1`,
		ticketID,
	).Scan(&sum)
	if err != nil {
		return 0, err
	}
	if sum.Valid {
		return sum.Int64, nil
	}
	return 0, nil
}

func (s *PostgresStore) SumAttachmentBytesByUser(userID string) (int64, error) {
	var sum sql.NullInt64
	err := s.db.QueryRow(
		`SELECT SUM(size_bytes) FROM ticket_attachments WHERE uploaded_by = $1`,
		userID,
	).Scan(&sum)
	if err != nil {
		return 0, err
	}
	if sum.Valid {
		return sum.Int64, nil
	}
	return 0, nil
}

// ── Canned responses ─────────────────────────────────────────────────

func scanCannedResponse(scan func(dest ...interface{}) error) (*models.CannedResponse, error) {
	var c models.CannedResponse
	var catID sql.NullInt64
	var createdBy sql.NullString
	err := scan(&c.ID, &c.Name, &c.Body, &catID, &createdBy, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if catID.Valid {
		n := int(catID.Int64)
		c.CategoryID = &n
	}
	if createdBy.Valid {
		n := createdBy.String
		c.CreatedBy = &n
	}
	return &c, nil
}

const cannedResponseCols = `id, name, body, category_id, created_by, created_at, updated_at`

func (s *PostgresStore) ListCannedResponses(categoryID *int) ([]models.CannedResponse, error) {
	query := `SELECT ` + cannedResponseCols + ` FROM ticket_canned_responses`
	var rows *sql.Rows
	var err error
	if categoryID != nil {
		query += ` WHERE category_id IS NULL OR category_id = $1 ORDER BY name ASC`
		rows, err = s.db.Query(query, *categoryID)
	} else {
		query += ` ORDER BY name ASC`
		rows, err = s.db.Query(query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.CannedResponse
	for rows.Next() {
		c, err := scanCannedResponse(rows.Scan)
		if err != nil {
			continue
		}
		out = append(out, *c)
	}
	return out, nil
}

func (s *PostgresStore) GetCannedResponse(id int) (*models.CannedResponse, error) {
	return scanCannedResponse(s.db.QueryRow(
		`SELECT `+cannedResponseCols+` FROM ticket_canned_responses WHERE id = $1`, id,
	).Scan)
}

func (s *PostgresStore) CreateCannedResponse(c *models.CannedResponse) (int, error) {
	var id int
	err := s.db.QueryRow(
		`INSERT INTO ticket_canned_responses (name, body, category_id, created_by)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		c.Name, c.Body, c.CategoryID, c.CreatedBy,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) UpdateCannedResponse(c *models.CannedResponse) error {
	_, err := s.db.Exec(
		`UPDATE ticket_canned_responses
		    SET name = $1, body = $2, category_id = $3, updated_at = NOW()
		  WHERE id = $4`,
		c.Name, c.Body, c.CategoryID, c.ID,
	)
	return err
}

func (s *PostgresStore) DeleteCannedResponse(id int) error {
	_, err := s.db.Exec(`DELETE FROM ticket_canned_responses WHERE id = $1`, id)
	return err
}

// ── Notifications ────────────────────────────────────────────────────

func (s *PostgresStore) InsertNotification(n *models.Notification) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		`INSERT INTO notifications (user_id, type, title, body, link)
		 VALUES ($1, $2, $3, $4, NULLIF($5, '')) RETURNING id`,
		n.UserID, n.Type, n.Title, n.Body, n.Link,
	).Scan(&id)
	return id, err
}

func (s *PostgresStore) ListNotifications(userID string, includeRead bool, limit int) ([]models.Notification, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id, user_id, type, title, body, COALESCE(link, ''), read_at, created_at
		FROM notifications WHERE user_id = $1`
	if !includeRead {
		query += ` AND read_at IS NULL`
	}
	query += ` ORDER BY created_at DESC LIMIT ` + fmt.Sprintf("%d", limit)
	rows, err := s.db.Query(query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Notification
	for rows.Next() {
		var n models.Notification
		var readAt sql.NullTime
		if err := rows.Scan(&n.ID, &n.UserID, &n.Type, &n.Title, &n.Body, &n.Link, &readAt, &n.CreatedAt); err != nil {
			continue
		}
		if readAt.Valid {
			t := readAt.Time
			n.ReadAt = &t
		}
		out = append(out, n)
	}
	return out, nil
}

func (s *PostgresStore) CountUnreadNotifications(userID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`,
		userID,
	).Scan(&n)
	return n, err
}

func (s *PostgresStore) MarkNotificationRead(id int64, userID string) error {
	_, err := s.db.Exec(
		`UPDATE notifications SET read_at = NOW()
		   WHERE id = $1 AND user_id = $2 AND read_at IS NULL`,
		id, userID,
	)
	return err
}

func (s *PostgresStore) MarkAllNotificationsRead(userID string) error {
	_, err := s.db.Exec(
		`UPDATE notifications SET read_at = NOW()
		   WHERE user_id = $1 AND read_at IS NULL`,
		userID,
	)
	return err
}

// ── Auto-close + participant fan-out ─────────────────────────────────

func (s *PostgresStore) ListResolvedTicketsOlderThan(cutoff time.Time) ([]int, error) {
	rows, err := s.db.Query(
		`SELECT id FROM tickets
		   WHERE status = 'resolved' AND updated_at < $1`,
		cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	// The retention sweep deletes exactly these tickets.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ==========================================
// MIGRATION + BACKUP
// ==========================================

// allowedTicketTables is the strict whitelist for any table-named operation
// (count, dump, restore). Prevents injection through the table parameter.
var allowedTicketTables = map[string]bool{
	"ticket_categories":       true,
	"tickets":                 true,
	"ticket_messages":         true,
	"ticket_watchers":         true,
	"ticket_audit_events":     true,
	"ticket_attachments":      true,
	"ticket_canned_responses": true,
}

// TicketTablesInOrder returns the tables in FK-safe order so a restore /
// migration can insert them sequentially without deferring constraints.
func TicketTablesInOrder() []string {
	return []string{
		"ticket_categories",
		"tickets",
		"ticket_messages",
		"ticket_watchers",
		"ticket_audit_events",
		"ticket_attachments",
		"ticket_canned_responses",
	}
}

func (s *PostgresStore) CountTicketRows(table string) (int, error) {
	if !allowedTicketTables[table] {
		return 0, fmt.Errorf("table %q not in ticket allowlist", table)
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
	return n, err
}

// DumpTicketTable returns every row of the named table as a []map. JSONB
// columns come back as []byte; the caller (backup handler) re-marshals to
// JSON so the on-disk format is normal JSON.
func (s *PostgresStore) DumpTicketTable(table string) ([]map[string]interface{}, error) {
	if !allowedTicketTables[table] {
		return nil, fmt.Errorf("table %q not in ticket allowlist", table)
	}
	rows, err := s.db.Query(`SELECT * FROM ` + table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []map[string]interface{}
	for rows.Next() {
		// Use []interface{} buffer; sql.Scan populates each based on the
		// column type. JSONB / INET come back as []byte, which JSON-marshals
		// as base64 — handler re-encodes those to readable strings.
		vals := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		row := make(map[string]interface{}, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		out = append(out, row)
	}
	// This feeds both the ticket backup and the cross-database migration. A
	// short read there writes an archive that is missing rows and reports
	// success - the one place a partial answer must never pass for a complete
	// one, because the gap only surfaces at restore time.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// ==========================================
// SERVER AUDIT
// ==========================================

func (s *PostgresStore) InsertServerAudit(ev *models.ServerAuditEvent) error {
	var metaJSON []byte
	if ev.Metadata != nil {
		metaJSON, _ = json.Marshal(ev.Metadata)
	}
	region := ev.Region
	if region == "" {
		region = "default"
	}
	_, err := s.db.Exec(
		`INSERT INTO server_audit_events
			(server_id, region, event_type, actor_user_id, target_user_id,
			 metadata, ip_address, user_agent)
		 VALUES ($1, $2, $3, $4, $5,
		         NULLIF($6, '')::jsonb, NULLIF($7, '')::inet, NULLIF($8, ''))`,
		ev.ServerID, region, ev.EventType, ev.ActorUserID, ev.TargetUserID,
		string(metaJSON), ev.IPAddress, ev.UserAgent,
	)
	return err
}

func (s *PostgresStore) ListServerAudit(serverID int, eventType string, limit, offset int) ([]models.ServerAuditEvent, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	// Total count for pagination — separate query so the page query stays
	// cheap when callers don't need the count.
	var total int
	countQ := `SELECT COUNT(*) FROM server_audit_events WHERE server_id = $1`
	countArgs := []interface{}{serverID}
	if eventType != "" {
		countQ += ` AND event_type = $2`
		countArgs = append(countArgs, eventType)
	}
	if err := s.db.QueryRow(countQ, countArgs...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT e.id, e.server_id, e.region, e.event_type,
	             e.actor_user_id, COALESCE(au.username, ''),
	             e.target_user_id, COALESCE(tu.username, ''),
	             e.metadata, e.ip_address::text, e.user_agent, e.created_at
	      FROM server_audit_events e
	      LEFT JOIN users au ON e.actor_user_id = au.id
	      LEFT JOIN users tu ON e.target_user_id = tu.id
	      WHERE e.server_id = $1`
	args := []interface{}{serverID}
	if eventType != "" {
		q += ` AND e.event_type = $2`
		args = append(args, eventType)
	}
	q += fmt.Sprintf(` ORDER BY e.created_at DESC LIMIT %d OFFSET %d`, limit, offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []models.ServerAuditEvent
	for rows.Next() {
		var ev models.ServerAuditEvent
		var actor, target sql.NullString
		var metaJSON []byte
		var ipAddr, ua sql.NullString
		if err := rows.Scan(&ev.ID, &ev.ServerID, &ev.Region, &ev.EventType,
			&actor, &ev.ActorName,
			&target, &ev.TargetName,
			&metaJSON, &ipAddr, &ua, &ev.CreatedAt); err != nil {
			continue
		}
		if actor.Valid {
			n := actor.String
			ev.ActorUserID = &n
		}
		if target.Valid {
			n := target.String
			ev.TargetUserID = &n
		}
		if len(metaJSON) > 0 {
			_ = json.Unmarshal(metaJSON, &ev.Metadata)
		}
		if ipAddr.Valid {
			ev.IPAddress = ipAddr.String
		}
		if ua.Valid {
			ev.UserAgent = ua.String
		}
		out = append(out, ev)
	}
	return out, total, nil
}

func (s *PostgresStore) SetServerAuditEnabled(serverID int, enabled bool) error {
	_, err := s.db.Exec(`UPDATE servers SET audit_enabled = $1 WHERE id = $2`, enabled, serverID)
	return err
}

func (s *PostgresStore) SetServerAuditForceOn(serverID int, force bool) error {
	_, err := s.db.Exec(`UPDATE servers SET audit_force_on = $1 WHERE id = $2`, force, serverID)
	return err
}

func (s *PostgresStore) GetServerAuditState(serverID int) (bool, bool, int, error) {
	var enabled, force bool
	var count int
	err := s.db.QueryRow(
		`SELECT COALESCE(audit_enabled, FALSE), COALESCE(audit_force_on, FALSE),
		        (SELECT COUNT(*) FROM server_audit_events WHERE server_id = $1)
		   FROM servers WHERE id = $1`,
		serverID,
	).Scan(&enabled, &force, &count)
	return enabled, force, count, err
}

func (s *PostgresStore) PurgeServerAuditOlderThan(cutoff time.Time) (int, error) {
	res, err := s.db.Exec(`DELETE FROM server_audit_events WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ListTicketParticipantsForNotify returns user IDs that should receive a
// notification when something happens on the ticket. Combines:
//   - the ticket creator
//   - the assigned supporter (if any)
//   - all watchers
//
// excludeUserID is used to skip the actor (you don't get notified for your
// own reply). Returns deduplicated, sorted IDs.
func (s *PostgresStore) ListTicketParticipantsForNotify(ticketID int, excludeUserID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT user_id FROM (
		    SELECT user_id FROM tickets WHERE id = $1
		    UNION
		    SELECT assigned_user_id AS user_id FROM tickets WHERE id = $1 AND assigned_user_id IS NOT NULL
		    UNION
		    SELECT user_id FROM ticket_watchers WHERE ticket_id = $1
		 ) AS p WHERE user_id IS NOT NULL AND user_id != $2`,
		ticketID, excludeUserID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// ==========================================
// TICKET DELETION (admin-only, audited)
// ==========================================

// ListAttachmentStorageKeysByTicket returns the storage_key column for every
// attachment on a ticket so the handler can DeletePath() the file blobs
// after the rows are gone. Empty/duplicate keys are skipped.
func (s *PostgresStore) ListAttachmentStorageKeysByTicket(ticketID int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT storage_key FROM ticket_attachments WHERE ticket_id = $1`,
		ticketID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	seen := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			continue
		}
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	// Ticket deletion removes the blobs behind these keys; a key missed here is
	// an object that outlives its ticket and is never referenced again.
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteTicket removes the ticket and every dependent row in a single tx.
// ON DELETE CASCADE on ticket_messages / ticket_watchers / ticket_audit_events
// / ticket_attachments would already cover most of this; we still issue
// explicit deletes so the contract is obvious from the source and so deletes
// fire in a deterministic order. notifications are not FK'd to tickets — they
// stay (the link will 404 gracefully when followed, by design).
func (s *PostgresStore) DeleteTicket(id int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Order matters only when FK constraints aren't set ON DELETE CASCADE; we
	// run the explicit deletes regardless so the path stays correct even if
	// the cascade is loosened in the future.
	for _, q := range []string{
		`DELETE FROM ticket_attachments    WHERE ticket_id = $1`,
		`DELETE FROM ticket_watchers       WHERE ticket_id = $1`,
		`DELETE FROM ticket_messages       WHERE ticket_id = $1`,
		`DELETE FROM ticket_audit_events   WHERE ticket_id = $1`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	res, err := tx.Exec(`DELETE FROM tickets WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// InsertTicketDeletion stamps the audit row. id + deleted_at default
// server-side; leave them blank in the caller-supplied struct.
func (s *PostgresStore) InsertTicketDeletion(rec *models.TicketDeletion) error {
	if rec == nil {
		return fmt.Errorf("nil deletion record")
	}
	var ip interface{}
	if rec.IPAddress != nil && *rec.IPAddress != "" {
		ip = *rec.IPAddress
	}
	var ua interface{}
	if rec.UserAgent != nil && *rec.UserAgent != "" {
		ua = *rec.UserAgent
	}
	var cat interface{}
	if rec.CategoryName != nil && *rec.CategoryName != "" {
		cat = *rec.CategoryName
	}
	var owner interface{}
	if rec.OwnerUserID != nil && *rec.OwnerUserID != "" {
		owner = *rec.OwnerUserID
	}
	return s.db.QueryRow(
		`INSERT INTO ticket_deletions
			(ticket_id, ticket_subject, owner_user_id, owner_username, category_name,
			 deleted_by, deleted_by_name, ip_address, user_agent)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id, deleted_at`,
		rec.TicketID, rec.TicketSubject, owner, rec.OwnerUsername, cat,
		rec.DeletedBy, rec.DeletedByName, ip, ua,
	).Scan(&rec.ID, &rec.DeletedAt)
}

// ListTicketDeletions returns (rows, total). limit is clamped to [1,200],
// offset to [0,∞). deletedBy "" means no actor filter.
func (s *PostgresStore) ListTicketDeletions(limit, offset int, deletedBy string) ([]models.TicketDeletion, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	where := ""
	args := []interface{}{}
	if deletedBy != "" {
		where = " WHERE deleted_by = $1"
		args = append(args, deletedBy)
	}

	var total int
	countQuery := `SELECT COUNT(*) FROM ticket_deletions` + where
	if err := s.db.QueryRow(countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limitArg := len(args) + 1
	offsetArg := len(args) + 2
	args = append(args, limit, offset)
	rowsQ := `SELECT id, ticket_id, ticket_subject, owner_user_id, owner_username,
	                 category_name, deleted_by, deleted_by_name, deleted_at,
	                 host(ip_address), user_agent
	            FROM ticket_deletions` + where +
		fmt.Sprintf(` ORDER BY deleted_at DESC LIMIT $%d OFFSET $%d`, limitArg, offsetArg)
	rows, err := s.db.Query(rowsQ, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []models.TicketDeletion{}
	for rows.Next() {
		var d models.TicketDeletion
		var owner, cat, ip, ua sql.NullString
		if err := rows.Scan(&d.ID, &d.TicketID, &d.TicketSubject, &owner, &d.OwnerUsername,
			&cat, &d.DeletedBy, &d.DeletedByName, &d.DeletedAt, &ip, &ua); err != nil {
			continue
		}
		if owner.Valid && owner.String != "" {
			s := owner.String
			d.OwnerUserID = &s
		}
		if cat.Valid && cat.String != "" {
			s := cat.String
			d.CategoryName = &s
		}
		if ip.Valid && ip.String != "" {
			s := ip.String
			d.IPAddress = &s
		}
		if ua.Valid && ua.String != "" {
			s := ua.String
			d.UserAgent = &s
		}
		out = append(out, d)
	}
	return out, total, nil
}
