package database

import (
	"database/sql"
	"fmt"
)

// createTicketTables sets up the ticket-system schema: categories,
// tickets themselves, messages on those tickets, watchers (CC), and per-ticket
// audit events. The region column is on tickets from day one so a future
// cross-region migration doesn't have to rewrite history.
func createTicketTables(db *sql.DB) error {
	tables := []string{
		// Categories: admin-curated, users pick one when creating a ticket.
		// requires_server gates the server picker on the create form.
		// default_priority pre-seeds the priority dropdown.
		// default_assignee_team is the team string used as fallback assignee.
		`CREATE TABLE IF NOT EXISTS ticket_categories (
			id                    SERIAL PRIMARY KEY,
			name                  VARCHAR(128) NOT NULL,
			description           TEXT NOT NULL DEFAULT '',
			requires_server       BOOLEAN NOT NULL DEFAULT FALSE,
			default_priority      VARCHAR(16) NOT NULL DEFAULT 'normal',
			default_assignee_team VARCHAR(64),
			color                 VARCHAR(16),
			enabled               BOOLEAN NOT NULL DEFAULT TRUE,
			position              INTEGER NOT NULL DEFAULT 0,
			created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(name)
		)`,
		// Tickets. server_uuid + server_region are nullable — only set when
		// the category required a server. assigned_user_id is the supporter
		// owning the ticket; assigned_team scopes visibility per the
		// support_team semantics.
		`CREATE TABLE IF NOT EXISTS tickets (
			id               SERIAL PRIMARY KEY,
			region           VARCHAR(32) NOT NULL DEFAULT 'default',
			-- Nullable + SET NULL, and the NAME is snapshotted beside it: a ticket
			-- must survive the deletion of the category it was filed under.
			-- See applyTicketCategorySnapshot.
			category_id      INTEGER REFERENCES ticket_categories(id) ON DELETE SET NULL,
			category_name    TEXT NOT NULL DEFAULT '',
			user_id          UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			server_uuid      VARCHAR(64),
			server_region    VARCHAR(32),
			title            VARCHAR(200) NOT NULL,
			status           VARCHAR(32) NOT NULL DEFAULT 'open',
			priority         VARCHAR(16) NOT NULL DEFAULT 'normal',
			assigned_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
			assigned_team    VARCHAR(64),
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			closed_at        TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tickets_user        ON tickets(user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_tickets_status      ON tickets(status, priority, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_tickets_assignee    ON tickets(assigned_user_id) WHERE assigned_user_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_tickets_team        ON tickets(assigned_team) WHERE assigned_team IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_tickets_server      ON tickets(server_uuid) WHERE server_uuid IS NOT NULL`,

		// Messages on a ticket. is_internal hides the message from the
		// ticket creator + watchers — only visible to support+admin.
		//
		// user_id is nullable ON PURPOSE: the FK is ON DELETE SET NULL so a
		// message survives its author's account deletion with the author link
		// dropped (the read query already LEFT JOINs and COALESCEs the name).
		// It was NOT NULL, which contradicts the SET NULL: deleting a user
		// raised a not-null violation instead, so anyone who had ever written
		// a ticket message could never be deleted, and the failure surfaced as
		// a plain 500 because the handler only maps foreign-key violations.
		`CREATE TABLE IF NOT EXISTS ticket_messages (
			id          SERIAL PRIMARY KEY,
			ticket_id   INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
			user_id     UUID REFERENCES users(id) ON DELETE SET NULL,
			body        TEXT NOT NULL,
			is_internal BOOLEAN NOT NULL DEFAULT FALSE,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ticket_messages_ticket ON ticket_messages(ticket_id, created_at ASC)`,
		// Existing databases were created with the NOT NULL above. Idempotent.
		`ALTER TABLE ticket_messages ALTER COLUMN user_id DROP NOT NULL`,

		// Watchers / CC. can_reply distinguishes "read-only" from
		// "co-resolves with me" — admin sets the policy default.
		`CREATE TABLE IF NOT EXISTS ticket_watchers (
			ticket_id  INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
			user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			can_reply  BOOLEAN NOT NULL DEFAULT FALSE,
			added_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			added_by   UUID REFERENCES users(id) ON DELETE SET NULL,
			PRIMARY KEY (ticket_id, user_id)
		)`,

		// Per-ticket audit. Separate from audit_events_identity since the
		// audience and retention concerns differ (ticket audit is shown in
		// the UI to support+admin; identity audit is admin-only ops history).
		`CREATE TABLE IF NOT EXISTS ticket_audit_events (
			id            BIGSERIAL PRIMARY KEY,
			ticket_id     INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
			event_type    VARCHAR(64) NOT NULL,
			actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
			metadata      JSONB,
			created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ticket_audit_ticket ON ticket_audit_events(ticket_id, created_at DESC)`,

		// Attachments table — storage layer holds the actual bytes;
		// this table is just metadata + the storage key the provider uses
		// to retrieve. message_id is nullable so attachments can be
		// uploaded into the create-ticket form before any messages exist.
		`CREATE TABLE IF NOT EXISTS ticket_attachments (
			id          SERIAL PRIMARY KEY,
			ticket_id   INTEGER NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
			message_id  INTEGER REFERENCES ticket_messages(id) ON DELETE SET NULL,
			filename    VARCHAR(255) NOT NULL,
			mime        VARCHAR(128) NOT NULL DEFAULT 'application/octet-stream',
			size_bytes  BIGINT NOT NULL DEFAULT 0,
			storage_key VARCHAR(512) NOT NULL,
			uploaded_by UUID REFERENCES users(id) ON DELETE SET NULL,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ticket_attachments_ticket ON ticket_attachments(ticket_id, created_at ASC)`,

		// Canned responses — admin-managed snippets that support
		// staff insert into replies. category_id ties a snippet to a
		// specific category for discoverability (NULL = global).
		// body supports variable expansion at insert-time on the client:
		// {{user_name}}, {{ticket_id}}, {{server_name}}, {{actor_name}}.
		`CREATE TABLE IF NOT EXISTS ticket_canned_responses (
			id          SERIAL PRIMARY KEY,
			name        VARCHAR(128) NOT NULL,
			body        TEXT NOT NULL,
			category_id INTEGER REFERENCES ticket_categories(id) ON DELETE SET NULL,
			created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(name)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_canned_category ON ticket_canned_responses(category_id) WHERE category_id IS NOT NULL`,

		// Notifications — generic in-app inbox; ticket events are
		// the first producer but the schema is intentionally open-ended
		// so other systems (backups, maintenance, security questions)
		// can write here later.
		`CREATE TABLE IF NOT EXISTS notifications (
			id         BIGSERIAL PRIMARY KEY,
			user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			type       VARCHAR(64) NOT NULL,
			title      VARCHAR(200) NOT NULL,
			body       TEXT NOT NULL DEFAULT '',
			link       VARCHAR(500),
			read_at    TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_user_unread ON notifications(user_id, created_at DESC) WHERE read_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_user        ON notifications(user_id, created_at DESC)`,

		// Per-server audit — indexed by (server_id, created_at)
		// since the dominant query is "events for one server, newest first".
		// Index on event_type lets the future filter dropdown stay fast.
		// metadata stays JSONB so producers can shape it freely.
		`CREATE TABLE IF NOT EXISTS server_audit_events (
			id             BIGSERIAL PRIMARY KEY,
			server_id      INTEGER NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
			region         VARCHAR(32) NOT NULL DEFAULT 'default',
			event_type     VARCHAR(64) NOT NULL,
			actor_user_id  UUID REFERENCES users(id) ON DELETE SET NULL,
			target_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
			metadata       JSONB,
			ip_address     INET,
			user_agent     TEXT,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_server_audit_server ON server_audit_events(server_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_server_audit_type   ON server_audit_events(event_type, created_at DESC)`,

		// ticket_deletions — append-only audit log for admin-driven ticket
		// deletions. Decoupled from tickets via no FK so the row survives the
		// referenced ticket disappearing. Username + category snapshots are
		// stored as text to stay readable after the source rows are gone.
		// deleted_by stays as UUID FK (SET NULL) so the actor's identity row
		// can be anonymized without nuking the audit history.
		`CREATE TABLE IF NOT EXISTS ticket_deletions (
			id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			ticket_id       INTEGER NOT NULL,
			ticket_subject  TEXT NOT NULL,
			owner_user_id   UUID,
			owner_username  TEXT NOT NULL,
			category_name   TEXT,
			deleted_by      UUID NOT NULL,
			deleted_by_name TEXT NOT NULL,
			deleted_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			ip_address      INET,
			user_agent      TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ticket_deletions_deleted_at ON ticket_deletions (deleted_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_ticket_deletions_deleted_by ON ticket_deletions (deleted_by)`,

		// What the ticket is ABOUT. server_uuid only ever covered servers, so a
		// customer whose BYON machine or protected address was the problem had
		// nowhere to say so and support had to work it out from the prose.
		//
		// subject_kind is 'server' | 'node' | 'route' | '' (nothing specific).
		// subject_ref names the node or the route; for a server the authority
		// stays server_uuid, so there is one place a server id lives.
		`ALTER TABLE tickets ADD COLUMN IF NOT EXISTS subject_kind VARCHAR(16) NOT NULL DEFAULT ''`,
		`ALTER TABLE tickets ADD COLUMN IF NOT EXISTS subject_ref VARCHAR(128) NOT NULL DEFAULT ''`,
	}
	for _, q := range tables {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("create ticket tables: %w", err)
		}
	}
	return nil
}
