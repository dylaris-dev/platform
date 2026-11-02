// Package handlertest holds the helpers the handler test suites share. It is
// imported only from _test.go files, and it must never import
// dylaris-core/handlers: the root package's own tests import this one, so that
// import would be a cycle.
package handlertest

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// FuncBody returns the text of one top-level func, from its signature to the
// closing brace in column 0.
func FuncBody(t testing.TB, file, fn string) string {
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

// HandlersRoot returns the absolute path of core/handlers, located from this
// file rather than from the working directory, so a test at any depth gets
// the same answer.
func HandlersRoot(t testing.TB) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("handlertest: cannot locate own source file")
	}
	return filepath.Dir(filepath.Dir(self))
}

// SourceFiles returns every non-test .go file under root, recursively and
// sorted. testdata directories are skipped, as the go tool skips them. Zero
// files fails the test: a source pin that reads nothing passes silently.
func SourceFiles(t testing.TB, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("no Go source under %s", root)
	}
	sort.Strings(files)
	return files
}
