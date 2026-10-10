package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// pg_dump and pg_restore, shelled out.
//
// Not reimplemented in Go, and that is the whole point: a hand-rolled logical
// dump gets the easy 90% - tables and rows - and then quietly loses sequences,
// constraint ordering, extensions and custom types. The tool that already knows
// all of that ships in the image (see the Dockerfile, Core only).
//
// Which client is a real decision, because the constraint is ASYMMETRIC and was
// measured rather than reasoned about:
//
//	pg_dump    refuses a server NEWER than itself.
//	pg_restore emits SQL the target has to understand, so a client NEWER than
//	           the target fails. Measured: pg_restore 17 and 18 write
//	           "SET transaction_timeout = 0", a parameter PostgreSQL 15 does
//	           not have, and the restore stops on it.
//
// So no single client serves both a PG15 platform database and a self-hoster on
// PG18. The image installs several, and PGToolPath picks the smallest one that
// is at least as new as the server it is talking to.

// PGConn is what it takes to reach a database. Deliberately not the whole
// config: this runs an external program, and every field here ends up in an
// argument list or an environment variable.
type PGConn struct {
	Host     string
	Port     string
	User     string
	Password string
	Name     string
	SSLMode  string
}

// PG turns a panel-supplied connection into what the external tools need.
//
// One shape for a database connection in this package, rather than two that
// drift: DBConnParams is what the panel form produces and what database/sql
// opens, PGConn is the same thing as arguments and environment for a subprocess.
func (p DBConnParams) PG() PGConn {
	return PGConn{
		Host: p.Host, Port: p.Port, User: p.User,
		Password: p.Password, Name: p.DBName, SSLMode: p.SSLMode,
	}
}

// ErrPGToolMissing is an image without the client. Reported as its own thing so
// an operator is told to update Core rather than shown a shell error.
type ErrPGToolMissing struct{ Tool string }

func (e *ErrPGToolMissing) Error() string {
	return fmt.Sprintf("%s is not installed in this Core image; platform database backups need it", e.Tool)
}

func (c PGConn) args() []string {
	args := []string{"--host=" + c.Host, "--username=" + c.User, "--dbname=" + c.Name}
	if c.Port != "" {
		args = append(args, "--port="+c.Port)
	}
	return args
}

// env passes the password out of band.
//
// Never as an argument: an argument list is world-readable in /proc on the
// container's own host, so a --password flag hands the platform database
// credential to anything that can read a process list.
func (c PGConn) env() []string {
	env := append(os.Environ(), "PGPASSWORD="+c.Password)
	if c.SSLMode != "" {
		env = append(env, "PGSSLMODE="+c.SSLMode)
	}
	return env
}

// dumpArgs is the pg_dump argument list.
//
// --lock-wait-timeout because pg_dump takes ACCESS SHARE on every table, and a
// table already held exclusively - a TimescaleDB compress_chunk, a migration -
// would otherwise park the dump, and the backup run with it, forever. Failing
// after a minute is a run the operator sees as failed, not one that never ends.
func (c PGConn) dumpArgs() []string {
	return append(c.args(), "--format=custom", "--no-owner", "--no-acl", "--compress=6", "--lock-wait-timeout=60s")
}

// DumpDatabase writes a custom-format dump of the whole database to dest.
//
// Custom format rather than plain SQL: it restores through pg_restore, which
// can drop and recreate objects in dependency order. A plain-SQL dump piped
// into psql stops at the first error and leaves a half-applied database, which
// is the worst possible state for a restore to end in.
//
// --no-owner and --no-acl because a bundle is restored onto a DIFFERENT
// installation, whose database roles are not the source's. Preserving ownership
// there means every statement failing on a role that does not exist.
func DumpDatabase(ctx context.Context, c PGConn, serverMajor int, dest io.Writer) error {
	bin, err := PGToolPath("pg_dump", serverMajor)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, c.dumpArgs()...)
	cmd.Env = c.env()
	cmd.Stdout = dest
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump: %w: %s", err, lastLines(stderr.String()))
	}
	return nil
}

// RestoreDatabase loads a custom-format dump into the target database,
// replacing what is there.
//
// --clean --if-exists drops each object before recreating it, so a restore onto
// a database that already has a schema - which is every restore, because Core
// creates its schema at boot - does not fail on every CREATE.
//
// --single-transaction because a half-restored database is the worst state a
// restore can end in: some tables from the bundle, some from whatever was there
// before, and nothing saying which is which. Either the whole dump applies or
// the database is exactly as it was. Note that this implies exit-on-error, so a
// restore that cannot drop something it was asked to replace fails loudly
// rather than continuing past it.
func RestoreDatabase(ctx context.Context, c PGConn, serverMajor int, src io.Reader) (string, error) {
	return runPGRestore(ctx, c, serverMajor, src, "--clean", "--if-exists")
}

func runPGRestore(ctx context.Context, c PGConn, serverMajor int, src io.Reader, extra ...string) (string, error) {
	bin, err := PGToolPath("pg_restore", serverMajor)
	if err != nil {
		return "", err
	}
	// Never -j: besides being incompatible with --single-transaction, TimescaleDB
	// documents that a parallel restore does not restore its catalogs correctly.
	args := append(c.args(), extra...)
	args = append(args, "--no-owner", "--no-acl", "--single-transaction")

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = c.env()
	cmd.Stdin = src
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stderr.String(), fmt.Errorf("pg_restore: %w: %s", err, lastLines(stderr.String()))
	}
	return stderr.String(), nil
}

// lastLines trims a tool's stderr to something that fits in an error message
// without losing the part that says what went wrong, which is the end.
func lastLines(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return strings.Join(lines, "; ")
}

// The statistics database.
//
// It is TimescaleDB, and a logical dump of it is an ordinary pg_dump: hypertable
// chunks are plain tables in _timescaledb_internal and the extension's catalog
// is dumped as extension configuration. What is NOT ordinary is the restore,
// which TimescaleDB documents as: the same extension version already created in
// the target, SELECT timescaledb_pre_restore(), pg_restore without -j, then
// SELECT timescaledb_post_restore(). Restoring the dump any other way fails or,
// worse, produces hypertables whose catalog does not match their chunks.
// https://docs.timescale.com/self-hosted/latest/backup-and-restore/logical-backup

// TimescaleVersion is the installed timescaledb extension version, or "" when
// this database does not have it.
func TimescaleVersion(ctx context.Context, db *sql.DB) (string, error) {
	var v string
	err := db.QueryRowContext(ctx,
		`SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the TimescaleDB version: %w", err)
	}
	return v, nil
}

func (t MetricsDBTarget) params() DBConnParams {
	n := t.Normalize()
	return DBConnParams{
		Host: n.Host, Port: n.Port, User: n.User,
		Password: n.Password, DBName: n.DBName, SSLMode: n.SSLMode,
	}
}

// DumpMetricsDB writes a custom-format dump of the statistics database to dest
// and returns the TimescaleDB version it was taken from ("" for a plain
// Postgres target). The version travels with the dump because a restore has to
// match it and the dump alone does not say it in a form a restore can check
// before it starts.
func DumpMetricsDB(ctx context.Context, t MetricsDBTarget, dest io.Writer) (string, error) {
	p := t.params()
	db, err := p.Open(ctx, probeTimeout)
	if err != nil {
		return "", fmt.Errorf("statistics database: %s", DescribePostgresError(err))
	}
	defer db.Close()
	major, err := PGServerMajor(db)
	if err != nil {
		return "", err
	}
	v, err := TimescaleVersion(ctx, db)
	if err != nil {
		return "", err
	}
	return v, DumpDatabase(ctx, p.PG(), major, dest)
}

// ErrMetricsTargetNotReady is a statistics restore target that cannot take the
// dump. Answered before anything is written.
var ErrMetricsTargetNotReady = errors.New("the target statistics database is not ready for this restore")

// CheckMetricsRestoreTarget refuses a target the TimescaleDB procedure cannot
// run on: a role that is not a superuser (timescaledb_pre_restore alters the
// database and the dump rewrites the extension's own catalog) or a database that
// already holds tables (there is no --clean here; dropping the extension's
// objects under timescaledb_pre_restore is not a documented path).
func CheckMetricsRestoreTarget(ctx context.Context, db *sql.DB) error {
	var super bool
	if err := db.QueryRowContext(ctx,
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		return fmt.Errorf("checking the target role: %w", err)
	}
	if !super {
		return fmt.Errorf("%w: connect as a superuser; restoring a TimescaleDB database rewrites the extension's catalog, which an ordinary role cannot", ErrMetricsTargetNotReady)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public'`).Scan(&n); err != nil {
		return fmt.Errorf("checking the target database: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: it already holds %d tables; restore into a new, empty database", ErrMetricsTargetNotReady, n)
	}
	return nil
}

// RestoreMetricsDB loads a statistics dump into db, following the documented
// TimescaleDB procedure when the dump came from TimescaleDB.
//
// timescaledb_post_restore runs whatever pg_restore did: --single-transaction
// means a failed restore left the target as it was, and a target left with
// timescaledb.restoring on has its background jobs stopped indefinitely.
func RestoreMetricsDB(ctx context.Context, db *sql.DB, c PGConn, serverMajor int, src io.Reader, sourceVersion string) (err error) {
	if sourceVersion != "" {
		target, verr := TimescaleVersion(ctx, db)
		if verr != nil {
			return verr
		}
		if target == "" {
			return fmt.Errorf("%w: the dump is from TimescaleDB %s; run CREATE EXTENSION timescaledb VERSION '%s' in the target first",
				ErrMetricsTargetNotReady, sourceVersion, sourceVersion)
		}
		if target != sourceVersion {
			return fmt.Errorf("%w: the dump is from TimescaleDB %s but the target has %s; TimescaleDB restores only into the same version",
				ErrMetricsTargetNotReady, sourceVersion, target)
		}
		if _, perr := db.ExecContext(ctx, `SELECT timescaledb_pre_restore()`); perr != nil {
			return fmt.Errorf("timescaledb_pre_restore: %w", perr)
		}
		defer func() {
			// Not the request's context: a cancelled restore must still put the
			// target back into normal operation.
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
			defer cancel()
			if _, perr := db.ExecContext(pctx, `SELECT timescaledb_post_restore()`); perr != nil && err == nil {
				err = fmt.Errorf("timescaledb_post_restore: %w", perr)
			}
		}()
	}
	_, err = runPGRestore(ctx, c, serverMajor, src)
	return err
}
