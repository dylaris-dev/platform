package database

import (
	"database/sql"
	"fmt"
)

func applyModrinthWatchSchema(db *sql.DB) error {
	for _, statement := range []string{
		`ALTER TABLE packs ADD COLUMN IF NOT EXISTS recommended_build_id INTEGER REFERENCES pack_builds(id) ON DELETE SET NULL`,
		`CREATE TABLE IF NOT EXISTS modrinth_favorites (
		 user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		 project_id VARCHAR(64) NOT NULL, title VARCHAR(200) NOT NULL,
		 PRIMARY KEY(user_id,project_id))`,
		`CREATE TABLE IF NOT EXISTS modrinth_watches (
		 id BIGSERIAL PRIMARY KEY, user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		 build_id INTEGER REFERENCES pack_builds(id) ON DELETE CASCADE,
		 spec JSONB NOT NULL, delivered JSONB NOT NULL DEFAULT '[]', result JSONB NOT NULL DEFAULT '[]',
		 last_error TEXT NOT NULL DEFAULT '', last_checked TIMESTAMPTZ,
		 next_check TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE INDEX IF NOT EXISTS modrinth_watches_due ON modrinth_watches(next_check)`,
		`CREATE INDEX IF NOT EXISTS modrinth_watches_user ON modrinth_watches(user_id,id)`,
		`ALTER TABLE modrinth_watches ADD COLUMN IF NOT EXISTS pack_id INTEGER REFERENCES packs(id) ON DELETE CASCADE`,
		`CREATE INDEX IF NOT EXISTS modrinth_watches_pack ON modrinth_watches(pack_id) WHERE pack_id IS NOT NULL`,
		`ALTER TABLE notifications ADD COLUMN IF NOT EXISTS dedupe_key TEXT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS notifications_dedupe ON notifications(user_id,dedupe_key) WHERE dedupe_key IS NOT NULL`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("modrinth watch schema: %w", err)
		}
	}
	return nil
}
