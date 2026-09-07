package services

import (
	"context"
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
	args := append(c.args(), "--format=custom", "--no-owner", "--no-acl", "--compress=6")

	cmd := exec.CommandContext(ctx, bin, args...)
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
	bin, err := PGToolPath("pg_restore", serverMajor)
	if err != nil {
		return "", err
	}
	args := append(c.args(), "--clean", "--if-exists", "--no-owner", "--no-acl", "--single-transaction")

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
