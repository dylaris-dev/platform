package store

import (
	"database/sql"
	"dylaris-core/models"
	"errors"
)

// server_mods queries (Modrinth-sourced installs).

var errServerModNotFound = errors.New("server mod not found")

const serverModCols = `id, server_id, sub_server_name, modrinth_project_id,
		modrinth_project_slug, modrinth_version_id, title, file_name, target_dir,
		sha512, installed_at, installed_by, status, status_message, install_id`

func scanServerMod(row interface {
	Scan(dest ...interface{}) error
}) (*models.ServerMod, error) {
	var m models.ServerMod
	var installedBy sql.NullString
	if err := row.Scan(&m.ID, &m.ServerID, &m.SubServerName, &m.ModrinthProjectID,
		&m.ModrinthProjectSlug, &m.ModrinthVersionID, &m.Title, &m.FileName, &m.TargetDir,
		&m.SHA512, &m.InstalledAt, &installedBy, &m.Status, &m.StatusMessage, &m.InstallID); err != nil {
		return nil, err
	}
	if installedBy.Valid {
		v := installedBy.String
		m.InstalledBy = &v
	}
	return &m, nil
}

func (s *PostgresStore) UpsertServerMod(m *models.ServerMod) (int, error) {
	var installedBy interface{}
	if m.InstalledBy != nil {
		installedBy = *m.InstalledBy
	}
	var id int
	err := s.db.QueryRow(`INSERT INTO server_mods
		(server_id, sub_server_name, modrinth_project_id, modrinth_project_slug,
		 modrinth_version_id, title, file_name, target_dir, sha512, installed_by,
		 status, status_message, install_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'',$12)
		ON CONFLICT (server_id, sub_server_name, modrinth_project_id)
		DO UPDATE SET
			modrinth_project_slug = EXCLUDED.modrinth_project_slug,
			modrinth_version_id   = EXCLUDED.modrinth_version_id,
			title                 = EXCLUDED.title,
			file_name             = EXCLUDED.file_name,
			target_dir            = EXCLUDED.target_dir,
			sha512                = EXCLUDED.sha512,
			installed_at          = NOW(),
			installed_by          = EXCLUDED.installed_by,
			status                = EXCLUDED.status,
			status_message        = '',
			install_id            = EXCLUDED.install_id
		RETURNING id`,
		m.ServerID, m.SubServerName, m.ModrinthProjectID, m.ModrinthProjectSlug,
		m.ModrinthVersionID, m.Title, m.FileName, m.TargetDir, m.SHA512, installedBy,
		m.Status, m.InstallID,
	).Scan(&id)
	return id, err
}

// GetServerModByProject returns the row a new install of this project would
// REPLACE, or nil when there is none.
//
// The install path needs it before it writes: the upsert overwrites file_name,
// so after it runs nothing knows which jar the old version left on disk. That
// is not a detail - it is the whole reason a server ended up carrying two
// copies of one mod, with only the new one nameable.
func (s *PostgresStore) GetServerModByProject(serverID int, subServerName, projectID string) (*models.ServerMod, error) {
	m, err := scanServerMod(s.db.QueryRow(`SELECT `+serverModCols+`
		FROM server_mods
		WHERE server_id=$1 AND sub_server_name=$2 AND modrinth_project_id=$3`,
		serverID, subServerName, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// SetServerModStatus records what the node reported.
//
// installID is compared rather than trusted: a node answering about an attempt
// this row has already moved past must not overwrite the newer one's state, and
// two clicks in a row is all it takes to produce that. A mismatch is not an
// error, it is a late answer - the caller is told nothing changed by the zero
// rows affected.
func (s *PostgresStore) SetServerModStatus(serverID int, subServerName, projectID, installID, status, message string) (bool, error) {
	res, err := s.db.Exec(`UPDATE server_mods
		SET status=$5, status_message=$6
		WHERE server_id=$1 AND sub_server_name=$2 AND modrinth_project_id=$3 AND install_id=$4`,
		serverID, subServerName, projectID, installID, status, message)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// serverModHistoryDepth is how many superseded versions are kept per project.
// Three, because past the last couple of builds nobody rolls back from memory -
// they pick a build from the list, which the tab already shows.
const serverModHistoryDepth = 3

// RecordServerModHistory files the version an install is replacing and trims
// the project's history back to serverModHistoryDepth.
func (s *PostgresStore) RecordServerModHistory(serverID int, subServerName string, prev *models.ServerMod) error {
	if prev == nil {
		return nil
	}
	if _, err := s.db.Exec(`INSERT INTO server_mod_history
		(server_id, sub_server_name, modrinth_project_id, modrinth_version_id,
		 title, file_name, target_dir, sha512, installed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		serverID, subServerName, prev.ModrinthProjectID, prev.ModrinthVersionID,
		prev.Title, prev.FileName, prev.TargetDir, prev.SHA512, prev.InstalledAt,
	); err != nil {
		return err
	}
	// Trimmed by id within the project, not by a global LIMIT: two projects on
	// one server keep three each.
	_, err := s.db.Exec(`DELETE FROM server_mod_history
		WHERE server_id=$1 AND sub_server_name=$2 AND modrinth_project_id=$3
		  AND id NOT IN (
			SELECT id FROM server_mod_history
			WHERE server_id=$1 AND sub_server_name=$2 AND modrinth_project_id=$3
			ORDER BY replaced_at DESC, id DESC
			LIMIT $4)`,
		serverID, subServerName, prev.ModrinthProjectID, serverModHistoryDepth)
	return err
}

// ListServerModHistory returns the superseded versions for one sub-server,
// newest first, so the panel can offer a way back.
func (s *PostgresStore) ListServerModHistory(serverID int, subServerName string) ([]models.ServerModHistoryEntry, error) {
	rows, err := s.db.Query(`SELECT id, modrinth_project_id, modrinth_version_id,
			title, file_name, target_dir, sha512, installed_at, replaced_at
		FROM server_mod_history
		WHERE server_id=$1 AND sub_server_name=$2
		ORDER BY replaced_at DESC, id DESC`, serverID, subServerName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ServerModHistoryEntry{}
	for rows.Next() {
		var e models.ServerModHistoryEntry
		if err := rows.Scan(&e.ID, &e.ModrinthProjectID, &e.ModrinthVersionID, &e.Title,
			&e.FileName, &e.TargetDir, &e.SHA512, &e.InstalledAt, &e.ReplacedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListServerMods(serverID int, subServerName string) ([]models.ServerMod, error) {
	rows, err := s.db.Query(`SELECT `+serverModCols+`
		FROM server_mods WHERE server_id=$1 AND sub_server_name=$2
		ORDER BY title ASC, installed_at DESC`, serverID, subServerName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ServerMod{}
	for rows.Next() {
		m, err := scanServerMod(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func (s *PostgresStore) DeleteServerMod(id, serverID int) error {
	res, err := s.db.Exec(`DELETE FROM server_mods WHERE id=$1 AND server_id=$2`, id, serverID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errServerModNotFound
	}
	return nil
}

// ListServerModSubServers returns every sub-server on this server that has mod
// rows.
//
// Needed because a whole-server backup has to describe sub-servers the install
// records do not know about: server_mods and sub_server_installs are written by
// different paths, and a sub-server that predates install records still carries
// mods. Describing only one source leaves the other's rows untouched by a
// restore, which is the exact divergence the manifest exists to close.
func (s *PostgresStore) ListServerModSubServers(serverID int) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT sub_server_name FROM server_mods WHERE server_id=$1 ORDER BY sub_server_name`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ReplaceServerMods makes the rows for one (server, sub-server) exactly mods.
//
// One transaction, delete then insert, because the two halves are one statement
// of fact: a restore says "this is what was installed", and a crash between them
// must not leave a server with no mods recorded and files that say otherwise.
//
// An EMPTY slice is a legitimate instruction and clears the scope. That is the
// backup-then-install-then-restore case: the archive had no mods, the row was
// added afterwards, and the jar is gone the moment the directory is replaced.
//
// installed_by is left NULL and status is "installed". The row is being restated
// from an archive, not installed by a person, and the archive deliberately
// carries no user id - it may have come from another platform, whose user ids
// mean nothing here.
func (s *PostgresStore) ReplaceServerMods(serverID int, subServerName string, mods []models.ServerMod) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM server_mods WHERE server_id=$1 AND sub_server_name=$2`,
		serverID, subServerName); err != nil {
		return err
	}
	for i := range mods {
		m := &mods[i]
		if _, err := tx.Exec(`INSERT INTO server_mods
			(server_id, sub_server_name, modrinth_project_id, modrinth_project_slug,
			 modrinth_version_id, title, file_name, target_dir, sha512, installed_by,
			 status, status_message, install_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,NULL,$10,'','')
			ON CONFLICT (server_id, sub_server_name, modrinth_project_id) DO NOTHING`,
			serverID, subServerName, m.ModrinthProjectID, m.ModrinthProjectSlug,
			m.ModrinthVersionID, m.Title, m.FileName, m.TargetDir, m.SHA512,
			models.ServerModInstalled); err != nil {
			return err
		}
	}
	return tx.Commit()
}
