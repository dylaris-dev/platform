package database

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every DATABASE-BACKED test in this package must actually RUN in CI.
//
// The db-tests job does not run the package, it runs a NAME FILTER over it
// (.github/db.Dockerfile: `go test -run Integration|Prepares|FreshInstall
// ./database/`). The go-tests matrix runs the package too, but with no
// database, where a test that asks for one skips itself.
//
// So a test that NEEDS a database and whose name misses the filter runs
// NOWHERE: skipped in the matrix for want of one, never selected by the job
// that has one. It passes locally, it is green in CI, and it has never
// executed. Eleven tests had landed in exactly that state before this existed.
//
// Which tests need one is decided by what they CALL - integrationDB or
// freshSchemaDB - not by what file they sit in or how they are named. A test
// that needs no database is none of this test's business.
//
// The filter is read out of the Dockerfile rather than restated here, because a
// second copy of a pattern is a second thing to keep in step, and the failure
// it causes is silence.
func TestEveryDatabaseBackedTestIsSelectedByCI(t *testing.T) {
	pattern := ciTestFilter(t)
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("the CI filter %q is not a valid expression: %v", pattern, err)
	}

	names := databaseBackedTests(t, ".")
	if len(names) == 0 {
		// A scan that matches nothing would let this test pass forever while
		// checking nothing at all.
		t.Fatal("found no database-backed tests; the scan is broken, not the code")
	}

	var missed []string
	for _, n := range names {
		if !re.MatchString(n) {
			missed = append(missed, n)
		}
	}
	if len(missed) > 0 {
		t.Errorf("these tests do not match the db-tests filter %q and therefore never run anywhere:\n  %s\n"+
			"Rename them (the convention is a TestIntegration... prefix) or widen the filter in .github/db.Dockerfile.",
			pattern, strings.Join(missed, "\n  "))
	}
}

// ciTestFilter reads the -run pattern out of the image the db-tests job builds.
func ciTestFilter(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "db.Dockerfile")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	// CMD ["go", "test", "-count=1", "-v", "-run", "<pattern>", "./database/"]
	m := regexp.MustCompile(`"-run",\s*"([^"]+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("no -run pattern found in %s; the db-tests job no longer filters by name, "+
			"or its CMD changed shape and this guard is now blind", path)
	}
	return string(m[1])
}

// databaseBackedTests returns the tests that ask for a real Postgres, by
// looking at what each one's body calls.
func databaseBackedTests(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	decl := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)

	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		// This file names both helpers in string literals and would otherwise
		// report itself. It needs no database of its own.
		if e.Name() == "ci_runs_these_tests_test.go" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		src := string(raw)
		locs := decl.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			body := src[loc[0]:end]
			if strings.Contains(body, "integrationDB(") || strings.Contains(body, "freshSchemaDB(") {
				out = append(out, src[loc[2]:loc[3]])
			}
		}
	}
	return out
}
