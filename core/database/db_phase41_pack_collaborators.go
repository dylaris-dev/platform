package database

import (
	"database/sql"
	"fmt"
)

// applyPackCollaboratorsSchema adds modpack collaboration: an owner lets
// another user work on their packs (all of them, pack_id NULL, or one), with
// the rights the owner ticked. Nothing here is Modrinth-specific: a
// collaborator acts on the PACK, and publishing asks for the owner's
// credentials of whichever provider the pack publishes to.
//
// pack_audit_events is the history the owner can read: who changed what on
// which pack. pack_id goes NULL when the pack is deleted so the history of a
// deleted pack survives; pack_name keeps what it was called.
func applyPackCollaboratorsSchema(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS pack_collaborators (
			id          SERIAL PRIMARY KEY,
			owner_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			pack_id     INTEGER REFERENCES packs(id) ON DELETE CASCADE,
			can_create  BOOLEAN NOT NULL DEFAULT FALSE,
			can_edit    BOOLEAN NOT NULL DEFAULT FALSE,
			can_delete  BOOLEAN NOT NULL DEFAULT FALSE,
			created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CHECK (owner_id <> user_id)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS pack_collaborators_all ON pack_collaborators(owner_id, user_id) WHERE pack_id IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS pack_collaborators_one ON pack_collaborators(owner_id, user_id, pack_id) WHERE pack_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS pack_collaborators_user ON pack_collaborators(user_id)`,
		`CREATE TABLE IF NOT EXISTS pack_audit_events (
			id             BIGSERIAL PRIMARY KEY,
			owner_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			pack_id        INTEGER REFERENCES packs(id) ON DELETE SET NULL,
			pack_name      TEXT NOT NULL DEFAULT '',
			actor_user_id  UUID REFERENCES users(id) ON DELETE SET NULL,
			action         TEXT NOT NULL,
			detail         JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS pack_audit_owner ON pack_audit_events(owner_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS pack_audit_pack ON pack_audit_events(pack_id, created_at DESC)`,
	} {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("pack collaborators schema: %w", err)
		}
	}
	return nil
}
