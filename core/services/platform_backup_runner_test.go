package services

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services/bundle"
)

const runnerPassphrase = "the documented backup passphrase"

// ---------- fakes ----------

type fakeRunnerStore struct {
	job        *models.PlatformBackupJob
	passphrase string
	storage    *models.BackupStorage
	servers    []models.BackupTargetServer

	openedRuns int
	finished   []finishedRun
}

type finishedRun struct {
	id         int
	status     string
	size       int64
	key        string
	errMessage string
	components []models.PlatformBackupComponent
}

func (f *fakeRunnerStore) GetPlatformBackupJob(int) (*models.PlatformBackupJob, error) {
	return f.job, nil
}
func (f *fakeRunnerStore) GetSetting(key string) (string, error) {
	if key == PlatformBackupPassphraseSetting {
		return f.passphrase, nil
	}
	return "", nil
}
func (f *fakeRunnerStore) GetBackupStorage(int) (*models.BackupStorage, error) { return f.storage, nil }
func (f *fakeRunnerStore) GetDefaultBackupStorage() (*models.BackupStorage, error) {
	return f.storage, nil
}
func (f *fakeRunnerStore) GetUserDefaultBackupStorage(string) (*models.BackupStorage, error) {
	return nil, nil
}
func (f *fakeRunnerStore) ListBackupTargetServers() ([]models.BackupTargetServer, error) {
	return f.servers, nil
}
func (f *fakeRunnerStore) CreatePlatformBackupRun(int, *int) (int, error) {
	f.openedRuns++
	return f.openedRuns, nil
}
func (f *fakeRunnerStore) FinishPlatformBackupRun(id int, status string, size int64,
	key, errMessage string, components []models.PlatformBackupComponent) error {
	f.finished = append(f.finished, finishedRun{id, status, size, key, errMessage, components})
	return nil
}

type fakeDest struct {
	key  string
	body []byte
}

func (d *fakeDest) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	d.key = key
	b, err := io.ReadAll(r)
	d.body = b
	return err
}

type fakeArea struct {
	files   map[string]string
	walkErr error
	openErr error
}

func (a *fakeArea) Walk(context.Context) ([]BundleFile, error) {
	if a.walkErr != nil {
		return nil, a.walkErr
	}
	// Sorted so a test can name what it expects.
	keys := make([]string, 0, len(a.files))
	for k := range a.files {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	out := make([]BundleFile, 0, len(keys))
	for _, k := range keys {
		out = append(out, BundleFile{Key: k, Size: int64(len(a.files[k]))})
	}
	return out, nil
}

func (a *fakeArea) Open(_ context.Context, key string) (io.ReadCloser, error) {
	if a.openErr != nil {
		return nil, a.openErr
	}
	return io.NopCloser(strings.NewReader(a.files[key])), nil
}

// ---------- helpers ----------

func newRunner(t *testing.T, st *fakeRunnerStore, dest *fakeDest) *PlatformBackupRunner {
	t.Helper()
	return &PlatformBackupRunner{
		Store:         st,
		Release:       "2026.09.08",
		ClusterSecret: "the-source-cluster-secret",
		WorkDir:       t.TempDir(),
		OpenDest: func(context.Context, *models.BackupStorage) (BundleDestination, error) {
			return dest, nil
		},
	}
}

func storeWith(sel models.PlatformBackupSelection) *fakeRunnerStore {
	return &fakeRunnerStore{
		job: &models.PlatformBackupJob{
			ID: 3, Name: "nightly", Schedule: "manual", Enabled: true, Selection: sel,
		},
		passphrase: runnerPassphrase,
		storage:    &models.BackupStorage{ID: 11, Name: "R2", Provider: "s3"},
	}
}

// entries opens the bundle the run uploaded and returns its manifest plus the
// names and bodies of every member after it.
func entries(t *testing.T, raw []byte) (*bundle.Manifest, map[string]string) {
	t.Helper()
	_, m, tr, err := bundle.Open(bytes.NewReader(raw), runnerPassphrase)
	if err != nil {
		t.Fatalf("opening the uploaded bundle: %v", err)
	}
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the bundle: %v", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, _ := io.ReadAll(tr)
		out[h.Name] = string(b)
	}
	return m, out
}

// ---------- tests ----------

// A bundle with no passphrase is worse than no bundle, because it is trusted.
// Refused BEFORE a run row exists, so a run that could never have produced
// anything does not appear as one that failed.
func TestPlatformBackupRefusesWithoutAPassphrase(t *testing.T) {
	st := storeWith(models.PlatformBackupSelection{Library: true})
	st.passphrase = ""
	dest := &fakeDest{}

	_, err := newRunner(t, st, dest).Run(context.Background(), 3)
	if !errors.Is(err, ErrNoBackupPassphrase) {
		t.Fatalf("err = %v, want ErrNoBackupPassphrase", err)
	}
	if st.openedRuns != 0 {
		t.Error("a run row was opened for a backup that could never run")
	}
	if dest.body != nil {
		t.Error("something was uploaded")
	}
}

func TestPlatformBackupRefusesASelectionThatCoversNothing(t *testing.T) {
	st := storeWith(models.PlatformBackupSelection{})
	if _, err := newRunner(t, st, &fakeDest{}).Run(context.Background(), 3); err == nil {
		t.Fatal("a run covering nothing was accepted")
	}
	if st.openedRuns != 0 {
		t.Error("a run row was opened for an empty selection")
	}
}

func TestPlatformBackupWritesAnOpenableBundle(t *testing.T) {
	sel := models.PlatformBackupSelection{Library: true, Modpacks: true}
	st := storeWith(sel)
	dest := &fakeDest{}
	r := newRunner(t, st, dest)
	r.OpenCoreStorage = func(area string) (CoreStorageArea, error) {
		switch area {
		case "library":
			return &fakeArea{files: map[string]string{"paper-1.21.4.jar": "JAR BYTES"}}, nil
		default:
			return &fakeArea{files: map[string]string{"packs/sky/1.0.mrpack": "PACK"}}, nil
		}
	}

	if _, err := r.Run(context.Background(), 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if dest.key == "" || dest.body == nil {
		t.Fatal("nothing was uploaded")
	}

	m, members := entries(t, dest.body)
	// The old cluster secret has to travel: the database in a bundle holds
	// values encrypted under it, and the warp leader key is derived from it and
	// stored nowhere.
	if m.ClusterSecret != "the-source-cluster-secret" {
		t.Errorf("the source cluster secret did not travel: %q", m.ClusterSecret)
	}
	if !m.Selection.Library || !m.Selection.Modpacks {
		t.Errorf("the manifest does not describe the selection: %+v", m.Selection)
	}
	if members["library/paper-1.21.4.jar"] != "JAR BYTES" {
		t.Errorf("library member = %q", members["library/paper-1.21.4.jar"])
	}
	if members["modpacks/packs/sky/1.0.mrpack"] != "PACK" {
		t.Errorf("modpack member = %q", members["modpacks/packs/sky/1.0.mrpack"])
	}
}

// A selection outlives what it names. A server deleted long after somebody
// ticked it is an ordinary outcome of a healthy run, and must never take the
// database backup down with it.
func TestPlatformBackupSkipsADeletedServerAndStillSucceeds(t *testing.T) {
	sel := models.PlatformBackupSelection{
		Library: true,
		Servers: models.PlatformBackupServers{
			Mode: models.PlatformBackupServersList, ServerIDs: []int{1, 404},
		},
	}
	st := storeWith(sel)
	st.servers = []models.BackupTargetServer{{ID: 1, UUID: "srv-1", Name: "alpha"}}
	dest := &fakeDest{}
	r := newRunner(t, st, dest)
	r.OpenCoreStorage = func(string) (CoreStorageArea, error) { return &fakeArea{}, nil }
	var triggered []int
	r.TriggerServerBackup = func(_ context.Context, id int) (string, error) {
		triggered = append(triggered, id)
		return "run 77", nil
	}

	if _, err := r.Run(context.Background(), 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(st.finished) != 1 || st.finished[0].status != "success" {
		t.Fatalf("run = %+v, want one successful run", st.finished)
	}
	if len(triggered) != 1 || triggered[0] != 1 {
		t.Errorf("triggered = %v, want only the server that exists", triggered)
	}

	var skipped, included int
	for _, c := range st.finished[0].components {
		if c.Kind != "server" {
			continue
		}
		switch c.Status {
		case models.PlatformBackupSkipped:
			skipped++
			if c.Ref != "404" {
				t.Errorf("skipped the wrong server: %q", c.Ref)
			}
		case models.PlatformBackupIncluded:
			included++
		}
	}
	if skipped != 1 || included != 1 {
		t.Errorf("components = %+v; want one skipped and one included server", st.finished[0].components)
	}
}

// One server whose node is offline must not cost the operator the database
// backup that was the point of the run.
func TestPlatformBackupSurvivesAServerThatCannotBeTriggered(t *testing.T) {
	sel := models.PlatformBackupSelection{
		Library: true,
		Servers: models.PlatformBackupServers{Mode: models.PlatformBackupServersAll},
	}
	st := storeWith(sel)
	st.servers = []models.BackupTargetServer{{ID: 1, UUID: "srv-1"}, {ID: 2, UUID: "srv-2"}}
	r := newRunner(t, st, &fakeDest{})
	r.OpenCoreStorage = func(string) (CoreStorageArea, error) { return &fakeArea{}, nil }
	r.TriggerServerBackup = func(_ context.Context, id int) (string, error) {
		if id == 2 {
			return "", errors.New("node offline")
		}
		return "run 5", nil
	}

	if _, err := r.Run(context.Background(), 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.finished[0].status != "success" {
		t.Fatalf("status = %q; one unreachable server must not fail the run", st.finished[0].status)
	}
	var failed int
	for _, c := range st.finished[0].components {
		if c.Kind == "server" && c.Status == models.PlatformBackupFailed {
			failed++
			if c.Ref != "srv-2" || c.Message == "" {
				t.Errorf("the failed server is not identified: %+v", c)
			}
		}
	}
	if failed != 1 {
		t.Errorf("components = %+v; want exactly one failed server", st.finished[0].components)
	}
}

// A dump that only fails at RESTORE time is worse than none, because it is
// relied on until the hour it is needed. The metrics database is TimescaleDB,
// so it is recorded as skipped rather than half-taken.
func TestPlatformBackupRecordsMetricsAsSkipped(t *testing.T) {
	sel := models.PlatformBackupSelection{Library: true, MetricsDB: true}
	st := storeWith(sel)
	r := newRunner(t, st, &fakeDest{})
	r.OpenCoreStorage = func(string) (CoreStorageArea, error) { return &fakeArea{}, nil }

	if _, err := r.Run(context.Background(), 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var found bool
	for _, c := range st.finished[0].components {
		if c.Kind == "metrics" {
			found = true
			if c.Status != models.PlatformBackupSkipped {
				t.Errorf("metrics status = %q, want skipped", c.Status)
			}
			if c.Message == "" {
				t.Error("metrics was skipped with no reason given")
			}
		}
	}
	if !found {
		t.Error("a selected metrics database was silently omitted from the record")
	}
}

// A run that dies part way must still say which components it had. Reporting
// only the failure would leave an operator unable to tell a run that failed
// immediately from one that failed on the last file.
func TestPlatformBackupRecordsWhatItHadWhenItFails(t *testing.T) {
	sel := models.PlatformBackupSelection{
		Library:  true,
		Modpacks: true,
		Servers:  models.PlatformBackupServers{Mode: models.PlatformBackupServersList, ServerIDs: []int{404}},
	}
	st := storeWith(sel)
	r := newRunner(t, st, &fakeDest{})
	r.OpenCoreStorage = func(area string) (CoreStorageArea, error) {
		if area == "modpacks" {
			return &fakeArea{walkErr: errors.New("bucket unreachable")}, nil
		}
		return &fakeArea{files: map[string]string{"a.jar": "A"}}, nil
	}

	if _, err := r.Run(context.Background(), 3); err == nil {
		t.Fatal("a failing component did not fail the run")
	}
	if len(st.finished) != 1 || st.finished[0].status != "failed" {
		t.Fatalf("run = %+v, want one failed run", st.finished)
	}
	if st.finished[0].errMessage == "" {
		t.Error("a failed run recorded no reason")
	}
	kinds := map[string]models.PlatformBackupComponentStatus{}
	for _, c := range st.finished[0].components {
		kinds[c.Kind] = c.Status
	}
	if kinds["library"] != models.PlatformBackupIncluded {
		t.Errorf("the component that DID succeed was not recorded: %+v", st.finished[0].components)
	}
	if kinds["modpacks"] != models.PlatformBackupFailed {
		t.Errorf("the failing component was not recorded as failed: %+v", st.finished[0].components)
	}
	// The skipped server was known before the failure and must survive it.
	if kinds["server"] != models.PlatformBackupSkipped {
		t.Errorf("the skipped server was lost: %+v", st.finished[0].components)
	}
}

func TestPlatformBackupKeyNamesTheJob(t *testing.T) {
	st := storeWith(models.PlatformBackupSelection{Library: true})
	dest := &fakeDest{}
	r := newRunner(t, st, dest)
	r.OpenCoreStorage = func(string) (CoreStorageArea, error) { return &fakeArea{}, nil }

	if _, err := r.Run(context.Background(), 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.HasPrefix(dest.key, "platform-backups/3/") {
		t.Errorf("key = %q, want it under the job's own prefix", dest.key)
	}
	if !strings.HasSuffix(dest.key, ".dylaris-bundle") {
		t.Errorf("key = %q, want a bundle suffix", dest.key)
	}
	if st.finished[0].key != dest.key {
		t.Errorf("the run records key %q but the object went to %q", st.finished[0].key, dest.key)
	}
	if st.finished[0].size != int64(len(dest.body)) {
		t.Errorf("the run records %d bytes, the object is %d", st.finished[0].size, len(dest.body))
	}
}

// The spool file is the whole bundle on local disk. Leaving it behind fills the
// data volume one run at a time, and the copy is unencrypted only in the sense
// that it is a second copy - it is the same bytes, in a place nothing prunes.
func TestPlatformBackupLeavesNoSpoolBehind(t *testing.T) {
	st := storeWith(models.PlatformBackupSelection{Library: true})
	r := newRunner(t, st, &fakeDest{})
	r.OpenCoreStorage = func(string) (CoreStorageArea, error) { return &fakeArea{}, nil }

	if _, err := r.Run(context.Background(), 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	left, err := os.ReadDir(r.WorkDir)
	if err != nil {
		t.Fatalf("reading the work directory: %v", err)
	}
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("the work directory still holds %v", names)
	}
}
