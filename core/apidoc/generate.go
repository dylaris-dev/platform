package apidoc

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DocPath is where the rendered reference lives, relative to the core module
// directory. The generator and the freshness test share it so they can never
// disagree about which file is authoritative.
//
// It sits beside README.md and TESTING.md rather than under docs/, which
// .gitignore has excluded since the repository was split out of the monorepo:
// a generated reference nobody can check out is worse than no reference.
const DocPath = "../API.md"

// PreamblePath is the hand-written prose, relative to the core module
// directory.
const PreamblePath = "apidoc/preamble.md"

// Generate renders the reference for the core module rooted at coreDir.
//
// Line endings are normalised to LF: the working tree is CRLF on Windows
// (core.autocrlf), and a generator that echoed whatever it read would produce a
// different document on each platform and break the freshness test for reasons
// that have nothing to do with the routes.
func Generate(coreDir string) (string, error) {
	// Every directory under handlers/ is a source of doc comments, found by
	// walking rather than listed, so a new handler package cannot silently
	// lose its descriptions. coreDir itself is one too: a handful of routes
	// are served by a plain function in package main rather than by a
	// handler method.
	dirs, err := handlerDirs(filepath.Join(coreDir, "handlers"))
	if err != nil {
		return "", err
	}
	routes, err := Parse(filepath.Join(coreDir, "routes.go"), append(dirs, coreDir))
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(filepath.Join(coreDir, PreamblePath))
	if err != nil {
		return "", fmt.Errorf("read preamble: %w", err)
	}
	return Render(routes, Normalize(string(raw))), nil
}

// handlerDirs returns root and every directory below it in WalkDir's lexical
// order, skipping testdata as the go tool does.
func handlerDirs(root string) ([]string, error) {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return dirs, nil
}

// Normalize strips carriage returns so generated and checked-in text compare on
// content rather than on checkout settings.
func Normalize(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}
