package main

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The import rules of the handler packages (spec
// docs/superpowers/specs/2026-11-02-core-handlers-step2-design.md, section 5):
//
//	layer 2  handlers/<domain>          layer 0-1, services/**, store, models, authz, grpc, storage/**, pkg/**
//	layer 1  guard access audit reauth  layer 0, services/**, store, models, authz, grpc, storage/**, pkg/**
//	         nodeops gate               (no lateral imports inside layer 1)
//	layer 0  httpx reqctx validate      stdlib, models, mux, google/uuid, pkg/** only
//	services/**                         never imports handlers/**
//	handlertest                         only from _test.go files; never imports handlers or a layer-2 package
//
// Only package main may import the root handlers package. No directory except
// core/authz may be named authz: the capability enforcement test skips every
// directory of that name.
//
// layeringAllowed lists the edges that break these rules today. A refactor
// stage deletes the entries it made obsolete and never adds one; an entry no
// longer needed fails the test, so the list cannot outlive the edges.
var layeringAllowed = map[string]bool{
	"handlers/gateway -> handlers/servers":   true,
	"handlers/templates -> handlers/servers": true,

	"handlers/stats -> metrics": true,
}

var (
	layer0Pkgs = map[string]bool{"handlers/httpx": true, "handlers/reqctx": true, "handlers/validate": true}
	layer1Pkgs = map[string]bool{
		"handlers/guard": true, "handlers/access": true, "handlers/audit": true,
		"handlers/reauth": true, "handlers/nodeops": true, "handlers/gate": true,
	}
)

const handlerTestPkg = "handlers/handlertest"

func TestHandlerLayering(t *testing.T) {
	const module = "dylaris-core/"
	// "from -> to" -> whether to is a dylaris-core package; to is
	// module-relative then, and the full import path otherwise
	edges := map[string]bool{}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if d.Name() == "authz" && filepath.ToSlash(path) != "authz" {
				t.Errorf("%s: no directory may be named authz, the capability enforcement test skips it", path)
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		from := filepath.ToSlash(filepath.Dir(path))
		if from == "." {
			from = ""
		}
		for _, imp := range f.Imports {
			to, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			rel, internal := strings.CutPrefix(to, module)
			edges[from+" -> "+rel] = internal
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(edges) == 0 {
		t.Fatal("no imports found; is the test running from the core module root?")
	}

	used := map[string]bool{}
	var bad []string
	for e, internal := range edges {
		from, to, _ := strings.Cut(e, " -> ")
		if why := layeringViolation(from, to, internal); why != "" {
			if layeringAllowed[e] {
				used[e] = true
				continue
			}
			bad = append(bad, e+": "+why)
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
	for e := range layeringAllowed {
		if !used[e] {
			t.Errorf("allow-list entry %q is no longer needed: delete it", e)
		}
	}
}

// layeringViolation returns why from may not import to, or "" if it may. from
// is a module-relative package dir ("" is package main); to is module-relative
// when internal (a dylaris-core package) and the full import path otherwise.
func layeringViolation(from, to string, internal bool) string {
	isHandlers := internal && (to == "handlers" || strings.HasPrefix(to, "handlers/"))
	if !internal {
		to = "ext:" + to // never equal to a module-relative name below
	}

	if to == handlerTestPkg && from != handlerTestPkg {
		return "handlertest is for _test.go files only"
	}
	if to == "handlers" && from != "" {
		return "only package main may import the root handlers package"
	}
	switch {
	case from == "services" || strings.HasPrefix(from, "services/"):
		if isHandlers {
			return "services never import handlers"
		}
	case layer0Pkgs[from]:
		ext := strings.TrimPrefix(to, "ext:")
		// google/uuid is a leaf library with no imports of its own; httpx needs
		// it for the path-parameter parser (ParseUserID)
		if !internal && (isStdlib(ext) || ext == "github.com/gorilla/mux" || ext == "github.com/google/uuid" || ext == "dylaris-pkg" || strings.HasPrefix(ext, "dylaris-pkg/")) {
			return ""
		}
		if to == "models" || strings.HasPrefix(to, "pkg/") {
			return ""
		}
		return "layer 0 imports only stdlib, models, mux, google/uuid and pkg"
	case layer1Pkgs[from]:
		if internal && !layer0Pkgs[to] && !lowerShared(to) {
			return "layer 1 imports only layer 0, services, store, models, authz, grpc, storage and pkg"
		}
	case from == handlerTestPkg:
		if isHandlers && !layer0Pkgs[to] && !layer1Pkgs[to] {
			return "handlertest never imports the root handlers package or a layer-2 package"
		}
	case strings.HasPrefix(from, "handlers/"):
		if internal && !layer0Pkgs[to] && !layer1Pkgs[to] && !lowerShared(to) {
			return "a handler package imports only layer 0-1, services, store, models, authz, grpc, storage and pkg"
		}
	}
	return ""
}

// lowerShared reports whether a module-relative package is one every handler
// layer above 0 may use.
func lowerShared(to string) bool {
	for _, p := range []string{"services", "storage", "pkg"} {
		if to == p || strings.HasPrefix(to, p+"/") {
			return true
		}
	}
	return to == "store" || to == "models" || to == "authz" || to == "grpc"
}

// isStdlib: a standard library path has no dot in its first element, and
// dylaris-pkg is the only other dotless module in this workspace.
func isStdlib(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return !strings.Contains(first, ".") && first != "dylaris-pkg"
}
