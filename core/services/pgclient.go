package services

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Choosing which installed Postgres client to run.
//
// The image installs more than one because the version constraint points both
// ways at once (see pgdump.go): pg_dump needs to be at least as new as the
// server, pg_restore needs to be no newer than it. The only client that
// satisfies both for a given server is one of the SAME major version, so the
// rule is: take the smallest installed client that is at least as new as the
// server, which is that server's own major whenever it is installed.
//
// The clients are DISCOVERED rather than listed, so adding one to the Dockerfile
// needs no change here - the failure mode of a hardcoded list is a client that
// is installed and never used, which nothing would report.

// pgClientRoot is where the alpine packages put versioned client binaries
// (/usr/libexec/postgresql16, .../postgresql18). A variable so a test can point
// it somewhere else.
var pgClientRoot = "/usr/libexec"

type pgClient struct {
	major int
	dir   string
}

// discoverPGClients returns the installed clients, oldest first.
func discoverPGClients(root string) []pgClient {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []pgClient
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rest, ok := strings.CutPrefix(e.Name(), "postgresql")
		if !ok {
			continue
		}
		major, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}
		out = append(out, pgClient{major: major, dir: filepath.Join(root, e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].major < out[j].major })
	return out
}

// PGToolPath returns the path to tool ("pg_dump", "pg_restore", "psql") for a
// server of the given major version.
//
// serverMajor 0 means "not known", which falls back to whatever is on PATH.
// That is the developer's machine, where one client is installed the ordinary
// way and the versioned directories do not exist.
func PGToolPath(tool string, serverMajor int) (string, error) {
	clients := discoverPGClients(pgClientRoot)

	if serverMajor > 0 {
		for _, c := range clients {
			if c.major < serverMajor {
				continue
			}
			p := filepath.Join(c.dir, tool)
			if isExecutable(p) {
				return p, nil
			}
		}
		// Every installed client is older than the server. pg_dump would refuse
		// outright, so say which versions are here rather than let the tool
		// fail with its own wording.
		if len(clients) > 0 {
			return "", &ErrPGToolMissing{
				Tool: fmt.Sprintf("%s for PostgreSQL %d (this image has %s)", tool, serverMajor, majorList(clients)),
			}
		}
	}

	if p, err := exec.LookPath(tool); err == nil {
		return p, nil
	}
	// A single unversioned install, which is how a workstation looks.
	for _, c := range clients {
		p := filepath.Join(c.dir, tool)
		if isExecutable(p) {
			return p, nil
		}
	}
	return "", &ErrPGToolMissing{Tool: tool}
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func majorList(clients []pgClient) string {
	parts := make([]string, 0, len(clients))
	for _, c := range clients {
		parts = append(parts, strconv.Itoa(c.major))
	}
	return strings.Join(parts, ", ")
}

// PGServerMajor asks a database what major version it is.
//
// Asked rather than configured: the version is a property of the running
// server, and an operator who upgrades Postgres does not come back to edit a
// setting about it.
func PGServerMajor(db *sql.DB) (int, error) {
	var num int
	if err := db.QueryRow(`SHOW server_version_num`).Scan(&num); err != nil {
		return 0, fmt.Errorf("reading the server version: %w", err)
	}
	// 150008 is 15.8; 90624 was 9.6.24. Everything this platform supports is
	// well past the two-part era, so the plain division is right.
	return num / 10000, nil
}
