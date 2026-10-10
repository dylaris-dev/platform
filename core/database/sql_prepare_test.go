package database

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every SQL statement in the module has to be preparable against the real
// schema. PREPARE parses the statement, resolves every table and column and
// deduces the parameter types - without executing anything - so this catches
// the whole class of defects that no fake can see and that only surface as a
// generic 500 in production:
//
//   - "inconsistent types deduced for parameter $1", which is how setting a
//     user's role and closing a ticket had never worked;
//   - a column or table name that does not exist, which is a typo away at any
//     time and which the fakes happily accept.
//
// TestNoParameterIsBothAssignedAndComparedToALiteral in core/store catches the
// first shape statically, at authoring time. This is the dynamic counterpart:
// it needs a database, but it makes no assumptions about the shape of the
// mistake.
//
// It runs against a database the schema has been built on EXACTLY ONCE, which
// is what a fresh install is. That is not a detail: the ADD COLUMN set in
// migrateSchema runs before the later phases create their tables, so a column
// whose table does not exist yet is missing until the next boot - and a
// database this suite has already booted a few times no longer shows it. This
// test found modversions.modrinth_download_url exactly that way, and would have
// missed it on a shared database.
//
// Skipped unless DYLARIS_TEST_DB_HOST is set. See postgres_integration_test.go
// for the local run recipe.
func TestEverySQLStatementPrepares(t *testing.T) {
	db := freshSchemaDB(t)

	stmts := collectSQLLiterals(t, []string{"../store", "../handlers", "../services", "../database"})
	if len(stmts) < 100 {
		t.Fatalf("only %d statements collected - the extraction is broken, not the SQL", len(stmts))
	}

	var failures, skipped []string
	prepared := 0
	for _, s := range stmts {
		if why := skipReason(s.sql); why != "" {
			skipped = append(skipped, s.where+": "+why)
			continue
		}
		stmt, err := db.Prepare(s.sql)
		if err == nil {
			stmt.Close()
			prepared++
			continue
		}
		// Postgres cannot always infer a parameter's type from the statement
		// alone; at runtime the driver supplies one from the Go value. That is
		// not a defect, so it is reported as a skip rather than swallowed.
		if isIndeterminateParam(err) {
			skipped = append(skipped, s.where+": "+firstLine(err.Error()))
			continue
		}
		failures = append(failures, s.where+"\n      "+firstLine(err.Error())+"\n      "+oneLine(s.sql))
	}

	t.Logf("prepared %d statements, skipped %d (reasons below)", prepared, len(skipped))
	for _, s := range skipped {
		t.Logf("  skipped: %s", s)
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		t.Errorf("%d statement(s) the database refuses to prepare:\n    %s",
			len(failures), strings.Join(failures, "\n    "))
	}
}

type sqlLiteral struct {
	where string
	sql   string
}

var (
	backtickLiteral = regexp.MustCompile("(?s)`([^`]*)`")
	looksLikeSQL    = regexp.MustCompile(`(?is)^\s*(SELECT|INSERT\s+INTO|UPDATE|DELETE\s+FROM|WITH)\b`)
	// A literal that is only part of a statement (built with + at the call
	// site) cannot be prepared on its own. A trailing operator or a trailing
	// FROM/INTO/JOIN is the tell; those fragments are dropped, and the count of
	// what survived is asserted above so a broken extraction cannot pass
	// silently.
	//
	// NOT is in the list because "AND NOT " is not caught by the AND alternative
	// - the word boundary lands on NOT, not on AND - so a query ending in it was
	// handed to Prepare as if it were whole and failed with a syntax error at
	// end of input. The two link-kit reconcile queries share their WHERE
	// predicate through a const, for the same reason they must never disagree,
	// and that shape is what exposed the gap. Both are covered by
	// TestOwnerCutOffMatchesItsSQLIntegration, which RUNS them rather than
	// preparing them.
	danglingFragment = regexp.MustCompile(`(?is)(,|\(|\bAND\b|\bOR\b|\bNOT\b|\bWHERE\b|\bSET\b|\bVALUES\b|\bFROM\b|\bINTO\b|\bJOIN\b)\s*$`)
	// TimescaleDB's own functions and catalog. Absent by design here: the suite
	// runs plain postgres so ensureSchema takes the ordinary-table path.
	timescaleOnly = regexp.MustCompile(`(?i)(create_hypertable|add_retention_policy|timescaledb_information|timescaledb_pre_restore|timescaledb_post_restore)`)
	// Statements assembled with fmt.Sprintf are templates, not SQL.
	formatTemplate = regexp.MustCompile(`%[sdqv]`)
)

// skipReason explains why a statement cannot be prepared in this environment,
// or "" when it has to be. Every skip is reported, so a growing exemption list
// is visible rather than quietly eroding the gate.
func skipReason(q string) string {
	switch {
	case formatTemplate.MatchString(q):
		return "fmt template, assembled at runtime"
	case timescaleOnly.MatchString(q):
		return "TimescaleDB-only; this suite runs plain postgres by design"
	}
	return ""
}

func collectSQLLiterals(t *testing.T, roots []string) []sqlLiteral {
	t.Helper()
	seen := map[string]bool{}
	var out []sqlLiteral
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, m := range backtickLiteral.FindAllStringSubmatch(string(src), -1) {
				q := strings.TrimSpace(m[1])
				if !looksLikeSQL.MatchString(q) || danglingFragment.MatchString(q) {
					continue
				}
				// A statement assembled from several literals shows up as its
				// first piece; those are covered by the dangling check above.
				if seen[q] {
					continue
				}
				seen[q] = true
				out = append(out, sqlLiteral{where: filepath.ToSlash(path), sql: q})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return out
}

// isIndeterminateParam reports the two Postgres codes for "I cannot work out
// what type this placeholder is", which the driver resolves at execution time.
func isIndeterminateParam(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "could not determine data type of parameter") ||
		strings.Contains(msg, "42P18")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + " ..."
	}
	return s
}
