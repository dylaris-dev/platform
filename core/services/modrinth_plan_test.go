package services

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type dep = struct {
	VersionID      string `json:"version_id"`
	ProjectID      string `json:"project_id"`
	DependencyType string `json:"dependency_type"`
}

// fakeDepModrinth answers the resolver's three calls from a fixed catalog. The
// project listing ignores the loader/MC filter on purpose, so the resolver's
// own fit check is what decides.
type fakeDepModrinth struct {
	versions      map[string]*modrinthVersion
	projectCalls  int
	versionsCalls int
}

func (f *fakeDepModrinth) install(t *testing.T) {
	t.Helper()
	oldV, oldP, oldM := fetchVersionsByID, fetchProjectVersions, fetchPlanProjects
	t.Cleanup(func() { fetchVersionsByID, fetchProjectVersions, fetchPlanProjects = oldV, oldP, oldM })
	fetchVersionsByID = func(_ context.Context, ids []string) (map[string]*modrinthVersion, error) {
		f.versionsCalls++
		out := map[string]*modrinthVersion{}
		for _, id := range ids {
			if v, ok := f.versions[id]; ok {
				out[id] = v
			}
		}
		return out, nil
	}
	fetchProjectVersions = func(_ context.Context, projectID string, _, _ []string) ([]modrinthVersion, error) {
		f.projectCalls++
		var list []modrinthVersion
		for _, v := range f.versions {
			if v.ProjectID == projectID {
				list = append(list, *v)
			}
		}
		if len(list) == 0 {
			return nil, &ModrinthHTTPError{Path: "/project/" + projectID + "/version", Status: 404}
		}
		return list, nil
	}
	fetchPlanProjects = func(_ context.Context, ids []string) (map[string]ModrinthProjectMeta, error) {
		out := map[string]ModrinthProjectMeta{}
		for _, id := range ids {
			out[id] = ModrinthProjectMeta{ID: id, Slug: strings.ToLower(id), Title: "Title " + id}
		}
		return out, nil
	}
}

// ver is a neoforge 1.21.1 release unless changed.
func ver(id, project, date string, deps ...dep) *modrinthVersion {
	return &modrinthVersion{
		ID: id, ProjectID: project, VersionNum: id, VersionType: "release", DatePub: date,
		Loaders: []string{"neoforge"}, GameVersions: []string{"1.21.1"},
		Files:        []ModrinthFile{{Filename: id + ".jar", URL: "https://cdn.modrinth.com/" + id, Primary: true}},
		Dependencies: deps,
	}
}

func req(project, version string) dep {
	return dep{ProjectID: project, VersionID: version, DependencyType: "required"}
}

func catalog(vs ...*modrinthVersion) map[string]*modrinthVersion {
	m := map[string]*modrinthVersion{}
	for _, v := range vs {
		m[v.ID] = v
	}
	return m
}

func planIDs(p *ModInstallPlan) []string {
	out := []string{}
	for _, e := range p.Install {
		out = append(out, e.VersionID)
	}
	return out
}

func problemIDs(ps []DepProblem) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.ProjectID)
	}
	return out
}

func TestPlanModInstall(t *testing.T) {
	beta := ver("b-beta", "B", "2026-10-01")
	beta.VersionType = "beta"
	forgeBuild := ver("b-forge", "B", "2026-10-05")
	forgeBuild.Loaders = []string{"forge"}
	wrongMC := ver("b-263", "B", "2026-10-06")
	wrongMC.GameVersions = []string{"26.3"}

	tests := []struct {
		name             string
		versions         map[string]*modrinthVersion
		installed        map[string]string
		wantInstall      []string
		wantUnresolved   []string
		wantIncompatible []string
	}{
		{
			name: "chain A->B->C is installed dependency-first",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "")),
				ver("b1", "B", "2026-01-01", req("C", "")),
				ver("c1", "C", "2026-01-01"),
			),
			wantInstall: []string{"c1", "b1", "a1"},
		},
		{
			name: "cycle A<->B stops",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "")),
				ver("b1", "B", "2026-01-01", req("A", "")),
			),
			wantInstall: []string{"b1", "a1"},
		},
		{
			name: "already installed dependency is skipped",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "")),
				ver("b1", "B", "2026-01-01", req("C", "")),
				ver("c1", "C", "2026-01-01"),
			),
			installed:   map[string]string{"B": "b1"},
			wantInstall: []string{"a1"},
		},
		{
			name: "version_id-only dependency learns its project",
			versions: catalog(
				ver("a1", "A", "2026-01-01", dep{VersionID: "b1", DependencyType: "required"}),
				ver("b1", "B", "2026-01-01"),
				ver("b2", "B", "2026-05-01"),
			),
			wantInstall: []string{"b1", "a1"},
		},
		{
			name: "pinned version that does not fit falls back to the newest fitting",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "b-forge")),
				forgeBuild,
				ver("b1", "B", "2026-01-01"),
				ver("b2", "B", "2026-03-01"),
				wrongMC,
			),
			wantInstall: []string{"b2", "a1"},
		},
		{
			name: "nothing fitting is unresolved",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "")),
				forgeBuild,
				wrongMC,
			),
			wantInstall:    []string{"a1"},
			wantUnresolved: []string{"B"},
		},
		{
			name: "unknown project is unresolved",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("GONE", "")),
			),
			wantInstall:    []string{"a1"},
			wantUnresolved: []string{"GONE"},
		},
		{
			name: "release preferred over a newer beta",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "")),
				ver("b-rel", "B", "2026-09-01"),
				beta,
			),
			wantInstall: []string{"b-rel", "a1"},
		},
		{
			name: "only a beta fits: the beta is taken",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "")),
				beta,
			),
			wantInstall: []string{"b-beta", "a1"},
		},
		{
			name: "installed incompatible project is reported",
			versions: catalog(
				ver("a1", "A", "2026-01-01", dep{ProjectID: "X", DependencyType: "incompatible"}, dep{ProjectID: "Y", DependencyType: "incompatible"}),
			),
			installed:        map[string]string{"X": "x-gone"},
			wantInstall:      []string{"a1"},
			wantIncompatible: []string{"X"},
		},
		{
			// The B on the server is a Forge jar; a NeoForge server cannot load it,
			// so B is not satisfied and the plan brings the fitting one.
			name: "installed wrong-loader build does not satisfy",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "")),
				ver("b1", "B", "2026-01-01"),
				forgeBuild,
			),
			installed:   map[string]string{"B": "b-forge"},
			wantInstall: []string{"b1", "a1"},
		},
		{
			name: "installed build for another MC version does not satisfy",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "")),
				ver("b1", "B", "2026-01-01"),
				wrongMC,
			),
			installed:   map[string]string{"B": "b-263"},
			wantInstall: []string{"b1", "a1"},
		},
		{
			// Modrinth does not know the installed version (pulled, or a jar we
			// never placed): taken as fitting rather than reinstalled on a guess.
			name: "installed build Modrinth does not know still satisfies",
			versions: catalog(
				ver("a1", "A", "2026-01-01", req("B", "")),
				ver("b1", "B", "2026-01-01"),
			),
			installed:   map[string]string{"B": "b-unknown"},
			wantInstall: []string{"a1"},
		},
		{
			// A conflict is a conflict whichever build of X is on disk.
			name: "installed incompatible project is reported even as a wrong build",
			versions: catalog(
				ver("a1", "A", "2026-01-01", dep{ProjectID: "X", DependencyType: "incompatible"}),
				func() *modrinthVersion {
					v := ver("x-forge", "X", "2026-01-01")
					v.Loaders = []string{"forge"}
					return v
				}(),
			),
			installed:        map[string]string{"X": "x-forge"},
			wantInstall:      []string{"a1"},
			wantIncompatible: []string{"X"},
		},
		{
			name: "optional dependencies are not installed",
			versions: catalog(
				ver("a1", "A", "2026-01-01", dep{ProjectID: "B", DependencyType: "optional"}),
				ver("b1", "B", "2026-01-01"),
			),
			wantInstall: []string{"a1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeDepModrinth{versions: tt.versions}
			f.install(t)
			plan, err := PlanModInstall(context.Background(), "a1", "neoforge", "1.21.1", tt.installed)
			if err != nil {
				t.Fatalf("PlanModInstall: %v", err)
			}
			if got := planIDs(plan); !reflect.DeepEqual(got, tt.wantInstall) {
				t.Errorf("install = %v, want %v", got, tt.wantInstall)
			}
			want := tt.wantUnresolved
			if want == nil {
				want = []string{}
			}
			if got := problemIDs(plan.Unresolved); !reflect.DeepEqual(got, want) {
				t.Errorf("unresolved = %v, want %v", got, want)
			}
			want = tt.wantIncompatible
			if want == nil {
				want = []string{}
			}
			if got := problemIDs(plan.Incompatible); !reflect.DeepEqual(got, want) {
				t.Errorf("incompatible = %v, want %v", got, want)
			}
		})
	}
}

func TestPlanModInstallEntryShape(t *testing.T) {
	f := &fakeDepModrinth{versions: catalog(
		ver("a1", "A", "2026-01-01", dep{ProjectID: "X", DependencyType: "incompatible"}),
	)}
	f.install(t)
	plan, err := PlanModInstall(context.Background(), "a1", "neoforge", "1.21.1", map[string]string{"X": ""})
	if err != nil {
		t.Fatal(err)
	}
	want := PlanEntry{ProjectID: "A", Slug: "a", Title: "Title A", VersionID: "a1", VersionNumber: "a1",
		FileName: "a1.jar", DownloadURL: "https://cdn.modrinth.com/a1"}
	if plan.Install[0] != want {
		t.Errorf("entry = %+v, want %+v", plan.Install[0], want)
	}
	if got := plan.Incompatible[0]; got.Title != "Title X" || got.Reason != "Title A is marked incompatible with it." {
		t.Errorf("incompatible = %+v", got)
	}
}

func TestPlanModInstallRefusesARootThatDoesNotFit(t *testing.T) {
	root := ver("a1", "A", "2026-01-01")
	root.Loaders = []string{"forge"}
	f := &fakeDepModrinth{versions: catalog(root)}
	f.install(t)
	_, err := PlanModInstall(context.Background(), "a1", "neoforge", "1.21.1", nil)
	var fit *ModFitError
	if !errors.As(err, &fit) || !strings.Contains(fit.Reason, "forge") {
		t.Fatalf("err = %v, want a ModFitError about forge", err)
	}
	_, err = PlanModInstall(context.Background(), "missing", "neoforge", "1.21.1", nil)
	if !ModrinthNotFound(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

// A graph wider than the cap stops at maxDepNodes lookups and says so.
func TestPlanModInstallIsBounded(t *testing.T) {
	var deps []dep
	vs := []*modrinthVersion{}
	for i := 0; i < maxDepNodes+10; i++ {
		p := "P" + strings.Repeat("x", i)
		deps = append(deps, req(p, ""))
		vs = append(vs, ver("v"+p, p, "2026-01-01"))
	}
	vs = append(vs, ver("a1", "A", "2026-01-01", deps...))
	f := &fakeDepModrinth{versions: catalog(vs...)}
	f.install(t)
	plan, err := PlanModInstall(context.Background(), "a1", "neoforge", "1.21.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Install) != maxDepNodes+1 || len(plan.Unresolved) != 10 {
		t.Errorf("install %d, unresolved %d", len(plan.Install), len(plan.Unresolved))
	}
	if f.projectCalls > maxDepNodes {
		t.Errorf("%d project lookups, want at most %d", f.projectCalls, maxDepNodes)
	}
}

// Packs share the walk: same picks, root left out, and no MC means no answer
// rather than an unfiltered one.
func TestResolveModrinthDependenciesSharesThePlan(t *testing.T) {
	beta := ver("b-beta", "B", "2026-10-01")
	beta.VersionType = "beta"
	root := ver("a1", "A", "2026-01-01", req("B", ""))
	f := &fakeDepModrinth{versions: catalog(root, ver("b-rel", "B", "2026-09-01"), beta)}
	f.install(t)
	deps, _, err := ResolveModrinthDependencies(root, "1.21.1", "neoforge", map[string]bool{"A": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].Version.ID != "b-rel" {
		t.Errorf("deps = %+v, want only b-rel", deps)
	}
	if _, _, err := ResolveModrinthDependencies(root, "", "neoforge", nil); err == nil {
		t.Error("empty MC resolved, want a refusal")
	}
	if v, err := LatestProjectVersionFor("B", "", "neoforge"); err == nil || v != nil {
		t.Errorf("LatestProjectVersionFor without MC = %v, %v; want a refusal", v, err)
	}
	if v, err := LatestProjectVersionFor("B", "1.21.1", "neoforge"); err != nil || v == nil || v.ID != "b-rel" {
		t.Errorf("LatestProjectVersionFor = %v, %v; want b-rel", v, err)
	}
}

// One dependency Modrinth fails on (429, 5xx, timeout) costs that dependency,
// not the plan; only the root lookup failing fails the whole call.
func TestPlanModInstallSurvivesADependencyLookupFailure(t *testing.T) {
	root := ver("a1", "A", "2026-01-01", req("B", ""), req("C", "c1"), dep{VersionID: "d1", DependencyType: "required"})
	f := &fakeDepModrinth{versions: catalog(root, ver("b1", "B", "2026-01-01"), ver("c1", "C", "2026-01-01"), ver("d1", "D", "2026-01-01"))}
	f.install(t)
	busy := &ModrinthHTTPError{Path: "/x", Status: 503}
	innerV, innerP := fetchVersionsByID, fetchProjectVersions
	fetchVersionsByID = func(ctx context.Context, ids []string) (map[string]*modrinthVersion, error) {
		if len(ids) == 1 && ids[0] == "a1" {
			return innerV(ctx, ids)
		}
		return nil, busy
	}
	fetchProjectVersions = func(ctx context.Context, projectID string, l, g []string) ([]modrinthVersion, error) {
		if projectID == "B" {
			return nil, busy
		}
		return innerP(ctx, projectID, l, g)
	}
	plan, err := PlanModInstall(context.Background(), "a1", "neoforge", "1.21.1", nil)
	if err != nil {
		t.Fatalf("PlanModInstall: %v", err)
	}
	// C is pinned, its bulk lookup failed, so its project lookup stands in.
	if got := planIDs(plan); !reflect.DeepEqual(got, []string{"c1", "a1"}) {
		t.Errorf("install = %v, want [c1 a1]", got)
	}
	if got := problemIDs(plan.Unresolved); !reflect.DeepEqual(got, []string{"B", ""}) {
		t.Errorf("unresolved = %v, want [B ]", got)
	}
	for _, p := range plan.Unresolved {
		if p.Reason != modrinthNoAnswer {
			t.Errorf("reason %q, want the no-answer one", p.Reason)
		}
	}

	fetchVersionsByID = func(context.Context, []string) (map[string]*modrinthVersion, error) { return nil, busy }
	if _, err := PlanModInstall(context.Background(), "a1", "neoforge", "1.21.1", nil); !errors.Is(err, busy) {
		t.Errorf("root lookup failure = %v, want it returned", err)
	}
}
