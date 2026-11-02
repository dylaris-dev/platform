package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Core file storage is ONE Manager per process (spec R7). Its mutex is the
// single-process half of the reach-round lock, so a second Manager - built in
// a route, a worker, a handler constructor - would be a second lock, and two
// saves on one replica could run rounds against each other. main.go builds it
// once and everything else reads appState.CoreStorage.
//
// A source pin rather than a runtime check: a second construction would sit in
// a code path a test may never reach.
func TestCoreStorageManagerIsBuiltOnce(t *testing.T) {
	var sites []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || filepath.ToSlash(path) == "services/corestorage" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if n := strings.Count(string(b), "corestorage.Manager{"); n > 0 {
			sites = append(sites, filepath.ToSlash(path)+" x"+string(rune('0'+n)))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(sites) != 1 || sites[0] != "main.go x1" {
		t.Fatalf("corestorage.Manager is constructed at %v; it must be built exactly once, in main.go", sites)
	}
}
