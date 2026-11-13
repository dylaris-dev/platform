package database

import (
	"database/sql"
	"fmt"
)

// applyRemoteGateway adds the local state a customer Core keeps when it uses
// ANOTHER Core as its gateway provider (managed gateway stage 7a).
//
// The tables are only ever written in remote mode, but they exist always: a
// switch back to local mode leaves the rows in place, so flipping the mode
// again reconciles instead of starting from nothing.
//
//   - remote_link_kits: the provider-issued link per node. link_key is the
//     bearer secret and is sealed at rest (store.remoteKitPurpose).
//   - remote_routes: what the provider holds for us, so a lookup by server
//     does not need a call.
//   - gateway_outbox: provider writes that failed or were rate limited and are
//     retried by the drain, so a 429 in the middle of a node drain loses nothing.
func applyRemoteGateway(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS remote_link_kits (
			node_id    BIGINT PRIMARY KEY,
			link_id    TEXT NOT NULL UNIQUE,
			link_key   TEXT NOT NULL,
			region     TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS remote_routes (
			domain         TEXT PRIMARY KEY,
			server_uuid    TEXT NOT NULL,
			node_id        BIGINT NOT NULL,
			port           INT NOT NULL,
			proxy_protocol BOOLEAN NOT NULL DEFAULT FALSE,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_remote_routes_server ON remote_routes (server_uuid)`,
		`CREATE TABLE IF NOT EXISTS gateway_outbox (
			id              BIGSERIAL PRIMARY KEY,
			op              TEXT NOT NULL,
			domain          TEXT NOT NULL,
			payload         JSONB NOT NULL,
			attempts        INT NOT NULL DEFAULT 0,
			next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			last_error      TEXT NOT NULL DEFAULT '',
			created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			done_at         TIMESTAMPTZ
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_outbox_due ON gateway_outbox (next_attempt_at) WHERE done_at IS NULL`,
		// The per-domain lookups (HasOpenOutbox, OldestOpenOutboxID, the claim's
		// "older open row" check) run on every create and every drained item.
		`CREATE INDEX IF NOT EXISTS idx_gateway_outbox_open_domain ON gateway_outbox (domain, id) WHERE done_at IS NULL`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("remote gateway tables: %w", err)
		}
	}
	return nil
}
