package store

import "database/sql"

// RotateTabShare gives a tab a new share token and, when it has a content
// host, a new host label, so the old link AND the host it pointed at stop
// working together. An expiry already in the past is dropped with the old
// token; a future one is the owner's choice and stays.
func RotateTabShare(db *sql.DB, tabID, serverID int, token, label string) (sql.Result, error) {
	return db.Exec(`UPDATE server_tabs
		SET share_token=$3,
		    proxy_host_label = CASE WHEN proxy_host_label IS NULL THEN NULL ELSE $4::text END,
		    share_expires_at = CASE WHEN share_expires_at <= now() THEN NULL ELSE share_expires_at END
		WHERE id=$1 AND server_id=$2`, tabID, serverID, token, label)
}
