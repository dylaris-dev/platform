package tickets

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"dylaris-core/store"
)

// ticketColumns parses CREATE TABLE and ADD COLUMN statements into
// table -> column -> NOT NULL.
func ticketColumns(sql string) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	create := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\s*\)`)
	for _, m := range create.FindAllStringSubmatch(sql, -1) {
		cols := map[string]bool{}
		for _, line := range strings.Split(m[2], "\n") {
			line = strings.TrimSpace(line)
			f := strings.Fields(line)
			if len(f) < 2 || strings.HasPrefix(line, "--") || f[0] == "PRIMARY" || strings.HasPrefix(f[0], "UNIQUE") {
				continue
			}
			cols[f[0]] = strings.Contains(line, "NOT NULL")
		}
		out[m[1]] = cols
	}
	add := regexp.MustCompile(`ALTER TABLE (\w+) ADD COLUMN IF NOT EXISTS (\w+) ([^` + "`" + `]*)`)
	for _, m := range add.FindAllStringSubmatch(sql, -1) {
		if out[m[1]] == nil {
			out[m[1]] = map[string]bool{}
		}
		out[m[1]][m[2]] = strings.Contains(m[3], "NOT NULL")
	}
	return out
}

// The external ticket DB is built by applyTicketSchema, and the migration
// copies SELECT * rows from the main DB into it. Every live column must exist
// there, and none may be NOT NULL where the live one is nullable, or the copy
// fails on the first ticket (it did: category_name was missing).
func TestApplyTicketSchemaMatchesTheLiveColumns(t *testing.T) {
	src, err := os.ReadFile("../../database/db_tickets.go")
	if err != nil {
		t.Fatal(err)
	}
	live := ticketColumns(string(src))
	ext := ticketColumns(strings.Join(ticketSchemaStmts, "\n"))
	for _, table := range store.TicketTablesInOrder() {
		if len(live[table]) == 0 {
			t.Fatalf("parsed no live columns for %s", table)
		}
		for col, liveNotNull := range live[table] {
			extNotNull, ok := ext[table][col]
			if !ok {
				t.Errorf("%s.%s is missing from applyTicketSchema", table, col)
				continue
			}
			if extNotNull && !liveNotNull {
				t.Errorf("%s.%s is NOT NULL in applyTicketSchema but nullable live", table, col)
			}
		}
	}
}
