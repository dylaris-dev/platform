package store

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// database/sql reports an iteration that ended because of an error only through
// rows.Err(). Without that check a connection dropped mid-stream, a cancelled
// context or a driver-side failure ends the loop early and the function returns
// the rows read so far with a NIL error - a short answer that is indistinguishable
// from a genuinely short table.
//
// The sweep found 44 of 101 loops without the check. Most return display lists,
// where a short read is a cosmetic wrong answer. These are the ones where the
// list drives a decision, so a partial answer causes a partial ACTION:
// the routing migration, the backup scheduler, the ticket dump, the retention
// jobs and the two access lists.
//
// errStreamCut stands in for whatever ends an iteration early.
var errStreamCut = errors.New("connection reset while streaming rows")

// TestGetAllActiveServersReportsAShortRead is the sharpest of these. The
// routing-mode handler saves the new mode FIRST and then calls
// RoutingMigration.Run, which loads the fleet through this function and returns
// (0, nil) when the list is empty. A cut stream therefore switched the platform
// to the new mode, migrated NOTHING, and answered
// {"success":true,"serversQueued":0} - which the panel renders as a plain
// "Routing mode saved.", identical to a platform with no servers. Every server
// stays on the old routing. That is the same end state as 01fa507, reached
// through a different door.
func TestGetAllActiveServersReportsAShortRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM servers")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1).RowError(0, errStreamCut))

	servers, err := s.GetAllActiveServers()
	if err == nil {
		t.Fatalf("a cut stream returned %d servers and a nil error; the routing migration would report success having migrated nothing", len(servers))
	}
	if !errors.Is(err, errStreamCut) {
		t.Errorf("err = %v, want the stream error", err)
	}
}

// The scheduler runs exactly the jobs this returns and never learns about the
// ones it did not see, so a short read is a backup that quietly does not happen
// while the schedule still looks healthy.
func TestListDueBackupJobsReportsAShortRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta("FROM backup_jobs")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1).RowError(0, errStreamCut))

	jobs, err := s.ListDueBackupJobs(time.Now())
	if err == nil {
		t.Fatalf("a cut stream returned %d due jobs and a nil error; those backups would silently not run", len(jobs))
	}
}

// DumpTicketTable feeds both the ticket backup and the cross-database
// migration. A short read there writes an archive that is missing rows and
// reports success; the gap only surfaces at restore time.
func TestDumpTicketTableReportsAShortRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM tickets")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1).RowError(0, errStreamCut))

	rows, err := s.DumpTicketTable("tickets")
	if err == nil {
		t.Fatalf("a cut stream dumped %d rows and reported success; the archive would be silently incomplete", len(rows))
	}
}

// Both of these decide what a user may reach.
func TestAccessListsReportAShortRead(t *testing.T) {
	cases := []struct {
		name  string
		table string
		call  func(*PostgresStore) (int, error)
	}{
		{"GetUserRegionIDs", "user_regions", func(s *PostgresStore) (int, error) {
			ids, err := s.GetUserRegionIDs("user-1")
			return len(ids), err
		}},
		{"GetSFTPAccessByNode", "servers", func(s *PostgresStore) (int, error) {
			a, err := s.GetSFTPAccessByNode(1)
			return len(a), err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()

			mock.ExpectQuery(regexp.QuoteMeta(tc.table)).
				WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow("a").RowError(0, errStreamCut))

			n, err := tc.call(NewPostgresStore(db))
			if err == nil {
				t.Fatalf("a cut stream returned %d entries and a nil error; the caller would silently narrow the user's access", n)
			}
		})
	}
}

// The retention jobs delete exactly what these return.
func TestRetentionListsReportAShortRead(t *testing.T) {
	cases := []struct {
		name  string
		table string
		call  func(*PostgresStore) (int, error)
	}{
		{"ListUsersDueForDeletion", "users", func(s *PostgresStore) (int, error) {
			ids, err := s.ListUsersDueForDeletion(time.Now())
			return len(ids), err
		}},
		{"ListAttachmentStorageKeysByTicket", "ticket_attachments", func(s *PostgresStore) (int, error) {
			keys, err := s.ListAttachmentStorageKeysByTicket(1)
			return len(keys), err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()

			mock.ExpectQuery(regexp.QuoteMeta(tc.table)).
				WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow("a").RowError(0, errStreamCut))

			n, err := tc.call(NewPostgresStore(db))
			if err == nil {
				t.Fatalf("a cut stream returned %d rows and a nil error", n)
			}
		})
	}
}

// The routing migration sends what this returns as the cpuset: the effective
// one, so a pinned server keeps its cores and a shared one gets the node's.
func TestGetAllActiveServersCarriesTheEffectiveCpuset(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)
	cols := []string{"id", "uuid", "node_id", "node_name", "node_token", "status", "host_port", "container_port", "memory", "cpu_limit", "disk_limit", "start_command", "game_image", "active_sub_server", "extra_jvm_flags", "mode", "cpuset", "node_cpuset"}
	mock.ExpectQuery(regexp.QuoteMeta("FROM servers")).WillReturnRows(sqlmock.NewRows(cols).
		AddRow(1, "pinned", 1, "n", "tok", "online", 0, 25565, 1024, 0.0, 0, "", "img", "s", "", "auto", "2-3", "0-7").
		AddRow(2, "shared", 1, "n", "tok", "online", 0, 25565, 1024, 0.0, 0, "", "img", "s", "", "shared", "", "0-7"))
	servers, err := s.GetAllActiveServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Cpuset != "2-3" || servers[1].Cpuset != "0-7" {
		t.Fatalf("cpusets = %+v", servers)
	}
}
