package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	pb "dylaris-proto/node"

	"github.com/gorilla/mux"
)

func TestNextModpackScan(t *testing.T) {
	cases := []struct {
		name, typ, current string
		reinstall          bool
		want               string
	}{
		{"technic setup", "technic", "", false, modpackScanPending},
		{"upload setup", "upload", "done", false, modpackScanPending},
		{"upload-zip setup", "upload-zip", "", false, modpackScanPending},
		{"loader setup clears", "fabric", "done", false, ""},
		{"modpack setup clears", "modpack", "pending", false, ""},
		{"reinstall of a scanned upload rescans", "forge", "done", true, modpackScanPending},
		{"reinstall of a plain loader stays off", "forge", "", true, ""},
	}
	for _, tc := range cases {
		if got := nextModpackScan(tc.typ, tc.current, tc.reinstall); got != tc.want {
			t.Errorf("%s: nextModpackScan(%q, %q, %v) = %q, want %q", tc.name, tc.typ, tc.current, tc.reinstall, got, tc.want)
		}
	}
}

func TestRowsFromHashes(t *testing.T) {
	hashes := []*pb.FileHash{
		{Name: "a.jar", Sha1: "h1"},
		{Name: "broken.jar", Sha1: "h2", Error: "permission denied"},
		{Name: "private.jar", Sha1: "h3"},
		{Name: "noproject.jar", Sha1: "h4"},
		{Name: "a-copy.jar", Sha1: "h5"},
		{Name: "b.jar", Sha1: "h6"},
	}
	known := map[string]services.ModrinthVersion{
		"h1": {ID: "v1", ProjectID: "P1", VersionNum: "1.0"},
		"h2": {ID: "v2", ProjectID: "P2"},
		"h4": {ID: "v4"},
		"h5": {ID: "v5", ProjectID: "P1"},
		"h6": {ID: "v6", ProjectID: "P6"},
	}
	rows := rowsFromHashes(7, "server", hashes, known)
	want := []struct{ pid, vid, file string }{{"P1", "v1", "a.jar"}, {"P6", "v6", "b.jar"}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d rows", rows, len(want))
	}
	for i, w := range want {
		r := rows[i]
		if r.ModrinthProjectID != w.pid || r.ModrinthVersionID != w.vid || r.FileName != w.file ||
			r.Side != models.SideBoth || r.ServerID != 7 || r.SubServerName != "server" {
			t.Errorf("row %d = %+v, want project %s version %s file %s side both", i, r, w.pid, w.vid, w.file)
		}
	}
	if rows[0].ModrinthVersionNumber != "1.0" {
		t.Errorf("version number = %q, want 1.0", rows[0].ModrinthVersionNumber)
	}
}

type modpackScanFakeStore struct {
	store.Store
	status    string
	installer string
	scan      string
	jars      int
	gen       int
	contents  []models.ServerModpackContent
	managed   []models.ServerMod
}

func (f *modpackScanFakeStore) GetServerByID(id int) (*models.Server, error) {
	return &models.Server{ID: id, UUID: "srv-uuid", NodeID: 1, ActiveSubServer: "server", Status: f.status}, nil
}
func (f *modpackScanFakeStore) GetModpackScan(int, string) (string, int, int, error) {
	return f.scan, f.jars, f.gen, nil
}
func (f *modpackScanFakeStore) ListServerModpackContents(int, string) ([]models.ServerModpackContent, error) {
	return f.contents, nil
}
func (f *modpackScanFakeStore) GetSubServerInstall(id int, sub string) (*models.SubServerInstall, error) {
	return &models.SubServerInstall{ServerID: id, SubServerName: sub, InstallerType: f.installer}, nil
}
func (f *modpackScanFakeStore) ListServerMods(int, string) ([]models.ServerMod, error) {
	return f.managed, nil
}
func (f *modpackScanFakeStore) CompleteModpackScan(_ int, _ string, gen int, rows []models.ServerModpackContent, jars int) (bool, error) {
	if f.scan != modpackScanPending || gen != f.gen {
		return false, nil
	}
	f.scan, f.jars, f.contents = modpackScanDone, jars, rows
	return true, nil
}

type modpackScanCalls struct{ listed, hashed []string }

func fakeScanDeps(calls *modpackScanCalls, files []string, known map[string]services.ModrinthVersion, lookupErr error) func(*models.Server) modpackScanDeps {
	return func(*models.Server) modpackScanDeps {
		return modpackScanDeps{
			list: func(dir string) ([]*pb.FileInfo, error) {
				calls.listed = append(calls.listed, dir)
				out := []*pb.FileInfo{{Name: "config", IsDir: true}, {Name: "readme.txt"}}
				for _, f := range files {
					out = append(out, &pb.FileInfo{Name: f})
				}
				return out, nil
			},
			hash: func(dir string, names []string) ([]*pb.FileHash, error) {
				calls.hashed = append(calls.hashed, names...)
				out := []*pb.FileHash{}
				for _, n := range names {
					out = append(out, &pb.FileHash{Name: n, Sha1: "sha-" + n})
				}
				return out, nil
			},
			lookup: func([]string) (map[string]services.ModrinthVersion, error) {
				return known, lookupErr
			},
		}
	}
}

func getModpackContents(t *testing.T, h *ServerModsHandler) (contents int, isModpack bool) {
	t.Helper()
	rw := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/servers/7/modpack-contents", nil)
	h.ModpackContents(rw, mux.SetURLVars(req, map[string]string{"id": "7"}))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rw.Code, rw.Body.String())
	}
	var body struct {
		Contents  []models.ServerModpackContent `json:"contents"`
		IsModpack bool                          `json:"isModpack"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return len(body.Contents), body.IsModpack
}

func TestModpackContentsLazyScan(t *testing.T) {
	cases := []struct {
		name          string
		status        string
		installer     string
		files         []string
		managed       []models.ServerMod
		known         map[string]services.ModrinthVersion
		lookupErr     error
		wantScan      string
		wantJars      int
		wantContents  int
		wantIsModpack bool
		wantListed    bool
	}{
		{
			name: "installing skips the scan", status: "installing", installer: "technic",
			files: []string{"a.jar"}, wantScan: modpackScanPending,
		},
		{
			name: "lookup error keeps pending", status: "stopped", installer: "technic",
			files: []string{"a.jar"}, lookupErr: errors.New("modrinth down"),
			wantScan: modpackScanPending, wantListed: true,
		},
		{
			name: "empty mods is done and not a modpack", status: "stopped", installer: "upload",
			wantScan: modpackScanDone, wantListed: true,
		},
		{
			name: "unknown jars still make a modpack", status: "stopped", installer: "upload",
			files: []string{"private.jar"}, known: map[string]services.ModrinthVersion{},
			wantScan: modpackScanDone, wantJars: 1, wantIsModpack: true, wantListed: true,
		},
		{
			name: "known jars become rows, managed jars are skipped", status: "online", installer: "technic",
			files:    []string{"a.jar", "mine.jar"},
			managed:  []models.ServerMod{{FileName: "mine.jar", TargetDir: "mods"}},
			known:    map[string]services.ModrinthVersion{"sha-a.jar": {ID: "v1", ProjectID: "P1"}},
			wantScan: modpackScanDone, wantJars: 1, wantContents: 1, wantIsModpack: true, wantListed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &modpackScanFakeStore{status: tc.status, installer: tc.installer, scan: modpackScanPending, managed: tc.managed}
			calls := &modpackScanCalls{}
			h := &ServerModsHandler{
				state:    &AppState{Store: fs, Redis: newServerPowerRedis(t)},
				scanDeps: fakeScanDeps(calls, tc.files, tc.known, tc.lookupErr),
			}
			contents, isModpack := getModpackContents(t, h)
			if fs.scan != tc.wantScan || fs.jars != tc.wantJars {
				t.Errorf("scan = %q jars %d, want %q jars %d", fs.scan, fs.jars, tc.wantScan, tc.wantJars)
			}
			if contents != tc.wantContents || isModpack != tc.wantIsModpack {
				t.Errorf("contents %d isModpack %v, want %d %v", contents, isModpack, tc.wantContents, tc.wantIsModpack)
			}
			if (len(calls.listed) > 0) != tc.wantListed {
				t.Errorf("listed %v, want listed=%v", calls.listed, tc.wantListed)
			}
			for _, d := range calls.listed {
				if d != "server/mods" {
					t.Errorf("listed %q, want only server/mods", d)
				}
			}
			for _, n := range calls.hashed {
				if n == "mine.jar" || n == "readme.txt" || n == "config" {
					t.Errorf("hashed %q, which is managed or not a jar", n)
				}
			}
		})
	}
}

// A failed scan holds the lock, so the next load inside the cooldown does not
// hammer the node and Modrinth again.
func TestModpackContentsFailedScanCoolsDown(t *testing.T) {
	fs := &modpackScanFakeStore{status: "stopped", installer: "technic", scan: modpackScanPending}
	calls := &modpackScanCalls{}
	h := &ServerModsHandler{
		state:    &AppState{Store: fs, Redis: newServerPowerRedis(t)},
		scanDeps: fakeScanDeps(calls, []string{"a.jar"}, nil, errors.New("modrinth down")),
	}
	getModpackContents(t, h)
	getModpackContents(t, h)
	if len(calls.listed) != 1 {
		t.Errorf("scan ran %d times, want 1 inside the cooldown", len(calls.listed))
	}
}

func TestIsModpackServer(t *testing.T) {
	cases := []struct {
		contents int
		typ      string
		jars     int
		want     bool
	}{
		{0, "upload", 0, false},
		{0, "fabric", 0, false},
		{3, "fabric", 0, true},
		{0, "modpack", 0, true},
		{0, "pack", 0, true},
		{0, "technic", 2, true},
	}
	for _, tc := range cases {
		if got := isModpackServer(tc.contents, tc.typ, tc.jars); got != tc.want {
			t.Errorf("isModpackServer(%d, %q, %d) = %v, want %v", tc.contents, tc.typ, tc.jars, got, tc.want)
		}
	}
}

// A reinstall while the scan hashes owes a new scan under a new generation; the
// old scan's result is of the old mods/ and must not be recorded as done.
func TestScanModpackJarsLosesToAReinstallDuringIt(t *testing.T) {
	st := &modpackScanFakeStore{scan: modpackScanPending, gen: 1}
	srv := &models.Server{ID: 7, UUID: "srv-uuid", NodeID: 1, ActiveSubServer: "server"}
	deps := modpackScanDeps{
		list: func(string) ([]*pb.FileInfo, error) {
			st.gen++ // the reinstall lands while the node is listing
			return []*pb.FileInfo{{Name: "a.jar"}}, nil
		},
		hash: func(_ string, names []string) ([]*pb.FileHash, error) {
			return []*pb.FileHash{{Name: "a.jar", Sha1: "s1"}}, nil
		},
		lookup: func([]string) (map[string]services.ModrinthVersion, error) {
			return map[string]services.ModrinthVersion{"s1": {ID: "v1", ProjectID: "P1"}}, nil
		},
	}
	if err := scanModpackJars(st, srv, deps); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if st.scan != modpackScanPending || len(st.contents) != 0 {
		t.Fatalf("stale scan recorded: scan %q contents %+v", st.scan, st.contents)
	}
}

func TestIsModJarName(t *testing.T) {
	for name, want := range map[string]bool{
		"a.jar": true, "B.JAR": true, "a.jar.disabled": true, "a.zip": false, "jar": false, "a.jar.bak": false,
	} {
		if got := isModJarName(name); got != want {
			t.Errorf("isModJarName(%q) = %v, want %v", name, got, want)
		}
	}
}
