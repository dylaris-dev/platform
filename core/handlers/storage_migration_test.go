package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// storageMigrationFakeStore serves settings, backup storages and manifests
// from maps. Local to this file; never redeclared.
type storageMigrationFakeStore struct {
	store.Store
	kv        map[string]string
	storages  []models.BackupStorage
	manifests []models.StorageManifest
	entries   map[int][]models.StorageManifestEntry
	deleted   []int
	onAudit   func(*models.AuditEventIdentity)
}

func newStorageMigrationFakeStore() *storageMigrationFakeStore {
	return &storageMigrationFakeStore{
		kv:      map[string]string{},
		entries: map[int][]models.StorageManifestEntry{},
	}
}

func (f *storageMigrationFakeStore) GetSetting(k string) (string, error) { return f.kv[k], nil }
func (f *storageMigrationFakeStore) SetSetting(k, v string) error {
	f.kv[k] = v
	return nil
}
func (f *storageMigrationFakeStore) ListBackupStorages() ([]models.BackupStorage, error) {
	return f.storages, nil
}
func (f *storageMigrationFakeStore) GetBackupStorage(id int) (*models.BackupStorage, error) {
	for i := range f.storages {
		if f.storages[i].ID == id {
			return &f.storages[i], nil
		}
	}
	return nil, nil
}
func (f *storageMigrationFakeStore) ListStorageManifests(dataSet string, limit int) ([]models.StorageManifest, error) {
	if dataSet == "" {
		return f.manifests, nil
	}
	out := []models.StorageManifest{}
	for _, m := range f.manifests {
		if m.DataSet == dataSet {
			out = append(out, m)
		}
	}
	return out, nil
}
func (f *storageMigrationFakeStore) GetStorageManifest(id int) (*models.StorageManifest, error) {
	for i := range f.manifests {
		if f.manifests[i].ID == id {
			return &f.manifests[i], nil
		}
	}
	return nil, nil
}
func (f *storageMigrationFakeStore) ListStorageManifestEntries(id int) ([]models.StorageManifestEntry, error) {
	return f.entries[id], nil
}
func (f *storageMigrationFakeStore) DeleteStorageManifest(id int) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *storageMigrationFakeStore) InsertAuditIdentity(e *models.AuditEventIdentity) error {
	if f.onAudit != nil {
		f.onAudit(e)
	}
	return nil
}

// storageMigrationTestRoot returns a Linux-absolute test-scoped directory.
// validateCoreStorageConfig deliberately requires a leading "/" (the config
// always names a path on the Core container's filesystem, which is Linux-only
// - see its doc comment), so a raw t.TempDir() - NOT "/"-prefixed on Windows -
// would fail that check before ResolveTarget ever reached the same-location
// comparison these tests exercise, rather than genuinely exercising it. Same
// convention as testConnectionProbeDir in core_storage_http_test.go: a
// leading "/" resolves under the current drive root on Windows (os.MkdirAll
// still succeeds there) while satisfying the string-prefix check either way.
func storageMigrationTestRoot(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(posixTestBase(), "dylaris-storage-migration-test-tmp")
	dir := filepath.ToSlash(filepath.Join(root, t.Name(), name))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return dir
}

func configuredPathStorage(t *testing.T, fs *storageMigrationFakeStore) {
	t.Helper()
	fs.kv[keyCoreStorageBackend] = "path"
	fs.kv[keyCoreStoragePath] = "/mnt/shared"
	fs.kv[keyCoreStoragePathConfirm] = "true"
}

func TestCoreStorageBackendLabel_CarriesNoCredentials(t *testing.T) {
	cases := []struct {
		name string
		cfg  CoreStorageConfig
		sub  string
		want string
	}{
		{
			"path",
			CoreStorageConfig{Backend: "path", Path: "/mnt/shared"},
			"library",
			"path:/mnt/shared/library",
		},
		{
			"s3 with prefix",
			CoreStorageConfig{Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "dylaris", S3Prefix: "prod"},
			"modpacks",
			"s3:https://s3.example.com/dylaris/prod/modpacks",
		},
		{
			"s3 without prefix",
			CoreStorageConfig{Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "dylaris"},
			"library",
			"s3:https://s3.example.com/dylaris/library",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := coreStorageBackendLabel(c.cfg, c.sub)
			if got != c.want {
				t.Fatalf("coreStorageBackendLabel = %q, want %q", got, c.want)
			}
		})
	}
}

func TestCoreStorageBackendLabel_NeverLeaksTheSecret(t *testing.T) {
	cfg := CoreStorageConfig{
		Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "b",
		S3AccessKey: "AKIAEXAMPLEKEY", S3SecretKey: "SUPERSECRETVALUE",
	}
	label := coreStorageBackendLabel(cfg, "library")
	for _, secret := range []string{"AKIAEXAMPLEKEY", "SUPERSECRETVALUE"} {
		if strings.Contains(label, secret) {
			t.Fatalf("backend label leaked a credential: %q", label)
		}
	}
}

func TestStorageDataSetResolver_ListMarksEveryDataSetMigratable(t *testing.T) {
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	fs.storages = []models.BackupStorage{
		{ID: 1, Name: "Hetzner S3", Provider: "s3"},
		{ID: 2, Name: "On the node", Provider: "node-local"},
	}
	res := NewStorageDataSetResolver(&AppState{Store: fs})

	sets, err := res.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byID := map[string]bool{}
	adHoc := map[string]bool{}
	notes := map[string]string{}
	for _, s := range sets {
		byID[s.ID] = s.Migratable
		adHoc[s.ID] = s.SupportsTargetConfig
		notes[s.ID] = s.Note
	}
	// Every blob data set is migratable, including the ones that only offer
	// the manual flow: they still capture manifests and verify.
	for _, id := range []string{CoreStorageDataSetID, "library", "ticket-attachments", "ticket-backups", "modpacks", ModpacksCoreStorageDataSetID} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("%s missing from the overview", id)
		}
		if !byID[id] {
			t.Errorf("%s is marked not migratable", id)
		}
	}
	// Exactly the data sets that OWN a settings-configured backend advertise an
	// ad-hoc target config, because advertising one is a promise to repoint
	// that backend after the copy. The four namespaces inside the shared Core
	// file storage own nothing: switching for any one of them repoints the
	// other three onto a backend their data was never copied to.
	for _, id := range []string{CoreStorageDataSetID, "modpacks"} {
		if !adHoc[id] {
			t.Errorf("%s must advertise SupportsTargetConfig: it is backed by its own settings-configured backend", id)
		}
	}
	for _, id := range []string{"library", "ticket-attachments", "ticket-backups", ModpacksCoreStorageDataSetID} {
		if adHoc[id] {
			t.Errorf("%s must NOT advertise SupportsTargetConfig: it shares one config with the other Core file storage namespaces, so switching it alone would strand them", id)
		}
		if !strings.Contains(notes[id], CoreStorageDataSetID) {
			t.Errorf("%s note = %q, want it to point the operator at the %q data set", id, notes[id], CoreStorageDataSetID)
		}
	}
	if !byID["server-backups:1"] {
		t.Error("the s3 backup storage row must be migratable")
	}
	if adHoc["server-backups:1"] {
		t.Error("a backup storage row targets another ROW, not an ad-hoc config; backups are already multi-storage")
	}
	if byID["server-backups:2"] {
		t.Error("a node-local row must NOT be migratable; those archives live on Node disks")
	}
	if notes["server-backups:2"] == "" {
		t.Error("the node-local row has no explanatory note")
	}
	if notes["modpacks"] == "" {
		t.Error("modpacks must carry the database-referenced-only note")
	}
}

// TestStorageDataSetResolver_SharedCoreStorageNamespacesCannotSwitchTheConfig
// is the refusal behind SupportsTargetConfig: false above. library,
// ticket-attachments, ticket-backups and modpacks@core-storage all read ONE
// saved Core file storage config, so letting any of them name a target config
// would repoint the others too - onto a backend their data had never been
// copied to - and strand them irrecoverably, since their source config would
// then name that new empty backend.
func TestStorageDataSetResolver_SharedCoreStorageNamespacesCannotSwitchTheConfig(t *testing.T) {
	tgt := services.StorageTargetConfig{Backend: "path", Path: "/mnt/elsewhere", PathConfirmed: true}

	for _, id := range []string{"library", "ticket-attachments", "ticket-backups", ModpacksCoreStorageDataSetID} {
		t.Run(id, func(t *testing.T) {
			fs := newStorageMigrationFakeStore()
			configuredPathStorage(t, fs)
			res := NewStorageDataSetResolver(&AppState{Store: fs})

			_, _, err := res.ResolveTarget(context.Background(), id, tgt)
			if err == nil {
				t.Fatal("ResolveTarget err = nil, want a refusal: this namespace shares its config with the others")
			}
			if !strings.Contains(err.Error(), CoreStorageDataSetID) {
				t.Errorf("err = %v, want it to name the %q data set as the way to move the whole Core file storage", err, CoreStorageDataSetID)
			}
			// The switch path has to refuse too, not just the resolve path:
			// the job calls SwitchConfig separately, after verification.
			if err := res.SwitchConfig(context.Background(), id, tgt); err == nil {
				t.Error("SwitchConfig err = nil, want the same refusal")
			}
			if fs.kv[keyCoreStoragePath] != "/mnt/shared" {
				t.Errorf("the shared config was repointed to %q by a refused switch", fs.kv[keyCoreStoragePath])
			}
		})
	}
}

func TestStorageDataSetResolver_CombinedCoreStorageIsConfigSwitchable(t *testing.T) {
	// The counterpart: the combined data set DOES own the shared config, so it
	// resolves a target and switches. It is the automatic move the four
	// namespaces above are pointed at.
	srcRoot := storageMigrationTestRoot(t, "src")
	tgtRoot := storageMigrationTestRoot(t, "tgt")
	fs := newStorageMigrationFakeStore()
	fs.kv[keyCoreStorageBackend] = "path"
	fs.kv[keyCoreStoragePath] = srcRoot
	fs.kv[keyCoreStoragePathConfirm] = "true"
	// One online Core: SwitchConfig now runs the single-Core host-path guard,
	// which needs to count instances. A nil Redis would read as "cannot verify"
	// and refuse the switch. A single-Core deployment is exactly when a
	// host-path target is legitimate.
	st := &AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")}
	res := NewStorageDataSetResolver(st)

	ds, label, err := res.ResolveTarget(context.Background(), CoreStorageDataSetID, services.StorageTargetConfig{
		Backend: "path", Path: tgtRoot, PathConfirmed: true,
	})
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if ds == nil {
		t.Fatal("ResolveTarget returned a nil DataSet")
	}
	// The combined set is rooted at the Core storage ROOT, with no sub-prefix:
	// that is what makes it enumerate library/, ticket-attachments/ and
	// ticket-backups/ together and reproduce the layout on the target.
	if label != "path:"+tgtRoot {
		t.Errorf("label = %q, want the bare target root %q (no sub-prefix)", label, "path:"+tgtRoot)
	}
	if err := res.SwitchConfig(context.Background(), CoreStorageDataSetID, services.StorageTargetConfig{
		Backend: "path", Path: tgtRoot, PathConfirmed: true,
	}); err != nil {
		t.Fatalf("SwitchConfig: %v", err)
	}
	if fs.kv[keyCoreStoragePath] != tgtRoot {
		t.Errorf("saved path = %q, want the target %q", fs.kv[keyCoreStoragePath], tgtRoot)
	}
}

func TestStorageDataSetResolver_ResolveTargetRefusesTheSourceLocation(t *testing.T) {
	// The refusal must happen here, at resolve time, so Start fails before a
	// single object is read. Discovering it mid-copy would mean the engine had
	// already rewritten source objects onto themselves.
	root := storageMigrationTestRoot(t, "root")
	fs := newStorageMigrationFakeStore()
	fs.kv[keyCoreStorageBackend] = "path"
	fs.kv[keyCoreStoragePath] = root
	fs.kv[keyCoreStoragePathConfirm] = "true"
	res := NewStorageDataSetResolver(&AppState{Store: fs})

	_, _, err := res.ResolveTarget(context.Background(), CoreStorageDataSetID, services.StorageTargetConfig{
		Backend: "path", Path: root, PathConfirmed: true,
	})
	if !errors.Is(err, ErrTargetSameLocation) {
		t.Fatalf("ResolveTarget err = %v, want ErrTargetSameLocation", err)
	}
}

func TestStorageDataSetResolver_ResolveTargetRefusesBeforeCreatingTheTargetDir(t *testing.T) {
	// ORDER, not just outcome. buildTargetStorageProvider reaches os.MkdirAll,
	// so a buildability check CREATES the target directory. It therefore has to
	// run AFTER the same-location refusal, never before - a refused migration
	// must leave the filesystem exactly as it found it. Asserting only the
	// error cannot tell the two orderings apart, so this asserts the directory.
	root := storageMigrationTestRoot(t, "root")
	fs := newStorageMigrationFakeStore()
	fs.kv[keyCoreStorageBackend] = "path"
	fs.kv[keyCoreStoragePath] = root
	fs.kv[keyCoreStoragePathConfirm] = "true"
	// modpacks-on-Core-storage as the source, so the target carries a real
	// sub-prefix directory that MkdirAll would have to create.
	fs.kv["modpack_storage_provider"] = "core-storage"
	res := NewStorageDataSetResolver(&AppState{Store: fs})

	sub := filepath.Join(root, CoreStoragePrefixModpacks)
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatalf("precondition: %q must not exist yet (stat err = %v)", sub, err)
	}

	_, _, err := res.ResolveTarget(context.Background(), "modpacks", services.StorageTargetConfig{
		Backend: "path", Path: root, PathConfirmed: true,
	})
	if !errors.Is(err, ErrTargetSameLocation) {
		t.Fatalf("ResolveTarget err = %v, want ErrTargetSameLocation", err)
	}
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatalf("a refused ResolveTarget created %q (stat err = %v); the provider was built before the same-location check", sub, err)
	}
}

func TestStorageDataSetResolver_ResolveTargetRefusesAParentOfTheModpackRoot(t *testing.T) {
	// THE aliasing case for modpacks on its own local backend. That provider
	// (modpack.LocalProvider) writes at filepath.Join(base, key) with NO
	// sub-directory, so its root IS modpack_storage_paths[0]. A target root one
	// level up therefore resolves to <target>/modpacks, which is the very same
	// directory - a parent/child pair, not a trivially different one.
	//
	// Appending the sub-prefix to BOTH sides compared "<paths[0]>/modpacks"
	// against "<target>/modpacks" and called them distinct. The copy then
	// rewrote every object onto itself, a full verify passed (it compared each
	// file with itself), the switch succeeded, and an opted-in deleteSource
	// removed the only copy.
	parent := storageMigrationTestRoot(t, "parent")
	modpackRoot := filepath.ToSlash(filepath.Join(parent, CoreStoragePrefixModpacks))
	if err := os.MkdirAll(modpackRoot, 0755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", modpackRoot, err)
	}

	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	fs.kv["modpack_storage_provider"] = "local"
	pathsJSON, _ := json.Marshal([]string{modpackRoot})
	fs.kv["modpack_storage_paths"] = string(pathsJSON)
	res := NewStorageDataSetResolver(&AppState{Store: fs})

	_, _, err := res.ResolveTarget(context.Background(), "modpacks", services.StorageTargetConfig{
		Backend: "path", Path: parent, PathConfirmed: true,
	})
	if !errors.Is(err, ErrTargetSameLocation) {
		t.Fatalf("ResolveTarget err = %v, want ErrTargetSameLocation: modpack root %q and target %q resolve to the same directory", err, modpackRoot, parent)
	}
}

func TestStorageDataSetResolver_ResolveTargetBuildsAnUnsavedTarget(t *testing.T) {
	srcRoot := storageMigrationTestRoot(t, "src")
	tgtRoot := storageMigrationTestRoot(t, "tgt")
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	fs.kv["modpack_storage_provider"] = "local"
	pathsJSON, _ := json.Marshal([]string{srcRoot})
	fs.kv["modpack_storage_paths"] = string(pathsJSON)
	res := NewStorageDataSetResolver(&AppState{Store: fs})

	ds, label, err := res.ResolveTarget(context.Background(), "modpacks", services.StorageTargetConfig{
		Backend: "path", Path: tgtRoot, PathConfirmed: true,
	})
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if ds == nil {
		t.Fatal("ResolveTarget returned a nil DataSet")
	}
	if !strings.Contains(label, tgtRoot) || !strings.Contains(label, CoreStoragePrefixModpacks) {
		t.Errorf("label = %q, want it to name the target root and the sub-prefix", label)
	}
	// The saved config must be untouched: resolving a target is not a switch.
	if fs.kv["modpack_storage_paths"] != string(pathsJSON) {
		t.Errorf("the saved config changed during ResolveTarget: %q", fs.kv["modpack_storage_paths"])
	}
}

func TestStorageDataSetResolver_ResolveTargetRejectsAnInvalidConfig(t *testing.T) {
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	res := NewStorageDataSetResolver(&AppState{Store: fs})

	if _, _, err := res.ResolveTarget(context.Background(), CoreStorageDataSetID, services.StorageTargetConfig{
		Backend: "s3", S3Bucket: "b", // no credentials
	}); err == nil {
		t.Fatal("ResolveTarget err = nil, want validateCoreStorageConfig to reject it")
	}
}

func TestStorageDataSetResolver_ModpackConfiguredPathsParsesTheStoredJSON(t *testing.T) {
	// modpack_storage_paths is written as a JSON array (ModpackSettingsHandler
	// .Set does json.Marshal(cleaned); NewProviderFromSettings reads it back
	// the same way). A comma split returns the literal bracketed, quoted JSON
	// text as a "path", which can never equal a real root - so it does not just
	// mislabel the backend, it silently defeats the same-location refusal for
	// the modpacks data set, because a garbled source path never compares equal
	// to any target.
	cases := []struct {
		name string
		raw  string
		set  bool
		want []string
	}{
		{"the real stored format", `["/data/modpacks","/mnt/mirror"]`, true, []string{"/data/modpacks", "/mnt/mirror"}},
		{"a single path", `["/data/modpacks"]`, true, []string{"/data/modpacks"}},
		{"an empty array", `[]`, true, []string{}},
		{"an empty setting", "", true, nil},
		{"the setting is absent", "", false, nil},
		{"malformed JSON is not silently split into pseudo-paths", `/data/modpacks,/mnt/mirror`, true, nil},
		{"a JSON scalar instead of an array", `"/data/modpacks"`, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newStorageMigrationFakeStore()
			if c.set {
				fs.kv["modpack_storage_paths"] = c.raw
			}
			got := NewStorageDataSetResolver(&AppState{Store: fs}).modpackConfiguredPaths()
			if len(got) != len(c.want) {
				t.Fatalf("modpackConfiguredPaths = %#v, want %#v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("modpackConfiguredPaths = %#v, want %#v", got, c.want)
				}
			}
		})
	}
}

func TestStorageDataSetResolver_ResolveTargetRejectsARowBackedDataSet(t *testing.T) {
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	fs.storages = []models.BackupStorage{{ID: 1, Name: "Hetzner S3", Provider: "s3"}}
	res := NewStorageDataSetResolver(&AppState{Store: fs})

	if _, _, err := res.ResolveTarget(context.Background(), "server-backups:1", services.StorageTargetConfig{
		Backend: "path", Path: t.TempDir(), PathConfirmed: true,
	}); err == nil {
		t.Fatal("ResolveTarget err = nil, want a refusal: server backups target another storage ROW, not an ad-hoc config")
	}
}

func TestStorageDataSetResolver_SwitchConfigPersistsTheTarget(t *testing.T) {
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	// A single online Core so checkSharedStorageReachable's short-circuit
	// applies; this test is about the persisted target shape, not the
	// reachability round.
	st := &AppState{Store: fs, Redis: multiCoreRedis(t, "core-a")}
	res := NewStorageDataSetResolver(st)

	err := res.SwitchConfig(context.Background(), CoreStorageDataSetID, services.StorageTargetConfig{
		Backend: "s3", S3Endpoint: "https://new.example.com", S3Bucket: "dylaris-new",
		S3Region: "eu-central-1", S3AccessKey: "AKIA-NEW", S3SecretKey: "NEW-SECRET",
		S3Prefix: "prod",
	})
	if err != nil {
		t.Fatalf("SwitchConfig: %v", err)
	}
	got := st.LoadCoreStorageConfig()
	if got.Backend != "s3" || got.S3Bucket != "dylaris-new" || got.S3Prefix != "prod" {
		t.Fatalf("saved config = %+v, want the target", got)
	}
	// This IS the one place the secret is allowed to land.
	if got.S3SecretKey != "NEW-SECRET" {
		t.Errorf("stored secret = %q, want the target's secret persisted here", got.S3SecretKey)
	}
}

func TestStorageMigrationHandler_StartNeverEchoesOrAuditsTheTargetSecret(t *testing.T) {
	// Safety invariant 6 at the HTTP boundary. The request carries a live
	// secret; the 400 body must not repeat it back, and neither must anything
	// the handler writes. The service is deliberately nil so this stays a pure
	// input check that cannot pass by accident of Redis being down.
	const secret = "SENTINEL-HTTP-SECRET"
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	h := NewStorageMigrationHandler(&AppState{Store: fs})

	body, _ := json.Marshal(map[string]interface{}{
		"kind": "migrate", "dataSet": "library",
		"targetConfig": map[string]interface{}{
			"backend": "s3", "s3Endpoint": "https://new.example.com",
			"s3Bucket": "dylaris-new", "s3AccessKey": "AKIA", "s3SecretKey": secret,
		},
		"verifyMode": "sample", "deleteSource": true,
	})
	rw := httptest.NewRecorder()
	h.Start(rw, httptest.NewRequest(http.MethodPost, "/api/admin/storage/migration", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("Start status = %d, want 400 (deleteSource under a sample verify)", rw.Code)
	}
	if strings.Contains(rw.Body.String(), secret) {
		t.Fatalf("the error response echoed the target secret: %s", rw.Body.String())
	}
}

func TestStorageMigrationHandler_AResolverFailureNeverEchoesTheTargetSecret(t *testing.T) {
	// The test above stops at req.Validate(), which never sees the resolver.
	// This one wires a REAL service over the REAL resolver and drives the path
	// that actually carries the credential: ResolveTarget fails, the service
	// wraps it ("target storage: %w"), and Start hands err.Error() straight to
	// sendJSONError. Anything a resolver error puts in its text is therefore
	// readable in a 400 body over HTTP, and the target config - secret included
	// - is the value in scope right where those errors are built.
	//
	// The service is constructed with nil Redis and a nil manifest store on
	// purpose: Start reaches the resolver before it touches either.
	const secret = "SENTINEL-RESOLVER-SECRET"
	root := storageMigrationTestRoot(t, "root")
	fs := newStorageMigrationFakeStore()
	fs.kv[keyCoreStorageBackend] = "path"
	fs.kv[keyCoreStoragePath] = root
	fs.kv[keyCoreStoragePathConfirm] = "true"
	state := &AppState{Store: fs}
	state.StorageMigration = services.NewStorageMigrationService(nil, nil, NewStorageDataSetResolver(state))
	h := NewStorageMigrationHandler(state)

	body, _ := json.Marshal(map[string]interface{}{
		"kind": "migrate", "dataSet": CoreStorageDataSetID,
		"targetConfig": map[string]interface{}{
			// Passes the request-shape check (backend is set) and fails inside
			// the resolver: validateCoreStorageConfig wants a bucket.
			"backend": "s3", "s3Endpoint": "https://new.example.com",
			"s3AccessKey": "AKIAEXAMPLEKEY", "s3SecretKey": secret,
		},
		"verifyMode": "full",
	})
	rw := httptest.NewRecorder()
	h.Start(rw, httptest.NewRequest(http.MethodPost, "/api/admin/storage/migration", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("Start status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	// Positive control: without this the test would still pass if Start had
	// bailed out earlier and never reached the resolver at all.
	if !strings.Contains(rw.Body.String(), "target storage") {
		t.Fatalf("the 400 did not come from the resolver path this test exists to cover: %s", rw.Body.String())
	}
	for _, leak := range []string{secret, "AKIAEXAMPLEKEY"} {
		if strings.Contains(rw.Body.String(), leak) {
			t.Errorf("a resolver error echoed %q to the caller: %s", leak, rw.Body.String())
		}
	}
}

func TestStorageMigrationAuditPayload_CarriesNoTargetSecret(t *testing.T) {
	// The audit payload is built from the request, so it is the other place a
	// secret could leak. It records the credential-free description only.
	payload := storageMigrationAuditPayload(services.StorageMigrationRequest{
		Kind: services.StorageJobMigrate, DataSet: "library", VerifyMode: "full", DeleteSource: true,
		TargetConfig: &services.StorageTargetConfig{
			Backend: "s3", S3Endpoint: "https://new.example.com", S3Bucket: "dylaris-new",
			S3AccessKey: "AKIAEXAMPLEKEY", S3SecretKey: "SENTINEL-AUDIT-SECRET", S3Prefix: "prod",
		},
	}, "job-1")

	blob, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal audit payload: %v", err)
	}
	for _, secret := range []string{"SENTINEL-AUDIT-SECRET", "AKIAEXAMPLEKEY"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("the audit payload leaks %q: %s", secret, blob)
		}
	}
	// Positive control: it still says WHERE the data went.
	if !strings.Contains(string(blob), "dylaris-new") {
		t.Errorf("the audit payload does not record the target at all: %s", blob)
	}
}

func TestStorageMigrationHandler_StartRejectsDeleteUnderSampleWith400(t *testing.T) {
	// Safety invariant 2 at the API boundary, not only in the UI. This must
	// hold even with no migration service wired, so it is a pure input check.
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	h := NewStorageMigrationHandler(&AppState{Store: fs})

	body, _ := json.Marshal(map[string]interface{}{
		"kind": "migrate", "dataSet": "modpacks", "targetDataSet": ModpacksCoreStorageDataSetID,
		"verifyMode": "sample", "deleteSource": true,
	})
	rw := httptest.NewRecorder()
	h.Start(rw, httptest.NewRequest(http.MethodPost, "/api/admin/storage/migration", bytes.NewReader(body)))

	if rw.Code != http.StatusBadRequest {
		t.Fatalf("Start status = %d, want 400 (%s)", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "full") {
		t.Errorf("the 400 body does not explain the full-verify requirement: %s", rw.Body.String())
	}
}

func TestStorageMigrationHandler_StartRejectsAnUnknownKind(t *testing.T) {
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	h := NewStorageMigrationHandler(&AppState{Store: fs})

	body, _ := json.Marshal(map[string]interface{}{"kind": "wipe", "dataSet": "modpacks"})
	rw := httptest.NewRecorder()
	h.Start(rw, httptest.NewRequest(http.MethodPost, "/api/admin/storage/migration", bytes.NewReader(body)))
	if rw.Code != http.StatusBadRequest {
		t.Fatalf("Start status = %d, want 400", rw.Code)
	}
}

func TestStorageMigrationHandler_GetJobWithoutTheServiceIs503(t *testing.T) {
	h := NewStorageMigrationHandler(&AppState{Store: newStorageMigrationFakeStore()})
	rw := httptest.NewRecorder()
	h.GetJob(rw, httptest.NewRequest(http.MethodGet, "/api/admin/storage/migration", nil))
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("GetJob status = %d, want 503 when the service is not wired", rw.Code)
	}
}

func TestStorageMigrationHandler_Overview(t *testing.T) {
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	fs.manifests = []models.StorageManifest{
		{ID: 7, DataSet: "modpacks", Algo: "sha256", CapturedAt: time.Now(), ObjectCount: 12, TotalBytes: 900},
	}
	h := NewStorageMigrationHandler(&AppState{Store: fs})

	rw := httptest.NewRecorder()
	h.Overview(rw, httptest.NewRequest(http.MethodGet, "/api/admin/storage/overview", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("Overview status = %d (%s)", rw.Code, rw.Body.String())
	}
	var got struct {
		Success  bool `json:"success"`
		DataSets []struct {
			ID             string `json:"id"`
			Label          string `json:"label"`
			BackendLabel   string `json:"backendLabel"`
			Migratable     bool   `json:"migratable"`
			Note           string `json:"note"`
			LatestManifest *struct {
				ID          int   `json:"id"`
				ObjectCount int64 `json:"objectCount"`
			} `json:"latestManifest"`
		} `json:"dataSets"`
	}
	if err := json.Unmarshal(rw.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rw.Body.String())
	}
	if !got.Success || len(got.DataSets) == 0 {
		t.Fatalf("Overview body = %s", rw.Body.String())
	}
	for _, ds := range got.DataSets {
		if ds.ID == "modpacks" {
			if ds.LatestManifest == nil || ds.LatestManifest.ID != 7 || ds.LatestManifest.ObjectCount != 12 {
				t.Errorf("modpacks latestManifest = %+v, want manifest 7 with 12 objects", ds.LatestManifest)
			}
		}
	}
}

func TestStorageMigrationHandler_ExportManifestStreamsCSV(t *testing.T) {
	fs := newStorageMigrationFakeStore()
	fs.manifests = []models.StorageManifest{{ID: 4, DataSet: "modpacks", Algo: "sha256", CapturedAt: time.Now()}}
	fs.entries[4] = []models.StorageManifestEntry{
		{ManifestID: 4, Key: "a.mrpack", Size: 3, Checksum: "aa"},
		{ManifestID: 4, Key: "b,with,commas.mrpack", Size: 5, Checksum: "bb"},
	}
	h := NewStorageMigrationHandler(&AppState{Store: fs})

	req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/admin/storage/manifests/4/export", nil), map[string]string{"id": "4"})
	rw := httptest.NewRecorder()
	h.ExportManifest(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("ExportManifest status = %d (%s)", rw.Code, rw.Body.String())
	}
	if ct := rw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := rw.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	body := rw.Body.String()
	if !strings.HasPrefix(body, "key,size,sha256\n") {
		t.Errorf("CSV header = %q", strings.SplitN(body, "\n", 2)[0])
	}
	// encoding/csv must quote the key containing commas.
	if !strings.Contains(body, `"b,with,commas.mrpack",5,bb`) {
		t.Errorf("a key containing commas was not quoted:\n%s", body)
	}
}

func TestStorageMigrationHandler_ExportManifestUnknownIDIs404(t *testing.T) {
	h := NewStorageMigrationHandler(&AppState{Store: newStorageMigrationFakeStore()})
	req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/api/admin/storage/manifests/99/export", nil), map[string]string{"id": "99"})
	rw := httptest.NewRecorder()
	h.ExportManifest(rw, req)
	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rw.Code)
	}
}

func TestStorageMigrationHandler_DeleteManifest(t *testing.T) {
	fs := newStorageMigrationFakeStore()
	fs.manifests = []models.StorageManifest{{ID: 4, DataSet: "modpacks"}}
	h := NewStorageMigrationHandler(&AppState{Store: fs})

	req := mux.SetURLVars(httptest.NewRequest(http.MethodDelete, "/api/admin/storage/manifests/4", nil), map[string]string{"id": "4"})
	rw := httptest.NewRecorder()
	h.DeleteManifest(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rw.Code, rw.Body.String())
	}
	if len(fs.deleted) != 1 || fs.deleted[0] != 4 {
		t.Errorf("deleted = %v, want [4]", fs.deleted)
	}
}

func TestStorageMigrationHandler_DeleteManifestUnknownIDIs404(t *testing.T) {
	// Consistent with ExportManifest on the same id, and no audit row: a 200
	// tells the operator a deletion happened, and an audit trail that records
	// deletions which never occurred is worse than no entry.
	fs := newStorageMigrationFakeStore()
	audited := 0
	fs.onAudit = func(*models.AuditEventIdentity) { audited++ }
	h := NewStorageMigrationHandler(&AppState{Store: fs})

	req := mux.SetURLVars(httptest.NewRequest(http.MethodDelete, "/api/admin/storage/manifests/99", nil), map[string]string{"id": "99"})
	rw := httptest.NewRecorder()
	h.DeleteManifest(rw, req)

	if rw.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (%s)", rw.Code, rw.Body.String())
	}
	if len(fs.deleted) != 0 {
		t.Errorf("deleted = %v, want no delete attempted", fs.deleted)
	}
	if audited != 0 {
		t.Errorf("audit rows written = %d, want 0 for a no-op delete", audited)
	}
}

func TestStorageMigrationHandler_ListManifests(t *testing.T) {
	fs := newStorageMigrationFakeStore()
	fs.manifests = []models.StorageManifest{
		{ID: 1, DataSet: "modpacks"},
		{ID: 2, DataSet: "library"},
	}
	h := NewStorageMigrationHandler(&AppState{Store: fs})

	rw := httptest.NewRecorder()
	h.ListManifests(rw, httptest.NewRequest(http.MethodGet, "/api/admin/storage/manifests?dataSet=library", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rw.Code, rw.Body.String())
	}
	var got struct {
		Manifests []models.StorageManifest `json:"manifests"`
	}
	_ = json.Unmarshal(rw.Body.Bytes(), &got)
	if len(got.Manifests) != 1 || got.Manifests[0].ID != 2 {
		t.Fatalf("manifests = %+v, want only the library one", got.Manifests)
	}
}

// TestStorageDataSetResolver_EnsureDistinctDataSetLocations is the row-to-row
// same-location refusal.
//
// Start used to check only that the two data-set IDS differed as strings, and
// ensureDistinctCoreStorageLocation ran only on the ad-hoc-TargetConfig path.
// A pair resolving to ONE physical location was therefore accepted: the copy
// skipped every object as already identical, the verification compared the
// location against its own manifest for a trivial 100% PASS, and finishMessage
// then told the operator "the source was left in place; remove it once you have
// switched the backend over" - about the same directory.
//
// The refusal must be narrow. Over-refusal is what this workstream fought
// repeatedly, so every legitimate shape below is a positive control that a
// blanket "different ids, refuse if unsure" rule would fail.
func TestStorageDataSetResolver_EnsureDistinctDataSetLocations(t *testing.T) {
	// The local modpack root is deliberately NOT under the Core storage root,
	// so "modpacks -> modpacks@core-storage" on the local backend is a genuine
	// move between two distinct places.
	const localModpackRoot = "/mnt/modpacks-elsewhere"

	cases := []struct {
		name string
		// setup mutates the settings the resolver reads.
		setup    func(fs *storageMigrationFakeStore)
		sourceID string
		targetID string
		// wantErr is the sentinel the pair must be refused with, or nil when the
		// pair is legitimate. The two sentinels describe different failures, so
		// asserting the specific one keeps a containment refusal from passing as
		// an equality refusal.
		wantErr error
	}{
		{
			// THE case this check exists for, and the mutation that pins it:
			// with the modpack backend pointed at Core storage, "modpacks" and
			// "modpacks@core-storage" are one directory under one config.
			name: "REFUSE modpacks to modpacks@core-storage when the modpack backend IS core storage",
			setup: func(fs *storageMigrationFakeStore) {
				fs.kv["modpack_storage_provider"] = "core-storage"
			},
			sourceID: "modpacks",
			targetID: ModpacksCoreStorageDataSetID,
			wantErr:  ErrTargetSameLocation,
		},
		{
			// The mutation control for the row above: the SAME pair on a local
			// modpack backend is the documented way to move modpacks onto Core
			// storage. Refusing it would break the feature.
			name: "PASS modpacks to modpacks@core-storage on a local modpack backend",
			setup: func(fs *storageMigrationFakeStore) {
				fs.kv["modpack_storage_provider"] = "local"
				paths, _ := json.Marshal([]string{localModpackRoot})
				fs.kv["modpack_storage_paths"] = string(paths)
			},
			sourceID: "modpacks",
			targetID: ModpacksCoreStorageDataSetID,
		},
		{
			// Likewise for an s3 modpack backend against a path-backed Core
			// storage: a path backend and an s3 backend are never one location.
			name: "PASS modpacks to modpacks@core-storage on an s3 modpack backend",
			setup: func(fs *storageMigrationFakeStore) {
				fs.kv["modpack_storage_provider"] = "s3"
				fs.kv["modpack_storage_s3_endpoint"] = "https://s3.example.com"
				fs.kv["modpack_storage_s3_bucket"] = "modpacks"
			},
			sourceID: "modpacks",
			targetID: ModpacksCoreStorageDataSetID,
		},
		{
			// Neither side is location-determinable from here: a backup row's
			// location lives in its provider-specific Config. Inequality is not
			// proof, but refusing on "unknown" would kill this documented flow.
			name: "PASS server-backups row to server-backups row (not location-determinable)",
			setup: func(fs *storageMigrationFakeStore) {
				fs.storages = []models.BackupStorage{
					{ID: 1, Name: "Hetzner", Provider: "s3"},
					{ID: 2, Name: "Backblaze", Provider: "s3"},
				}
			},
			sourceID: "server-backups:1",
			targetID: "server-backups:2",
		},
		{
			// The extension the original adjudication missed. A backup row with
			// provider "core-storage" ALWAYS lands under the same sub-prefix in
			// the shared Core file storage, whatever its Config says - Open
			// ignores the Config for that provider. This workstream allowlisted
			// "core-storage" as a backup provider, so two such rows are trivial
			// to create, and backupStorageLabel is only "Name (provider)", so
			// their labels differ by design and no label-based panel warning
			// can fire.
			name: "REFUSE core-storage backup row to core-storage backup row",
			setup: func(fs *storageMigrationFakeStore) {
				fs.storages = []models.BackupStorage{
					{ID: 1, Name: "Primary", Provider: "core-storage"},
					{ID: 2, Name: "Secondary", Provider: "core-storage"},
				}
			},
			sourceID: "server-backups:1",
			targetID: "server-backups:2",
			wantErr:  ErrTargetSameLocation,
		},
		{
			// One side determinable, one not: still not proof of equality.
			name: "PASS core-storage backup row to s3 backup row",
			setup: func(fs *storageMigrationFakeStore) {
				fs.storages = []models.BackupStorage{
					{ID: 1, Name: "Primary", Provider: "core-storage"},
					{ID: 2, Name: "Hetzner", Provider: "s3"},
				}
			},
			sourceID: "server-backups:1",
			targetID: "server-backups:2",
		},
		{
			// CONTAINMENT, refused with its own sentinel. Library keys are
			// relative to library/, so copying them to the storage ROOT rebases
			// every one of them into the live namespace beside modpacks/ and
			// server-backups/. MkdirLibraryHandler lets an operator create a
			// library folder called "modpacks", and copyOne overwrites on a
			// checksum mismatch, so the collision destroys a live object.
			name:     "REFUSE library to core-storage (library is stored inside it)",
			sourceID: "library",
			targetID: CoreStorageDataSetID,
			wantErr:  ErrTargetNestedLocation,
		},
		{
			// The same nesting the other way round: the outer data set's keys
			// get rebased INTO the inner one's live namespace.
			name:     "REFUSE core-storage to modpacks@core-storage (modpacks is stored inside it)",
			sourceID: CoreStorageDataSetID,
			targetID: ModpacksCoreStorageDataSetID,
			wantErr:  ErrTargetNestedLocation,
		},
		{
			// Two sibling namespaces under one config are two directories.
			name:     "PASS library to ticket-attachments",
			sourceID: "library",
			targetID: "ticket-attachments",
		},
		{
			// The over-refusal guard for the containment rule. The target is the
			// storage ROOT, whose empty sub-prefix nests everything - but only on
			// a SHARED root. With the modpack backend on a local path of its own,
			// these are two unrelated trees and the migration is legitimate, so
			// the sub-prefix comparison must never be reached.
			name: "PASS modpacks to core-storage when the modpack backend is a separate local root",
			setup: func(fs *storageMigrationFakeStore) {
				fs.kv["modpack_storage_provider"] = "local"
				paths, _ := json.Marshal([]string{localModpackRoot})
				fs.kv["modpack_storage_paths"] = string(paths)
			},
			sourceID: "modpacks",
			targetID: CoreStorageDataSetID,
		},
		{
			// An unknown id is not location-determinable either.
			name:     "PASS unknown id",
			sourceID: "library",
			targetID: "not-a-data-set",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := newStorageMigrationFakeStore()
			configuredPathStorage(t, fs)
			if c.setup != nil {
				c.setup(fs)
			}
			res := NewStorageDataSetResolver(&AppState{Store: fs})

			err := res.EnsureDistinctDataSetLocations(context.Background(), c.sourceID, c.targetID)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("EnsureDistinctDataSetLocations(%q, %q) err = %v, want %v", c.sourceID, c.targetID, err, c.wantErr)
				}
				for _, want := range []string{c.sourceID, c.targetID} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not name %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureDistinctDataSetLocations(%q, %q) err = %v, want nil: over-refusal breaks a documented flow", c.sourceID, c.targetID, err)
			}
		})
	}
}

// TestStorageDataSetResolver_LocationCheckIsLocationCompleteNotAStringCompare
// proves the row-to-row check reuses the LOCATION-complete comparator rather
// than comparing labels or raw path strings. Two different path strings naming
// ONE directory are exactly the shape a naive comparison misses, and the shape
// that makes the copy rewrite every object onto itself.
func TestStorageDataSetResolver_LocationCheckIsLocationCompleteNotAStringCompare(t *testing.T) {
	root := storageMigrationTestRoot(t, "root")
	coreModpacks := filepath.ToSlash(filepath.Join(root, CoreStoragePrefixModpacks))
	if err := os.MkdirAll(coreModpacks, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// A different STRING for the same directory. filepath.Clean collapses it,
	// which is the comparator's fast path; os.SameFile is what additionally
	// catches a symlink or bind-mount alias that no string rewrite can.
	alias := root + "/./" + CoreStoragePrefixModpacks + "/../" + CoreStoragePrefixModpacks

	fs := newStorageMigrationFakeStore()
	fs.kv[keyCoreStorageBackend] = "path"
	fs.kv[keyCoreStoragePath] = root
	fs.kv[keyCoreStoragePathConfirm] = "true"
	fs.kv["modpack_storage_provider"] = "local"
	paths, _ := json.Marshal([]string{alias})
	fs.kv["modpack_storage_paths"] = string(paths)
	res := NewStorageDataSetResolver(&AppState{Store: fs})

	err := res.EnsureDistinctDataSetLocations(context.Background(), "modpacks", ModpacksCoreStorageDataSetID)
	if !errors.Is(err, ErrTargetSameLocation) {
		t.Fatalf("err = %v, want ErrTargetSameLocation: %q and %q are one directory", err, alias, coreModpacks)
	}
}

// TestValidateCoreStorageConfig_RejectsCredentialsInTheS3Endpoint closes the
// live half of the sanitizer finding. validateCoreStorageConfig checked bucket,
// access key and secret only - it never parsed the endpoint - so
// "https://AKIA...:secret@minio.internal" was an ACCEPTED config whose
// credential then flowed into storage_manifests.backend_label (durable, in
// Postgres), the Redis job record, the panel job log and the manifest CSV.
func TestValidateCoreStorageConfig_RejectsCredentialsInTheS3Endpoint(t *testing.T) {
	base := func(endpoint string) CoreStorageConfig {
		return CoreStorageConfig{
			Backend: "s3", S3Bucket: "dylaris", S3AccessKey: "k", S3SecretKey: "s",
			S3Endpoint: endpoint,
		}
	}
	cases := []struct {
		name       string
		endpoint   string
		wantReject bool
	}{
		{"credentials in the endpoint", "https://AKIAEXAMPLE:supersecret@minio.internal", true},
		{"user only, no password", "https://AKIAEXAMPLE@minio.internal", true},
		// A password containing a space defeats url.Parse, which is why this
		// fails CLOSED on "@" rather than trusting a clean parse.
		{"unparseable credentials", "https://user:pass word@minio.internal", true},
		{"schemeless credentials", "AKIAEXAMPLE:supersecret@minio.internal", true},
		// Positive controls: the ordinary shapes must still be accepted.
		{"plain https endpoint", "https://s3.example.com", false},
		{"endpoint with a port", "http://minio.internal:9000", false},
		{"empty endpoint (AWS default, region selects the host)", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateCoreStorageConfig(base(c.endpoint))
			if c.wantReject {
				if err == nil {
					t.Fatalf("validateCoreStorageConfig(endpoint=%q) err = nil, want a rejection: this config persists a credential", c.endpoint)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateCoreStorageConfig(endpoint=%q) err = %v, want nil", c.endpoint, err)
			}
		})
	}
}

// TestCoreStorageBackendLabel_SanitizesAHostileEndpoint is the other half: a
// config that predates or bypasses the validation above must still not put a
// credential in a label. coreStorageBackendLabel concatenated the raw endpoint,
// while SanitizeBackendLabel - the function documented as "the ONE place in the
// codebase allowed to build that string" - had zero production callers.
func TestCoreStorageBackendLabel_SanitizesAHostileEndpoint(t *testing.T) {
	const secret = "supersecretvalue"
	cases := []struct {
		name     string
		endpoint string
		sub      string
	}{
		{"userinfo in a schemed endpoint", "https://AKIAEXAMPLE:" + secret + "@minio.internal", "library"},
		{"userinfo in a schemeless endpoint", "AKIAEXAMPLE:" + secret + "@minio.internal", "library"},
		{"unparseable userinfo fails closed", "https://user:" + secret + " x@minio.internal", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label := coreStorageBackendLabel(CoreStorageConfig{
				Backend: "s3", S3Endpoint: c.endpoint, S3Bucket: "dylaris",
				S3AccessKey: "AKIAEXAMPLE", S3SecretKey: secret,
			}, c.sub)
			if strings.Contains(label, secret) {
				t.Fatalf("label %q leaked the credential from endpoint %q", label, c.endpoint)
			}
		})
	}
}

// TestModpackBackendLabel_SanitizesAHostileEndpoint covers the second live
// label builder. The modpack s3 endpoint is operator-supplied too and its label
// reaches the same manifest rows, job records and CSV exports.
func TestModpackBackendLabel_SanitizesAHostileEndpoint(t *testing.T) {
	const secret = "modpacksecret"
	fs := newStorageMigrationFakeStore()
	configuredPathStorage(t, fs)
	fs.kv["modpack_storage_provider"] = "s3"
	fs.kv["modpack_storage_s3_endpoint"] = "https://AKIAEXAMPLE:" + secret + "@minio.internal"
	fs.kv["modpack_storage_s3_bucket"] = "modpacks"
	res := NewStorageDataSetResolver(&AppState{Store: fs})

	if label := res.modpackBackendLabel(); strings.Contains(label, secret) {
		t.Fatalf("modpack backend label %q leaked the credential", label)
	}
}

// TestSubPrefixContains pins the nesting predicate the containment refusal is
// built on. Equality must return false: that is the same-location case, and
// letting it answer true here would replace a precise refusal message with a
// vaguer one. The match is segment-aware so a sibling whose name merely starts
// with another data set's name is not read as living inside it.
func TestSubPrefixContains(t *testing.T) {
	cases := []struct {
		name  string
		outer string
		inner string
		want  bool
	}{
		{"the root contains every data set", "", "library", true},
		{"the root contains a nested data set", "", "modpacks", true},
		{"a data set does not contain the root", "library", "", false},
		{"equality is not containment", "library", "library", false},
		{"the root is not nested in itself", "", "", false},
		{"siblings do not contain each other", "library", "modpacks", false},
		{"a name prefix is not containment", "modpacks", "modpacks-archive", false},
		{"a real nested path is containment", "modpacks", "modpacks/vanilla", true},
		{"surrounding slashes do not change the answer", "/library/", "library", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := subPrefixContains(c.outer, c.inner); got != c.want {
				t.Errorf("subPrefixContains(%q, %q) = %v, want %v", c.outer, c.inner, got, c.want)
			}
		})
	}
}
