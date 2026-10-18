package store

import (
	"database/sql"

	"dylaris-core/models"
)

// server_modpack_contents queries. The per-(server, sub-server) snapshot of a
// modpack's Modrinth-identified members, backing the advisory Content-tab
// cross-check. Written wholesale at install/reinstall; read by the panel.

// ReplaceServerModpackContents clears the (server_id, sub_server_name) snapshot
// and re-inserts rows, atomically, so a reinstall refreshes cleanly. Passing an
// empty slice clears the snapshot (e.g. a reinstall to a non-modpack installer).
// ON CONFLICT DO UPDATE keeps the last write if a pack lists a project twice.
func (s *PostgresStore) ReplaceServerModpackContents(serverID int, subServer string, rows []models.ServerModpackContent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := replaceModpackContentsTx(tx, serverID, subServer, rows); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceModpackContentsTx(tx *sql.Tx, serverID int, subServer string, rows []models.ServerModpackContent) error {
	if _, err := tx.Exec(`DELETE FROM server_modpack_contents
		WHERE server_id=$1 AND sub_server_name=$2`, serverID, subServer); err != nil {
		return err
	}
	for _, r := range rows {
		if _, err := tx.Exec(`INSERT INTO server_modpack_contents
			(server_id, sub_server_name, modrinth_project_id, modrinth_version_id,
			 modrinth_version_number, file_name, side)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (server_id, sub_server_name, modrinth_project_id)
			DO UPDATE SET
				modrinth_version_id     = EXCLUDED.modrinth_version_id,
				modrinth_version_number = EXCLUDED.modrinth_version_number,
				file_name               = EXCLUDED.file_name,
				side                    = EXCLUDED.side`,
			serverID, subServer, r.ModrinthProjectID, r.ModrinthVersionID,
			r.ModrinthVersionNumber, r.FileName, r.Side); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) GetModpackScan(serverID int, subServer string) (string, int, int, error) {
	var scan string
	var jars, gen int
	err := s.db.QueryRow(`SELECT modpack_scan, modpack_jars, modpack_scan_gen FROM sub_server_installs
		WHERE server_id=$1 AND sub_server_name=$2`, serverID, subServer).Scan(&scan, &jars, &gen)
	if err == sql.ErrNoRows {
		return "", 0, 0, nil
	}
	return scan, jars, gen, err
}

func (s *PostgresStore) SetModpackScan(serverID int, subServer, scan string, jars int) error {
	// Owing a scan starts a new generation, so a scan of the previous one
	// cannot complete over it.
	bump := 0
	if scan == "pending" {
		bump = 1
	}
	_, err := s.db.Exec(`UPDATE sub_server_installs SET modpack_scan=$3, modpack_jars=$4,
		modpack_scan_gen = modpack_scan_gen + $5
		WHERE server_id=$1 AND sub_server_name=$2`, serverID, subServer, scan, jars, bump)
	return err
}

func (s *PostgresStore) CompleteModpackScan(serverID int, subServer string, gen int, rows []models.ServerModpackContent, jars int) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE sub_server_installs SET modpack_scan='done', modpack_jars=$3
		WHERE server_id=$1 AND sub_server_name=$2 AND modpack_scan='pending' AND modpack_scan_gen=$4`,
		serverID, subServer, jars, gen)
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if err := replaceModpackContentsTx(tx, serverID, subServer, rows); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *PostgresStore) ListServerModpackContents(serverID int, subServer string) ([]models.ServerModpackContent, error) {
	rows, err := s.db.Query(`SELECT id, server_id, sub_server_name, modrinth_project_id,
		modrinth_version_id, modrinth_version_number, file_name, side
		FROM server_modpack_contents
		WHERE server_id=$1 AND sub_server_name=$2
		ORDER BY file_name ASC`, serverID, subServer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ServerModpackContent{}
	for rows.Next() {
		var m models.ServerModpackContent
		if err := rows.Scan(&m.ID, &m.ServerID, &m.SubServerName, &m.ModrinthProjectID,
			&m.ModrinthVersionID, &m.ModrinthVersionNumber, &m.FileName, &m.Side); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
