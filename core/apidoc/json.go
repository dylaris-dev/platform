package apidoc

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// JSONPath is where the machine-readable route list lives, relative to the core
// module directory. Beside API.md and generated from the same parse, so the two
// can never describe different routers.
const JSONPath = "../API.json"

// jsonRoute is one route as a consumer wants it, which is not quite how the
// parser holds it.
//
// The differences are deliberate. `Methods` is flattened to one method per entry
// because a caller asks "how do I POST this", not "which verbs share a template".
// `Cap` and `Authz` collapse into a single `authorization` string, because the
// distinction between "declares no capability" and "checks inside the handler"
// matters to whoever maintains the router and not at all to whoever is calling
// it.
type jsonRoute struct {
	Method        string `json:"method"`
	Path          string `json:"path"`
	Auth          string `json:"auth"`
	Authorization string `json:"authorization"`
	Description   string `json:"description,omitempty"`
	Handler       string `json:"handler,omitempty"`
	// Group is the leading path segment under /api, used to file routes into
	// sections. "servers", "auth", "external", and so on.
	Group string `json:"group"`
	// External marks the ten scoped routes an API key can reach. They are the
	// ones a script should prefer, so they are worth finding without reading
	// every path.
	External bool `json:"external"`
}

// APIDocument is the whole machine-readable reference.
type APIDocument struct {
	// Generated is deliberately absent. This file is committed and compared by a
	// freshness test, and a timestamp would make every regeneration a diff.
	Routes []jsonRoute `json:"routes"`
	Counts struct {
		Total    int            `json:"total"`
		ByAuth   map[string]int `json:"byAuth"`
		ByGroup  map[string]int `json:"byGroup"`
		External int            `json:"external"`
	} `json:"counts"`
}

// authorizationOf renders why a route is or is not capability-gated, in the one
// string a caller needs.
func authorizationOf(r Route) string {
	if r.Cap != "" {
		return r.Cap
	}
	switch r.Authz {
	case "in-handler":
		return "checked in handler"
	case "uncapped method":
		return "no capability on this method"
	case "exempt":
		if r.Auth == "" {
			return "public"
		}
		return "no capability"
	}
	if r.Auth == "" {
		return "public"
	}
	return "no capability"
}

// groupOf files a route by the first segment after /api.
func groupOf(path string) string {
	p := strings.TrimPrefix(path, "/api/")
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i > 0 {
		p = p[:i]
	}
	if p == "" {
		return "root"
	}
	// Strip a mux constraint, so "{id:[0-9]+}" never becomes a group of its own.
	if strings.HasPrefix(p, "{") {
		return "root"
	}
	return p
}

// GenerateJSON renders the machine-readable reference for the core module rooted
// at coreDir. Same parse as Generate, so the two documents cannot drift.
func GenerateJSON(coreDir string) (string, error) {
	routes, err := Parse(filepath.Join(coreDir, "routes.go"), []string{
		filepath.Join(coreDir, "handlers"),
		filepath.Join(coreDir, "handlers", "packs"),
		filepath.Join(coreDir, "handlers", "nodes"),
		filepath.Join(coreDir, "handlers", "gateway"),
		filepath.Join(coreDir, "handlers", "beam"),
		filepath.Join(coreDir, "handlers", "billing"),
		filepath.Join(coreDir, "handlers", "tabs"),
		filepath.Join(coreDir, "handlers", "backup"),
		filepath.Join(coreDir, "handlers", "stats"),
		filepath.Join(coreDir, "handlers", "tickets"),
		coreDir,
	})
	if err != nil {
		return "", err
	}

	doc := APIDocument{}
	doc.Counts.ByAuth = map[string]int{}
	doc.Counts.ByGroup = map[string]int{}

	for _, r := range routes {
		methods := r.Methods
		if len(methods) == 0 {
			// A registration that constrains no method answers all of them. Say so
			// once rather than inventing a list.
			methods = []string{"ANY"}
		}
		auth := r.Auth
		if auth == "" {
			auth = "none"
		}
		group := groupOf(r.Path)
		external := strings.HasPrefix(r.Path, "/api/external/")
		for _, m := range methods {
			doc.Routes = append(doc.Routes, jsonRoute{
				Method:        m,
				Path:          r.Path,
				Auth:          auth,
				Authorization: authorizationOf(r),
				Description:   r.Doc,
				Handler:       r.Handler,
				Group:         group,
				External:      external,
			})
			doc.Counts.Total++
			doc.Counts.ByAuth[auth]++
			doc.Counts.ByGroup[group]++
			if external {
				doc.Counts.External++
			}
		}
	}

	// Stable order: group, then path, then method. Source order is what API.md
	// uses and is right for reading the router; a reference someone searches
	// wants to be sorted, and an unstable order would churn the diff.
	sort.SliceStable(doc.Routes, func(i, j int) bool {
		a, b := doc.Routes[i], doc.Routes[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Method < b.Method
	})

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out) + "\n", nil
}
