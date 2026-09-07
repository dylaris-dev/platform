package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClients builds a root that looks like the image: versioned directories
// holding the tools.
func fakeClients(t *testing.T, majors ...int) string {
	t.Helper()
	root := t.TempDir()
	for _, m := range majors {
		dir := filepath.Join(root, "postgresql"+itoa(m))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		for _, tool := range []string{"pg_dump", "pg_restore", "psql"} {
			if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
	// Something that is not a client, to prove the scan is not just listing.
	if err := os.MkdirAll(filepath.Join(root, "not-postgres"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "postgresqlnope"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func withClientRoot(t *testing.T, root string) {
	t.Helper()
	old := pgClientRoot
	pgClientRoot = root
	t.Cleanup(func() { pgClientRoot = old })
}

// The rule the whole two-client install exists for: the SMALLEST client that is
// at least as new as the server. Too old and pg_dump refuses the server; too
// new and pg_restore writes SQL the server does not understand - measured on
// PostgreSQL 15, which does not know transaction_timeout.
func TestPGToolPathPicksTheSmallestClientThatIsNewEnough(t *testing.T) {
	withClientRoot(t, fakeClients(t, 16, 18))

	cases := []struct {
		server int
		want   string
	}{
		{15, "postgresql16"}, // the platform database in production
		{16, "postgresql16"}, // the metrics database in production
		{17, "postgresql18"},
		{18, "postgresql18"},
	}
	for _, tc := range cases {
		got, err := PGToolPath("pg_dump", tc.server)
		if err != nil {
			t.Fatalf("server %d: %v", tc.server, err)
		}
		if !strings.Contains(filepath.ToSlash(got), tc.want) {
			t.Errorf("server %d picked %q, want a %s client", tc.server, got, tc.want)
		}
	}
}

// A server newer than everything installed has to be REPORTED. pg_dump would
// refuse it anyway, and its own message says nothing about which versions this
// image actually has.
func TestPGToolPathReportsAServerNewerThanEveryClient(t *testing.T) {
	withClientRoot(t, fakeClients(t, 16, 18))

	_, err := PGToolPath("pg_dump", 19)
	if err == nil {
		t.Fatal("a server newer than every client was accepted")
	}
	msg := err.Error()
	for _, want := range []string{"19", "16", "18"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q: %s", want, msg)
		}
	}
}

// Adding a client to the Dockerfile must not need a change in Go. A hardcoded
// list fails by leaving an installed client unused, which nothing reports.
func TestPGToolPathFindsAClientNobodyListed(t *testing.T) {
	withClientRoot(t, fakeClients(t, 21))

	got, err := PGToolPath("pg_restore", 21)
	if err != nil {
		t.Fatalf("PGToolPath: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(got), "postgresql21") {
		t.Errorf("got %q", got)
	}
}

func TestDiscoverPGClientsIgnoresWhatIsNotAClient(t *testing.T) {
	got := discoverPGClients(fakeClients(t, 18, 16))
	if len(got) != 2 {
		t.Fatalf("found %d clients, want 2: %+v", len(got), got)
	}
	// Oldest first, because the search wants the smallest match.
	if got[0].major != 16 || got[1].major != 18 {
		t.Errorf("order = %d, %d; want 16, 18", got[0].major, got[1].major)
	}
}

func TestDiscoverPGClientsOnAMachineWithNone(t *testing.T) {
	if got := discoverPGClients(filepath.Join(t.TempDir(), "does-not-exist")); got != nil {
		t.Fatalf("got %+v, want nothing", got)
	}
}

// A workstation has one client installed the ordinary way and no versioned
// directories at all. Backups there are not the point, but every OTHER caller
// of this must keep working.
func TestPGToolPathFallsBackToThePathWhenNothingIsVersioned(t *testing.T) {
	withClientRoot(t, t.TempDir())

	// "go" stands in for a tool that is certainly on PATH in any environment
	// that can run this test.
	if _, err := PGToolPath("go", 0); err != nil {
		t.Fatalf("a tool on PATH was not found: %v", err)
	}
	if _, err := PGToolPath("definitely-not-a-real-tool", 0); err == nil {
		t.Fatal("a tool that exists nowhere was reported as found")
	}
}
