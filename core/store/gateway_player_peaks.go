package store

import "time"

// PlayerPeak is one account's highest concurrent players on one UTC day, each
// column its own maximum. A lower bound: a sample that was never taken (edge
// update, Core restart, leader change) is not in it.
type PlayerPeak struct {
	Day                  time.Time // UTC midnight
	Java, Bedrock, Total int
}

// The day goes to Postgres as a date string: a time.Time would be read in the
// session time zone and could land on the neighbouring date.
func pgDay(t time.Time) string { return t.Format("2006-01-02") }

// RaisePlayerPeak raises the owner's row for day column by column. GREATEST
// keeps a stored value from ever going down, whichever process writes and in
// which order; a sample at or below the row changes nothing, not even
// updated_at.
func (s *PostgresStore) RaisePlayerPeak(ownerID string, day time.Time, java, bedrock, total int) error {
	_, err := s.db.Exec(`
		INSERT INTO gateway_player_peaks (owner_id, day, java, bedrock, total, updated_at)
		VALUES ($1, $2::date, $3, $4, $5, NOW())
		ON CONFLICT (owner_id, day) DO UPDATE SET
			java       = GREATEST(gateway_player_peaks.java, EXCLUDED.java),
			bedrock    = GREATEST(gateway_player_peaks.bedrock, EXCLUDED.bedrock),
			total      = GREATEST(gateway_player_peaks.total, EXCLUDED.total),
			updated_at = NOW()
		WHERE EXCLUDED.java > gateway_player_peaks.java
		   OR EXCLUDED.bedrock > gateway_player_peaks.bedrock
		   OR EXCLUDED.total > gateway_player_peaks.total`,
		ownerID, pgDay(day), java, bedrock, total)
	return err
}

// ListPlayerPeaks is the owner's rows from since (a UTC day) on, newest first.
func (s *PostgresStore) ListPlayerPeaks(ownerID string, since time.Time) ([]PlayerPeak, error) {
	rows, err := s.db.Query(`
		SELECT day, java, bedrock, total FROM gateway_player_peaks
		WHERE owner_id = $1 AND day >= $2::date
		ORDER BY day DESC`, ownerID, pgDay(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlayerPeak
	for rows.Next() {
		var p PlayerPeak
		var d time.Time
		if err := rows.Scan(&d, &p.Java, &p.Bedrock, &p.Total); err != nil {
			return nil, err
		}
		p.Day = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PurgePlayerPeaks deletes the rows before the day before. Today's rows (UTC)
// survive whatever cut is passed, and a second run deletes nothing more.
func (s *PostgresStore) PurgePlayerPeaks(before time.Time) (int64, error) {
	res, err := s.db.Exec(`
		DELETE FROM gateway_player_peaks
		WHERE day < LEAST($1::date, (NOW() AT TIME ZONE 'UTC')::date)`, pgDay(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
