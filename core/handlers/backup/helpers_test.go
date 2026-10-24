package backup

import (
	"os"
	"regexp"
	"testing"
)

// funcBody returns the source of one function, for the tests that pin how a
// handler is written; a copy of handlers/nodes_live_fields_test.go's.
func funcBody(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	re := regexp.MustCompile(`(?ms)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(fn) + `\(.*?^}`)
	body := re.FindString(string(src))
	if body == "" {
		t.Fatalf("%s not found in %s", fn, file)
	}
	return body
}
