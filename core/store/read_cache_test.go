package store

import (
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// fakeClock moves cacheNow for one test.
func fakeClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	prev := cacheNow
	cacheNow = func() time.Time { return now }
	t.Cleanup(func() { cacheNow = prev })
	return &now
}

const hasUsersQ = `SELECT EXISTS (SELECT 1 FROM users)`

// The setup gate asks once per second while the answer is true, and every
// time while it is false: a fresh install must unlock on the next request.
func TestHasUsersCachesOnlyTrueForOneSecond(t *testing.T) {
	for _, tc := range []struct {
		name      string
		answers   []bool
		advance   []time.Duration // before each call after the first
		wantCalls int
	}{
		{"true is served from cache inside the second", []bool{true}, []time.Duration{0, 500 * time.Millisecond, 499 * time.Millisecond}, 1},
		{"true is asked again after the second", []bool{true, true}, []time.Duration{0, time.Second}, 2},
		{"false is never cached", []bool{false, false, false}, []time.Duration{0, 0, 0}, 3},
		{"false then true unlocks at once", []bool{false, true}, []time.Duration{0, 0}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := fakeClock(t)
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, a := range tc.answers {
				mock.ExpectQuery(regexp.QuoteMeta(hasUsersQ)).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(a))
			}
			s := NewPostgresStore(db)
			var last bool
			for i, d := range tc.advance {
				*now = now.Add(d)
				got, err := s.HasUsers()
				if err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
				last = got
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("want %d queries: %v", tc.wantCalls, err)
			}
			if want := tc.answers[len(tc.answers)-1]; last != want {
				t.Fatalf("last answer %v, want %v", last, want)
			}
		})
	}
}

const loadSettingsQ = `SELECT key, value FROM settings`

func settingsRows(kv ...string) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"key", "value"})
	for i := 0; i+1 < len(kv); i += 2 {
		r.AddRow(kv[i], kv[i+1])
	}
	return r
}

// Many reads inside one second cost one query, and a key without a row keeps
// answering sql.ErrNoRows - callers tell "never saved" from "unreadable" by it.
func TestGetSettingServesTheSnapshot(t *testing.T) {
	now := fakeClock(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(loadSettingsQ)).WillReturnRows(settingsRows("a", "1", "b", "").AddRow("n", nil))
	mock.ExpectQuery(regexp.QuoteMeta(loadSettingsQ)).WillReturnRows(settingsRows("a", "2"))
	s := NewPostgresStore(db)

	for _, tc := range []struct {
		key     string
		want    string
		wantErr error
	}{
		{"a", "1", nil},
		{"b", "", nil},
		{"missing", "", sql.ErrNoRows},
		{"n", "", errSettingNull},
	} {
		got, err := s.GetSetting(tc.key)
		if got != tc.want || !errors.Is(err, tc.wantErr) {
			t.Fatalf("GetSetting(%q) = %q, %v; want %q, %v", tc.key, got, err, tc.want, tc.wantErr)
		}
	}
	*now = now.Add(999 * time.Millisecond)
	if got, _ := s.GetSetting("a"); got != "1" {
		t.Fatalf("inside the second: %q", got)
	}
	*now = now.Add(time.Millisecond)
	if got, _ := s.GetSetting("a"); got != "2" {
		t.Fatalf("after the second: %q, want the reloaded 2", got)
	}
	if _, err := s.GetSetting("b"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("a row deleted elsewhere must read as missing after reload, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A replica sees its own writes at once: every writer drops the snapshot.
func TestSettingWritesInvalidateTheSnapshot(t *testing.T) {
	fakeClock(t)
	for _, tc := range []struct {
		name  string
		write func(s *PostgresStore, mock sqlmock.Sqlmock) error
	}{
		{"SetSetting", func(s *PostgresStore, mock sqlmock.Sqlmock) error {
			mock.ExpectExec("INSERT INTO settings").WillReturnResult(sqlmock.NewResult(0, 1))
			return s.SetSetting("a", "2")
		}},
		{"SetSettingBy", func(s *PostgresStore, mock sqlmock.Sqlmock) error {
			mock.ExpectExec("INSERT INTO settings").WillReturnResult(sqlmock.NewResult(0, 1))
			return s.SetSettingBy("a", "2", "u")
		}},
		{"InsertSettingIfAbsent", func(s *PostgresStore, mock sqlmock.Sqlmock) error {
			mock.ExpectExec("INSERT INTO settings").WillReturnResult(sqlmock.NewResult(0, 1))
			_, err := s.InsertSettingIfAbsent("a", "2")
			return err
		}},
		{"DeleteSetting", func(s *PostgresStore, mock sqlmock.Sqlmock) error {
			mock.ExpectExec("DELETE FROM settings").WillReturnResult(sqlmock.NewResult(0, 1))
			return s.DeleteSetting("a")
		}},
		{"ConsumeOneShotJoin", func(s *PostgresStore, mock sqlmock.Sqlmock) error {
			mock.ExpectQuery("UPDATE settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("disabled"))
			_, err := s.ConsumeOneShotJoin()
			return err
		}},
		{"UpdateSetting", func(s *PostgresStore, mock sqlmock.Sqlmock) error {
			mock.ExpectBegin()
			mock.ExpectExec("INSERT INTO settings").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery("SELECT value FROM settings").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("1"))
			mock.ExpectExec("UPDATE settings").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			return s.UpdateSetting("a", func(string) (string, error) { return "2", nil })
		}},
		{"SetUserAccountPolicy", func(s *PostgresStore, mock sqlmock.Sqlmock) error {
			mock.ExpectExec("INSERT INTO settings").WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec("INSERT INTO settings").WillReturnResult(sqlmock.NewResult(0, 1))
			return s.SetUserAccountPolicy(true, 30)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			s := NewPostgresStore(db)
			mock.ExpectQuery(regexp.QuoteMeta(loadSettingsQ)).WillReturnRows(settingsRows("a", "1"))
			if got, _ := s.GetSetting("a"); got != "1" {
				t.Fatalf("seed read %q", got)
			}
			if err := tc.write(s, mock); err != nil {
				t.Fatalf("write: %v", err)
			}
			mock.ExpectQuery(regexp.QuoteMeta(loadSettingsQ)).WillReturnRows(settingsRows("a", "2"))
			if got, _ := s.GetSetting("a"); got != "2" {
				t.Fatalf("read after own write = %q, want 2 (snapshot not dropped)", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A failed load is not cached and is not ErrNoRows: GetFailClosed and the
// demo gate must see "unreadable", and the next read must ask again.
func TestSettingsLoadErrorIsNotCached(t *testing.T) {
	fakeClock(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewPostgresStore(db)
	mock.ExpectQuery(regexp.QuoteMeta(loadSettingsQ)).WillReturnError(errors.New("db down"))
	mock.ExpectQuery(regexp.QuoteMeta(loadSettingsQ)).WillReturnRows(settingsRows("a", "1"))
	if _, err := s.GetSetting("a"); err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("load error surfaced as %v", err)
	}
	if got, err := s.GetSetting("a"); got != "1" || err != nil {
		t.Fatalf("second read %q, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A write that lands while a load is in flight must not let that load's
// (possibly pre-write) result become the cached snapshot.
func TestSettingsLoadRacingAWriteIsNotKept(t *testing.T) {
	fakeClock(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewPostgresStore(db)
	mock.ExpectQuery(regexp.QuoteMeta(loadSettingsQ)).WillDelayFor(300 * time.Millisecond).WillReturnRows(settingsRows("a", "old"))
	mock.ExpectQuery(regexp.QuoteMeta(loadSettingsQ)).WillReturnRows(settingsRows("a", "new"))
	done := make(chan string)
	go func() {
		v, _ := s.GetSetting("a")
		done <- v
	}()
	time.Sleep(100 * time.Millisecond) // the load is in flight
	s.invalidateSettings()             // a write commits meanwhile
	if v := <-done; v != "old" {
		t.Fatalf("overlapping reader got %q", v)
	}
	if got, _ := s.GetSetting("a"); got != "new" {
		t.Fatalf("got %q: the racing load was kept", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

const listModulesQ = `SELECT id, name, type, icon, COALESCE(url, ''), is_enabled, is_system, position, COALESCE(access_role, 'all') FROM modules ORDER BY position ASC, id ASC`

func moduleRows(names ...string) *sqlmock.Rows {
	r := sqlmock.NewRows([]string{"id", "name", "type", "icon", "url", "is_enabled", "is_system", "position", "access_role"})
	for i, n := range names {
		r.AddRow(i+1, n, "internal", "x", "/"+n, true, true, i+1, "all")
	}
	return r
}

// One query per second; the caller gets a copy it may filter in place (the
// modules handler does), a write drops the cache, and FreshModules always asks.
func TestListModulesCache(t *testing.T) {
	now := fakeClock(t)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewPostgresStore(db)
	mock.ExpectQuery(regexp.QuoteMeta(listModulesQ)).WillReturnRows(moduleRows("Servers", "Admin"))

	first, err := s.ListModules()
	if err != nil || len(first) != 2 {
		t.Fatalf("first: %v %v", first, err)
	}
	first[0].Name = "mutated"
	second, _ := s.ListModules()
	if len(second) != 2 || second[0].Name != "Servers" {
		t.Fatalf("a caller's edit leaked into the cache: %+v", second)
	}

	mock.ExpectExec("UPDATE modules SET is_enabled").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := s.UpdateModuleStatus(2, false); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(listModulesQ)).WillReturnRows(moduleRows("Servers"))
	if got, _ := s.ListModules(); len(got) != 1 {
		t.Fatalf("after own write: %+v, want the reloaded list", got)
	}

	*now = now.Add(time.Second)
	mock.ExpectQuery(regexp.QuoteMeta(listModulesQ)).WillReturnRows(moduleRows("Servers", "Tickets"))
	if got, _ := s.ListModules(); len(got) != 2 {
		t.Fatalf("after the second: %+v", got)
	}

	mock.ExpectQuery(regexp.QuoteMeta(listModulesQ)).WillReturnRows(moduleRows("Servers"))
	if got, _ := FreshModules(s); len(got) != 1 {
		t.Fatalf("FreshModules served the cache: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
