package services

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"dylaris-core/store"
)

// Where the long-term statistics are written, as an operator configures it.
//
// The settings table is the ONLY answer. There was an environment variable too,
// and it won where it was set - which meant the same question had two sources
// and the panel could show a target that was not the one being written. For a
// setting whose wrong value silently changes what is being written, one source
// is worth more than the convenience of declaring it in a stack file.
//
// There is also only one KIND of answer now. Recording into the Core database
// at hour resolution used to be the alternative, selected by a mode field; it
// is gone, along with the mode. A target is either named here, in which case it
// is a database of its own, or it is not named and nothing is recorded.
// `metrics_db_mode` may still exist as a stale row on an installation that
// predates this - it is no longer read and no longer written.
const (
	MetricsDBHostSetting     = "metrics_db_host"
	MetricsDBPortSetting     = "metrics_db_port"
	MetricsDBNameSetting     = "metrics_db_name"
	MetricsDBUserSetting     = "metrics_db_user"
	MetricsDBPasswordSetting = "metrics_db_password"
	MetricsDBSSLModeSetting  = "metrics_db_sslmode"
)

// MetricsDBTarget is the panel-configurable half. An empty Host means no
// target is configured, which is how recording is switched off.
type MetricsDBTarget struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	DBName   string `json:"dbName"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	SSLMode  string `json:"sslMode"`
}

// Configured reports whether a database is named at all. Nothing else in this
// file needs to know more than that: a named target is a database of its own,
// and an unnamed one means nothing is recorded.
func (t MetricsDBTarget) Configured() bool {
	return strings.TrimSpace(t.Host) != ""
}

// Normalize trims every field and fills the defaults a form leaves empty.
func (t MetricsDBTarget) Normalize() MetricsDBTarget {
	t.Host = strings.TrimSpace(t.Host)
	t.Port = strings.TrimSpace(t.Port)
	t.DBName = strings.TrimSpace(t.DBName)
	t.User = strings.TrimSpace(t.User)
	t.SSLMode = strings.TrimSpace(t.SSLMode)
	if t.Port == "" {
		t.Port = "5432"
	}
	if t.SSLMode == "" {
		// Matches the rest of this platform's in-Docker connections, and is what
		// a service name like `metricsdb` on an overlay can actually offer.
		t.SSLMode = "disable"
	}
	// The password is deliberately NOT trimmed: a trailing space is a legal
	// character in one, and silently removing it produces an auth failure the
	// operator cannot see in the form.
	return t
}

// Validate reports what a form is missing.
//
// An unconfigured target is valid and means "record nothing" - that is how
// recording is switched off, and refusing to save it would leave an operator
// unable to stop. The password is optional on purpose: a database reached over
// a private network can legitimately have none, which is how the reference
// deployment runs it.
func (t MetricsDBTarget) Validate() error {
	if !t.Configured() {
		return nil
	}
	if t.DBName == "" {
		return fmt.Errorf("a database name is required")
	}
	if t.User == "" {
		return fmt.Errorf("a user is required")
	}
	if p, err := strconv.Atoi(t.Port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("port must be a number between 1 and 65535")
	}
	return nil
}

// DSN renders what metrics.Open takes. Empty means there is nothing to open,
// which the manager treats as "record nothing" - the same thing the panel says
// when no database is configured, so there is no second way of saying it.
//
// lib/pq accepts this keyword form as readily as a URL, and it is the form the
// DB-migration screen already uses - one less place where a password has to be
// percent-encoded correctly.
func (t MetricsDBTarget) DSN() string {
	if !t.Configured() {
		return ""
	}
	n := t.Normalize()
	return DBConnParams{
		Host: n.Host, Port: n.Port, User: n.User,
		Password: n.Password, DBName: n.DBName, SSLMode: n.SSLMode,
	}.DSN()
}

// LoadMetricsDBTarget reads the stored target, password included.
//
// A missing row is not an error and not a fault: it means nobody has configured
// this, and nothing is recorded. That is the same shape as every other unset
// setting here.
func LoadMetricsDBTarget(st store.Store) MetricsDBTarget {
	get := func(k string) string {
		v, _ := st.GetSetting(k)
		return v
	}
	return MetricsDBTarget{
		Host:     get(MetricsDBHostSetting),
		Port:     get(MetricsDBPortSetting),
		DBName:   get(MetricsDBNameSetting),
		User:     get(MetricsDBUserSetting),
		Password: get(MetricsDBPasswordSetting),
		SSLMode:  get(MetricsDBSSLModeSetting),
	}.Normalize()
}

// SaveMetricsDBTarget persists it. The password is written like any other field
// here; what protects it is that the GET never emits it.
func SaveMetricsDBTarget(st store.Store, t MetricsDBTarget) error {
	n := t.Normalize()
	for _, kv := range []struct{ k, v string }{
		{MetricsDBHostSetting, n.Host},
		{MetricsDBPortSetting, n.Port},
		{MetricsDBNameSetting, n.DBName},
		{MetricsDBUserSetting, n.User},
		{MetricsDBPasswordSetting, n.Password},
		{MetricsDBSSLModeSetting, n.SSLMode},
	} {
		if err := st.SetSetting(kv.k, kv.v); err != nil {
			return fmt.Errorf("save %s: %w", kv.k, err)
		}
	}
	return nil
}

// MetricsDBProbe is what a test button learns about a target.
type MetricsDBProbe struct {
	Reachable bool   `json:"reachable"`
	Timescale bool   `json:"timescale"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
	// Stage says which of the two steps failed - see services/conncheck.go.
	// A host that never answered and a host that rejected the password are
	// fixed in different places, and "could not connect" hid that difference.
	Stage string `json:"stage,omitempty"`
}

// probeTimeout bounds the test button. Long enough for a cold container to
// answer, short enough that a wrong host does not look like a hung panel.
const probeTimeout = 8 * time.Second

// ProbeMetricsDB opens the target, asks it what it is, and closes it again.
//
// It reports rather than judges: whether the extension is there decides how the
// data is STORED, not whether the target works, and the two mistakes an operator
// can make here have opposite consequences. See the handler for which of them is
// worth refusing a save over.
//
// Reachability comes first and separately. A host that never answered says
// nothing about the password, and a probe that reported both as one failure
// sent operators to retype credentials that were correct all along.
func ProbeMetricsDB(ctx context.Context, t MetricsDBTarget) MetricsDBProbe {
	n := t.Normalize()
	if reach := Reachable(ctx, n.Host, n.Port); !reach.OK {
		return MetricsDBProbe{Stage: StageUnreachable, Error: reach.Message}
	}
	db, err := DBConnParams{
		Host: n.Host, Port: n.Port, User: n.User,
		Password: n.Password, DBName: n.DBName, SSLMode: n.SSLMode,
	}.Open(ctx, probeTimeout)
	if err != nil {
		return MetricsDBProbe{Stage: StageRejected, Error: DescribePostgresError(err)}
	}
	defer db.Close()
	return MetricsDBProbe{
		Reachable: true,
		Stage:     StageOK,
		Timescale: probeTimescale(ctx, db),
		Version:   probeVersion(ctx, db),
	}
}

// probeTimescale asks whether the extension is INSTALLED in this database, not
// whether the server could install it. Available-but-absent is the state a
// Supabase Postgres is in, and it is the one that silently produces a plain
// table.
func probeTimescale(ctx context.Context, db *sql.DB) bool {
	q, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var ok bool
	if err := db.QueryRowContext(q,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).Scan(&ok); err != nil {
		return false
	}
	return ok
}

func probeVersion(ctx context.Context, db *sql.DB) string {
	q, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	var v string
	if err := db.QueryRowContext(q, `SHOW server_version`).Scan(&v); err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}
