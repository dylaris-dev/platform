package handlers

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"dylaris-core/storage"
)

func TestPurgeLibraryPrefixDeletesOnlyTheLibrary(t *testing.T) {
	base := t.TempDir()
	for _, f := range []string{"library/a.jar", "library/b/c.jar", "library-old/keep.jar", "ticket-attachments/1/x.png"} {
		p := filepath.Join(base, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	build := func(subPrefix string) (storage.StorageProvider, error) {
		if subPrefix != "" {
			t.Errorf("built a provider under %q, want the storage root", subPrefix)
		}
		return &storage.LocalProvider{BasePath: base}, nil
	}

	n, err := purgeLibraryPrefix(context.Background(), build)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d files, want 2", n)
	}
	if _, err := os.Stat(filepath.Join(base, "library")); !os.IsNotExist(err) {
		t.Errorf("library/ still exists: %v", err)
	}
	for _, keep := range []string{"library-old/keep.jar", "ticket-attachments/1/x.png"} {
		if _, err := os.Stat(filepath.Join(base, filepath.FromSlash(keep))); err != nil {
			t.Errorf("%s was touched: %v", keep, err)
		}
	}

	// A second run finds nothing and still succeeds: the marker is only an
	// optimisation, never what makes this safe to repeat.
	if n, err := purgeLibraryPrefix(context.Background(), build); err != nil || n != 0 {
		t.Errorf("second run = %d, %v; want 0, nil", n, err)
	}
}
