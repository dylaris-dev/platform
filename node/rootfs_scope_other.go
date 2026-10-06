//go:build !linux

package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// writeScope off Linux: no openat chain is available, so the write runs in a
// Root at rootDir as before. The node ships for Linux; this keeps it building
// and testable elsewhere.
func writeScope(rootDir, rel string, mkParents bool) (*os.Root, string, error) {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return nil, "", err
	}
	rel = path.Clean("/" + strings.ReplaceAll(rel, "\\", "/"))[1:]
	if rel == "" {
		rel = "."
	}
	if mkParents {
		if err := mkdirParentIn(root, rel); err != nil {
			root.Close()
			return nil, "", err
		}
	}
	return root, rel, nil
}

func renameNoFollow(rootDir, oldRel, newRel string) error {
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Rename(oldRel, newRel)
}

func pinDir(rootDir, rel string, mk bool) (string, func(), error) {
	p := filepath.Join(rootDir, filepath.FromSlash(rel))
	if mk {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return "", nil, err
		}
	}
	return p, func() {}, nil
}

func openNoFollow(p string) (*os.File, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	return f, nil
}
