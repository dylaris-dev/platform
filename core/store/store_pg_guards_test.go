package store

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var backupRunMockCols = []string{"id", "job_id", "started_at", "completed_at", "status", "size_bytes", "storage_key",
	"error_message", "install_snapshot", "manifest", "storage_id", "upload_id", "part_size", "uploaded_bytes"}

// The caller deletes the archive behind every run this returns. It used to
// SELECT the candidates and then DELETE each row on its own, dropping the
// error: a failed DELETE left the row listed while its archive went, and a
// restore created between the two statements had its row cascaded away and
// its archive deleted under it. One statement that re-applies the guard and
// returns only what it removed closes both.
func TestPruneOldBackupRunsReturnsOnlyWhatOneGuardedDeleteRemoved(t *testing.T) {
	guarded := regexp.MustCompile(`(?s)DELETE FROM backup_runs.*status = 'success'.*NOT EXISTS .*backup_restores.*RETURNING`)
	for _, tc := range []struct {
		name    string
		rows    *sqlmock.Rows
		err     error
		wantIDs []int
		wantErr bool
	}{
		{"deleted rows are returned", sqlmock.NewRows(backupRunMockCols).
			AddRow(3, 9, time.Now(), nil, "success", 1, "k3", "", "", "", nil, nil, nil, nil), nil, []int{3}, false},
		{"a failed delete is an error, not a list", nil, errStreamCut, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(_, actual string) error {
				if !guarded.MatchString(actual) {
					return errors.New("not one guarded DELETE ... RETURNING: " + actual)
				}
				return nil
			})))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			q := mock.ExpectQuery("").WithArgs(9, 1)
			if tc.err != nil {
				q.WillReturnError(tc.err)
			} else {
				q.WillReturnRows(tc.rows)
			}
			got, err := NewPostgresStore(db).PruneOldBackupRuns(9, 1)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && got != nil {
				t.Fatalf("returned %d runs alongside an error; the caller would delete their archives", len(got))
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("got %d runs, want %v", len(got), tc.wantIDs)
			}
			for i, id := range tc.wantIDs {
				if got[i].ID != id {
					t.Errorf("run %d id = %d, want %d", i, got[i].ID, id)
				}
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Anonymize keeps the row and its invites, so the account's SFTP keys kept
// signing in to every server it was invited to under the new username, and
// pending_email (an address, PII) survived the wipe.
func TestAnonymizeUserRemovesSFTPKeysAndPendingEmail(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(`(?s)DELETE FROM user_ssh_keys WHERE user_id = \$2.*pending_email = NULL`).
		WithArgs("<deleted_user_u1>", "u1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := NewPostgresStore(db).AnonymizeUser("u1"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A cooldown of 0 is a value the panel and the handler accept; it read back
// as 30. And a failed read must not turn a disabled rename switch back on.
func TestGetUserAccountPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rows      [][2]string
		loadErr   error
		wantAllow bool
		wantDays  int
		wantErr   bool
	}{
		{"zero cooldown stays zero", [][2]string{{"users.allow_name_change", "true"}, {"users.name_change_cooldown_days", "0"}}, nil, true, 0, false},
		{"never saved is the default", nil, nil, true, 30, false},
		{"unparsable cooldown is the default", [][2]string{{"users.name_change_cooldown_days", "x"}}, nil, true, 30, false},
		{"disabled stays disabled", [][2]string{{"users.allow_name_change", "false"}, {"users.name_change_cooldown_days", "7"}}, nil, false, 7, false},
		{"a failed read is an error and does not allow", nil, errStreamCut, false, 30, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			q := mock.ExpectQuery(regexp.QuoteMeta(`SELECT key, value FROM settings`))
			if tc.loadErr != nil {
				q.WillReturnError(tc.loadErr)
			} else {
				rows := sqlmock.NewRows([]string{"key", "value"})
				for _, r := range tc.rows {
					rows.AddRow(r[0], r[1])
				}
				q.WillReturnRows(rows)
			}
			allow, days, err := NewPostgresStore(db).GetUserAccountPolicy()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if allow != tc.wantAllow || days != tc.wantDays {
				t.Errorf("got allow=%v days=%d, want allow=%v days=%d", allow, days, tc.wantAllow, tc.wantDays)
			}
		})
	}
}

// The network-policy publisher builds every node's policy from this list; a
// short list with a nil error drops proxy backends from it.
func TestListServersReportsAShortRead(t *testing.T) {
	cols := []string{"id", "uuid", "name", "node_id", "node_name", "owner_id", "owner_name", "port", "status",
		"desired_state", "game_image", "is_fixed", "active_sub_server", "created_at", "server_type", "proxy_id"}
	for _, tc := range []struct {
		name string
		rows *sqlmock.Rows
	}{
		{"cut stream", sqlmock.NewRows(cols).
			AddRow(1, "u", "a", 1, "n", "o", "x", 1, "online", "running", "img", true, "", time.Now(), "game", nil).
			RowError(0, errStreamCut)},
		{"unscannable row", sqlmock.NewRows(cols).
			AddRow("not-an-int", "u", "a", 1, "n", "o", "x", 1, "online", "running", "img", true, "", time.Now(), "game", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectQuery(regexp.QuoteMeta("FROM servers s")).WillReturnRows(tc.rows)
			servers, err := NewPostgresStore(db).ListServers("")
			if err == nil {
				t.Fatalf("returned %d servers and a nil error for a partial read", len(servers))
			}
		})
	}
}

// The reaper's close must not land on a run that left "running" or whose
// upload Core completed while the reaper worked through its batch.
func TestFailAbandonedBackupRunIsConditional(t *testing.T) {
	for _, tc := range []struct {
		name     string
		affected int64
		want     bool
	}{
		{"still abandoned", 1, true},
		{"completed or closed meanwhile", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			now := time.Now()
			mock.ExpectExec(`(?s)UPDATE backup_runs SET status = 'failed'.*WHERE id = \$4 AND status = 'running' AND uploaded_bytes IS NULL`).
				WithArgs("msg", int64(5), now, 7).WillReturnResult(sqlmock.NewResult(0, tc.affected))
			got, err := NewPostgresStore(db).FailAbandonedBackupRun(7, "msg", 5, now)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("landed = %v, want %v", got, tc.want)
			}
		})
	}
}

// R39-04: a progress report is written after a read, so the reaper or a
// server move can close the run in between. Writing "running" must only land
// on a row that is still running; an outcome is written as before.
func TestUpdateBackupRunStatusProgressNeedsARunningRow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status string
	}{
		{"progress", "running"},
		{"an outcome", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectExec(`(?s)UPDATE backup_runs SET status = \$1.*WHERE id = \$6 AND \(\$7 OR status = 'running'\)`).
				WithArgs(tc.status, "", int64(5), "k", nil, 7, tc.status != "running").WillReturnResult(sqlmock.NewResult(0, 0))
			if err := NewPostgresStore(db).UpdateBackupRunStatus(7, tc.status, "", 5, "k", time.Time{}); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
