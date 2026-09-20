package store

import (
	"database/sql"
	"strconv"
	"strings"
)

// The two lookups that answer "which archives are about to lose the row that
// names them".
//
// Deleting a backup SCHEDULE deletes only its own row, and deleting a SERVER
// deletes only the server's. Everything below them goes by foreign key:
// backup_jobs.server_id and backup_runs.job_id are both ON DELETE CASCADE, and
// backup_runs.storage_key is the ONLY record of where an archive lives. So the
// rows vanish, the objects stay, and nothing can name them again - they cannot
// even be listed as orphans without walking the bucket.
//
// Both queries mirror ListBackupRunsByOwner: the RUN's storage where it has
// one, the job's only as a fallback, because an archive written before the job
// was pointed elsewhere still lives where it was written.

// ListBackupRunRefsForJob returns every run of one schedule.
func (s *PostgresStore) ListBackupRunRefsForJob(jobID int) ([]BackupRunRef, error) {
	rows, err := s.db.Query(`
		SELECT br.id, br.storage_key, COALESCE(br.storage_id, bj.storage_id), COALESCE(sv.owner_id::text, '')
		FROM backup_runs br
		JOIN backup_jobs bj ON bj.id = br.job_id
		LEFT JOIN servers sv ON sv.id = bj.server_id
		WHERE br.job_id = $1`, jobID)
	if err != nil {
		return nil, err
	}
	return scanBackupRunRefs(rows)
}

// ListBackupRunRefsForServers returns every run of every schedule of the given
// servers. Empty in, empty out - a caller with no servers must not read as "all
// of them", which is what an unguarded IN () would do.
func (s *PostgresStore) ListBackupRunRefsForServers(serverIDs []int) ([]BackupRunRef, error) {
	if len(serverIDs) == 0 {
		return nil, nil
	}
	args := make([]any, len(serverIDs))
	ph := make([]string, len(serverIDs))
	for i, id := range serverIDs {
		args[i] = id
		ph[i] = "$" + strconv.Itoa(i+1)
	}
	rows, err := s.db.Query(`
		SELECT br.id, br.storage_key, COALESCE(br.storage_id, bj.storage_id), COALESCE(sv.owner_id::text, '')
		FROM backup_runs br
		JOIN backup_jobs bj ON bj.id = br.job_id
		LEFT JOIN servers sv ON sv.id = bj.server_id
		WHERE bj.server_id IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	return scanBackupRunRefs(rows)
}

func scanBackupRunRefs(rows *sql.Rows) ([]BackupRunRef, error) {
	defer rows.Close()
	var out []BackupRunRef
	for rows.Next() {
		var ref BackupRunRef
		var sid sql.NullInt64
		if err := rows.Scan(&ref.RunID, &ref.StorageKey, &sid, &ref.OwnerID); err != nil {
			return nil, err
		}
		if sid.Valid {
			v := int(sid.Int64)
			ref.StorageID = &v
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}
