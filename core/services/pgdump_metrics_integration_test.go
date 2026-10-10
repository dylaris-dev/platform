package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// The documented TimescaleDB dump/restore procedure against a real server.
//
// Skipped without DYLARIS_TEST_TIMESCALE_HOST: the CI db-tests job runs plain
// Postgres, so this needs a TimescaleDB server with pg_dump and pg_restore on
// the PATH (the timescale/timescaledb image has both), connected as a superuser.
func TestMetricsDBDumpAndRestoreOnTimescale(t *testing.T) {
	host := os.Getenv("DYLARIS_TEST_TIMESCALE_HOST")
	if host == "" {
		t.Skip("DYLARIS_TEST_TIMESCALE_HOST not set")
	}
	ctx := context.Background()
	base := DBConnParams{Host: host, Port: os.Getenv("DYLARIS_TEST_TIMESCALE_PORT"),
		User: os.Getenv("DYLARIS_TEST_TIMESCALE_USER"), Password: os.Getenv("DYLARIS_TEST_TIMESCALE_PASSWORD"),
		DBName: "postgres", SSLMode: "disable"}
	if base.Port == "" {
		base.Port = "5432"
	}
	admin, err := base.Open(ctx, 10*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer admin.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	src, dst, bare := "msrc_"+suffix, "mdst_"+suffix, "mbare_"+suffix
	for _, n := range []string{src, dst, bare} {
		// template0: the timescale image puts the extension into template1,
		// and the bare target must not have it.
		if _, err := admin.Exec("CREATE DATABASE " + n + " TEMPLATE template0"); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
		defer admin.Exec("DROP DATABASE IF EXISTS " + n + " WITH (FORCE)")
	}
	open := func(name string) DBConnParams { p := base; p.DBName = name; return p }

	sdb, err := open(src).Open(ctx, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	for _, q := range []string{
		`CREATE EXTENSION IF NOT EXISTS timescaledb`,
		`CREATE TABLE metric_samples (time timestamptz NOT NULL, server_id int, v double precision)`,
		`SELECT create_hypertable('metric_samples', 'time', chunk_time_interval => INTERVAL '1 day')`,
		`INSERT INTO metric_samples SELECT now() - (i || ' hours')::interval, i % 3, i FROM generate_series(1, 200) i`,
	} {
		if _, err := sdb.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	var dump bytes.Buffer
	target := MetricsDBTarget{Host: base.Host, Port: base.Port, User: base.User, Password: base.Password, DBName: src}
	version, err := DumpMetricsDB(ctx, target, &dump)
	if err != nil {
		t.Fatalf("DumpMetricsDB: %v", err)
	}
	if version == "" || dump.Len() == 0 {
		t.Fatalf("version %q, %d bytes", version, dump.Len())
	}

	// A target without the extension is refused before anything is written.
	bdb, err := open(bare).Open(ctx, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer bdb.Close()
	major, _ := PGServerMajor(bdb)
	if err := RestoreMetricsDB(ctx, bdb, open(bare).PG(), major, bytes.NewReader(dump.Bytes()), version); !errors.Is(err, ErrMetricsTargetNotReady) {
		t.Fatalf("bare target: err = %v, want ErrMetricsTargetNotReady", err)
	}

	ddb, err := open(dst).Open(ctx, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ddb.Close()
	if _, err := ddb.Exec(`CREATE EXTENSION IF NOT EXISTS timescaledb VERSION '` + version + `'`); err != nil {
		t.Fatalf("extension: %v", err)
	}
	if err := CheckMetricsRestoreTarget(ctx, ddb); err != nil {
		t.Fatalf("CheckMetricsRestoreTarget: %v", err)
	}
	if err := RestoreMetricsDB(ctx, ddb, open(dst).PG(), major, bytes.NewReader(dump.Bytes()), version); err != nil {
		t.Fatalf("RestoreMetricsDB: %v", err)
	}

	var rows, hypertables, chunks int
	if err := ddb.QueryRow(`SELECT count(*) FROM metric_samples`).Scan(&rows); err != nil || rows != 200 {
		t.Errorf("rows = %d, err %v; want 200", rows, err)
	}
	ddb.QueryRow(`SELECT count(*) FROM timescaledb_information.hypertables WHERE hypertable_name = 'metric_samples'`).Scan(&hypertables)
	ddb.QueryRow(`SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_name = 'metric_samples'`).Scan(&chunks)
	if hypertables != 1 || chunks == 0 {
		t.Errorf("hypertables = %d, chunks = %d: the restore lost the hypertable", hypertables, chunks)
	}
	// post_restore must have run: a database left in restoring mode has its
	// background jobs stopped. A fresh session reads the database-level setting.
	fresh, err := open(dst).Open(ctx, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	var restoring string
	fresh.QueryRow(`SHOW timescaledb.restoring`).Scan(&restoring)
	if restoring != "off" {
		t.Errorf("timescaledb.restoring = %q after the restore, want off", restoring)
	}
	// A new insert lands in the restored hypertable.
	if _, err := fresh.Exec(`INSERT INTO metric_samples VALUES (now() + interval '3 days', 1, 1)`); err != nil {
		t.Errorf("insert after restore: %v", err)
	}
	// The target now holds tables, so a second restore is refused up front.
	if err := CheckMetricsRestoreTarget(ctx, ddb); !errors.Is(err, ErrMetricsTargetNotReady) {
		t.Errorf("non-empty target: err = %v", err)
	}
}
