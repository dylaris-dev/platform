package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services/bundle"
	"dylaris-core/store"
)

const restorePass = "a documented backup passphrase"

// ---------- fakes ----------

type fakeWriter struct {
	area    string
	written map[string]string
	err     error
}

func (w *fakeWriter) Write(_ context.Context, key string, r io.Reader) error {
	if w.err != nil {
		return w.err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	w.written[key] = string(b)
	return nil
}

type fakeReseal struct {
	from, to string
	calls    int
	report   *store.ResealReport
	err      error
}

func (f *fakeReseal) ResealAtRest(from, to string) (*store.ResealReport, error) {
	f.calls++
	f.from, f.to = from, to
	if f.err != nil {
		return nil, f.err
	}
	if f.report != nil {
		return f.report, nil
	}
	return &store.ResealReport{Buckets: []store.ResealBucket{{Name: "node-redis-secret", Moved: 2}}}, nil
}

// ---------- helpers ----------

// writeTestBundle produces a real bundle, so the restore is exercised against
// the format rather than against a hand-built tar.
func writeTestBundle(t *testing.T, clusterSecret string, members map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := bundle.NewWriter(&out, restorePass, "2026.09.08")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteManifest(&bundle.Manifest{
		Schema:        bundle.Schema,
		Source:        "2026.09.08",
		ClusterSecret: clusterSecret,
		Selection:     models.PlatformBackupSelection{Database: true, Library: true},
	}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	for name, body := range members {
		if err := w.AddBytes(name, []byte(body)); err != nil {
			t.Fatalf("AddBytes %s: %v", name, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes()
}

type restoreRig struct {
	r        *PlatformRestorer
	lib      *fakeWriter
	packs    *fakeWriter
	reseal   *fakeReseal
	dbLoaded string
	dbCalls  int
	empty    bool
}

func newRig(t *testing.T, clusterSecret string) *restoreRig {
	t.Helper()
	rig := &restoreRig{
		lib:    &fakeWriter{area: "library", written: map[string]string{}},
		packs:  &fakeWriter{area: "modpacks", written: map[string]string{}},
		reseal: &fakeReseal{},
		empty:  true,
	}
	rig.r = &PlatformRestorer{
		ClusterSecret: clusterSecret,
		WorkDir:       t.TempDir(),
		RestoreDatabaseInto: func(_ context.Context, src io.Reader) error {
			rig.dbCalls++
			b, err := io.ReadAll(src)
			rig.dbLoaded = string(b)
			return err
		},
		TargetIsEmpty: func(context.Context) (bool, error) { return rig.empty, nil },
		OpenTarget: func(context.Context) (ResealTarget, io.Closer, error) {
			return rig.reseal, nil, nil
		},
		OpenCoreStorageWriter: func(area string) (CoreStorageWriter, error) {
			if area == "library" {
				return rig.lib, nil
			}
			return rig.packs, nil
		},
	}
	return rig
}

func all() PlatformRestoreSelection {
	return PlatformRestoreSelection{Database: true, Library: true, Modpacks: true}
}

// ---------- tests ----------

func TestRestorePutsEveryPartBack(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{
		"database.dump":            "PGDMP...",
		"library/paper-1.21.4.jar": "JAR",
		"modpacks/sky/1.0.mrpack":  "PACK",
	})
	rig := newRig(t, "this-instances-secret")

	res, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass, all(), false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rig.dbLoaded != "PGDMP..." {
		t.Errorf("the dump reaching the target = %q", rig.dbLoaded)
	}
	if rig.lib.written["paper-1.21.4.jar"] != "JAR" {
		t.Errorf("library = %+v", rig.lib.written)
	}
	if rig.packs.written["sky/1.0.mrpack"] != "PACK" {
		t.Errorf("modpacks = %+v", rig.packs.written)
	}
	if res.Source != "2026.09.08" {
		t.Errorf("source = %q", res.Source)
	}
}

// The whole reason the verifier lives in the plaintext header: a restore refuses
// a wrong passphrase before it decrypts a byte, let alone writes one.
func TestRestoreRefusesTheWrongPassphraseBeforeWritingAnything(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{
		"database.dump": "PGDMP...",
		"library/a.jar": "JAR",
	})
	rig := newRig(t, "this-instances-secret")

	if _, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), "wrong one entirely", all(), false); err == nil {
		t.Fatal("a wrong passphrase was accepted")
	}
	if rig.dbCalls != 0 {
		t.Error("the target database was written to")
	}
	if len(rig.lib.written) != 0 {
		t.Error("Core storage was written to")
	}
	if rig.reseal.calls != 0 {
		t.Error("the reseal ran")
	}
}

// The at-rest values in the dump only open under the secret that wrote them.
// Without this step the restore looks like it worked and nothing does: every
// node fails authentication and every provider build fails.
func TestRestoreResealsFromTheBundlesSecretToThisOne(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{"database.dump": "PGDMP..."})
	rig := newRig(t, "this-instances-secret")

	res, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Database: true}, false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rig.reseal.calls != 1 {
		t.Fatalf("reseal ran %d times, want 1", rig.reseal.calls)
	}
	if rig.reseal.from != "source-secret" || rig.reseal.to != "this-instances-secret" {
		t.Errorf("resealed %q -> %q", rig.reseal.from, rig.reseal.to)
	}
	if res.Reseal == nil {
		t.Error("the reseal report was not returned")
	}
	// The operator has to be told that this Core is still on its own database.
	if len(res.Warnings) == 0 {
		t.Error("a database restore said nothing about the restart it needs")
	}
}

// Restoring the storage alone must not touch a database, and must not reseal
// anything: there is no restored database to reseal.
func TestRestoreOfStorageAloneLeavesTheDatabaseAlone(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{
		"database.dump": "PGDMP...",
		"library/a.jar": "JAR",
		"modpacks/b":    "PACK",
	})
	rig := newRig(t, "this-instances-secret")

	res, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Library: true}, false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rig.dbCalls != 0 {
		t.Error("an unselected database was restored")
	}
	if rig.reseal.calls != 0 {
		t.Error("the reseal ran without a restored database")
	}
	if len(rig.packs.written) != 0 {
		t.Errorf("an unselected component was restored: %+v", rig.packs.written)
	}
	if rig.lib.written["a.jar"] != "JAR" {
		t.Errorf("library = %+v", rig.lib.written)
	}
	if res.Reseal != nil {
		t.Error("a reseal report appeared for a restore that resealed nothing")
	}
}

// A bundle is a file somebody handed us and may have been assembled by hand.
func TestRestoreSkipsEntriesThatWouldWriteOutsideTheirArea(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{
		"library/../../etc/passwd": "ROOT",
		"library/ok.jar":           "JAR",
	})
	rig := newRig(t, "this-instances-secret")

	res, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Library: true}, false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for key := range rig.lib.written {
		if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
			t.Fatalf("an escaping key was written: %q", key)
		}
	}
	if rig.lib.written["ok.jar"] != "JAR" {
		t.Errorf("the safe entry was not written: %+v", rig.lib.written)
	}
	// Skipped silently is not good enough: the operator asked for a restore and
	// got less than the bundle held.
	if len(res.Warnings) == 0 {
		t.Error("an unsafe entry was skipped without saying so")
	}
}

// A restore over an existing schema leaves rows from two installations in one
// database, with nothing afterwards saying which came from where.
func TestRestoreRefusesANonEmptyTargetUnlessTold(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{"database.dump": "PGDMP..."})

	rig := newRig(t, "this-instances-secret")
	rig.empty = false
	if _, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Database: true}, false); !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("err = %v, want ErrTargetNotEmpty", err)
	}
	if rig.dbCalls != 0 {
		t.Error("the non-empty target was written to anyway")
	}

	// And with the operator's confirmation it proceeds.
	rig2 := newRig(t, "this-instances-secret")
	rig2.empty = false
	if _, err := rig2.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Database: true}, true); err != nil {
		t.Fatalf("a confirmed overwrite was refused: %v", err)
	}
	if rig2.dbCalls != 1 {
		t.Error("a confirmed overwrite did not restore")
	}
}

// An archive written before the cluster secret travelled, or one assembled by
// hand. The restore must SAY what the operator now has to redo rather than
// leaving them with credentials nothing can read and no explanation.
func TestRestoreWarnsWhenTheBundleCarriesNoClusterSecret(t *testing.T) {
	raw := writeTestBundle(t, "", map[string]string{"database.dump": "PGDMP..."})
	rig := newRig(t, "this-instances-secret")

	res, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Database: true}, false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rig.reseal.calls != 0 {
		t.Error("a reseal was attempted with no source secret")
	}
	var mentioned bool
	for _, wmsg := range res.Warnings {
		if strings.Contains(wmsg, "re-pair") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("warnings = %v; the operator was not told what they have to redo", res.Warnings)
	}
}

// Restoring onto the SAME installation is the ordinary rollback case. There is
// nothing to move, and ResealAtRest refuses a no-op rotation, so asking it would
// turn a healthy restore into an error.
func TestRestoreSkipsTheResealWhenTheSecretIsUnchanged(t *testing.T) {
	raw := writeTestBundle(t, "same-secret", map[string]string{"database.dump": "PGDMP..."})
	rig := newRig(t, "same-secret")

	if _, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Database: true}, false); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rig.reseal.calls != 0 {
		t.Errorf("the reseal ran %d times for an unchanged secret", rig.reseal.calls)
	}
}

// A value nothing can open is left exactly as it is, and the operator is told.
// Silence here means a credential that reads as empty for the rest of the
// installation's life with nobody knowing why.
func TestRestoreReportsCredentialsItCouldNotReEncrypt(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{"database.dump": "PGDMP..."})
	rig := newRig(t, "this-instances-secret")
	rig.reseal.report = &store.ResealReport{Buckets: []store.ResealBucket{
		{Name: "node-redis-secret", Moved: 3},
		{Name: "modrinth-pat", Unreadable: 2},
	}}

	res, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Database: true}, false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	var found bool
	for _, wmsg := range res.Warnings {
		if strings.Contains(wmsg, "2 stored credentials") {
			found = true
		}
	}
	if !found {
		t.Errorf("warnings = %v; unreadable credentials were not surfaced", res.Warnings)
	}
}

func TestRestoreRefusesASelectionThatDoesNothing(t *testing.T) {
	raw := writeTestBundle(t, "s", map[string]string{"database.dump": "PGDMP..."})
	rig := newRig(t, "t")
	if _, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{}, false); !errors.Is(err, ErrNothingSelected) {
		t.Fatalf("err = %v, want ErrNothingSelected", err)
	}
}

// Identifying a bundle is not the same as opening it: an operator holding three
// of them needs to know which is which before typing a passphrase.
func TestInspectReadsTheHeaderWithoutAPassphrase(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{"database.dump": "PGDMP..."})

	h, m, err := Inspect(bytes.NewReader(raw), "")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if h.Source != "2026.09.08" || h.CreatedAt.IsZero() {
		t.Errorf("header = %+v", h)
	}
	if m != nil {
		t.Error("the manifest was returned without a passphrase")
	}

	h, m, err = Inspect(bytes.NewReader(raw), restorePass)
	if err != nil {
		t.Fatalf("Inspect with the passphrase: %v", err)
	}
	if m == nil || !m.Selection.Database {
		t.Errorf("manifest = %+v", m)
	}
	if h == nil {
		t.Error("the header was withheld")
	}
}

func TestSafeBundleKey(t *testing.T) {
	cases := map[string]bool{
		"paper.jar":      true,
		"a/b/c.jar":      true,
		"a/./b.jar":      true,
		"":               false,
		"..":             false,
		"../etc/passwd":  false,
		"a/../../etc/pw": false,
		"/etc/passwd":    false,
	}
	for key, want := range cases {
		if got := safeBundleKey(key); got != want {
			t.Errorf("safeBundleKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// Measured on production when a backup was first restored for real: the target
// role was an ordinary one, the new database had inherited TimescaleDB from
// template1, and the restore died on its first statement with
//
//	pg_restore: error: could not execute query: ERROR:  must be owner of
//	extension timescaledb; Command was: DROP EXTENSION IF EXISTS timescaledb;
//
// which says nothing about what to do. The failure is now named, and it names
// the fix. It is recognised from the failure rather than gated beforehand,
// because a platform without TimescaleDB restores fine with an ordinary role.
func TestRestoreNamesTheSuperuserRequirement(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{"database.dump": "PGDMP..."})

	for _, raw0 := range []string{
		"pg_restore: exit status 1: pg_restore: error: could not execute query: ERROR:  must be owner of extension timescaledb; Command was: DROP EXTENSION IF EXISTS timescaledb;",
		"pg_restore: exit status 1: pg_restore: error: could not execute query: ERROR:  permission denied to create extension \"timescaledb\"",
	} {
		rig := newRig(t, "this-instances-secret")
		rig.r.RestoreDatabaseInto = func(context.Context, io.Reader) error { return errors.New(raw0) }
		_, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
			PlatformRestoreSelection{Database: true}, false)
		if !errors.Is(err, ErrTargetNeedsSuperuser) {
			t.Errorf("err = %v, want ErrTargetNeedsSuperuser", err)
			continue
		}
		if !strings.Contains(err.Error(), "superuser") || !strings.Contains(err.Error(), "OWNER TO") {
			t.Errorf("the message does not say what to do: %v", err)
		}
		// The raw line is kept beside it, for whoever has to look deeper.
		if !strings.Contains(err.Error(), "timescaledb") {
			t.Errorf("the original pg_restore error was dropped: %v", err)
		}
	}

	// Any other failure stays what it was - this must not turn every broken
	// restore into advice about superusers.
	rig := newRig(t, "this-instances-secret")
	rig.r.RestoreDatabaseInto = func(context.Context, io.Reader) error {
		return errors.New("pg_restore: exit status 1: connection refused")
	}
	_, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Database: true}, false)
	if err == nil || errors.Is(err, ErrTargetNeedsSuperuser) {
		t.Errorf("an unrelated failure was relabelled: %v", err)
	}
}

// A bundle written by the runner carries the statistics dump and the TimescaleDB
// version it came from, and the version reaches the restore BEFORE the dump does,
// so a mismatched target can be refused without loading anything.
func TestRestoreBringsBackTheMetricsDBWithItsVersion(t *testing.T) {
	st := storeWith(models.PlatformBackupSelection{MetricsDB: true})
	dest := &fakeDest{}
	w := newRunner(t, st, dest)
	w.DumpMetrics = func(_ context.Context, out io.Writer) (string, error) {
		_, err := io.WriteString(out, "PGDMP-metrics")
		return "2.17.2", err
	}
	if _, err := w.Run(context.Background(), 3); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The runner and this file use different passphrases; reopen under its own.
	rig := newRig(t, "the-source-cluster-secret")
	var gotBody, gotVersion string
	rig.r.RestoreMetricsInto = func(_ context.Context, src io.Reader, v string) error {
		b, err := io.ReadAll(src)
		gotBody, gotVersion = string(b), v
		return err
	}
	res, err := rig.r.Restore(context.Background(), bytes.NewReader(dest.body), runnerPassphrase,
		PlatformRestoreSelection{MetricsDB: true}, false)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if gotBody != "PGDMP-metrics" || gotVersion != "2.17.2" {
		t.Errorf("restored %q at version %q", gotBody, gotVersion)
	}
	if rig.dbCalls != 0 {
		t.Error("restoring only the statistics database touched the platform database")
	}
	if len(res.Components) != 1 || res.Components[0].Kind != MetricsDBComponent ||
		res.Components[0].Status != models.PlatformBackupIncluded {
		t.Errorf("components = %+v", res.Components)
	}

	// A failing load is reported per component and fails the restore.
	rig = newRig(t, "the-source-cluster-secret")
	rig.r.RestoreMetricsInto = func(context.Context, io.Reader, string) error {
		return ErrMetricsTargetNotReady
	}
	res, err = rig.r.Restore(context.Background(), bytes.NewReader(dest.body), runnerPassphrase,
		PlatformRestoreSelection{MetricsDB: true}, false)
	if !errors.Is(err, ErrMetricsTargetNotReady) || len(res.Components) != 1 ||
		res.Components[0].Status != models.PlatformBackupFailed {
		t.Errorf("err = %v, components = %+v", err, res.Components)
	}
}

// writeMetricsBundle is a bundle with the platform database and the statistics
// dump, in the order the runner writes them, and a manifest that lists both.
func writeMetricsBundle(t *testing.T, clusterSecret string) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := bundle.NewWriter(&out, restorePass, "2026.09.08")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteManifest(&bundle.Manifest{
		Schema: bundle.Schema, Source: "2026.09.08", ClusterSecret: clusterSecret,
		Selection: models.PlatformBackupSelection{Database: true, MetricsDB: true},
		Components: []models.PlatformBackupComponent{
			{Kind: "database", Status: models.PlatformBackupIncluded},
			{Kind: MetricsDBComponent, Status: models.PlatformBackupIncluded},
		},
	}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	for _, m := range [][2]string{{"database.dump", "PGDMP..."}, {metricsVersionMember, "2.17.2"}, {metricsDumpMember, "PGDMP-metrics"}} {
		if err := w.AddBytes(m[0], []byte(m[1])); err != nil {
			t.Fatalf("AddBytes %s: %v", m[0], err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return out.Bytes()
}

// A statistics failure AFTER the platform database landed must not skip the
// reseal: that would leave a restored database whose credentials only the
// source installation can open.
func TestRestoreResealsTheDatabaseWhenTheMetricsDBFails(t *testing.T) {
	raw := writeMetricsBundle(t, "source-secret")
	rig := newRig(t, "this-instances-secret")
	rig.r.RestoreMetricsInto = func(context.Context, io.Reader, string) error {
		return ErrMetricsTargetNotReady
	}
	_, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Database: true, MetricsDB: true}, false)
	if !errors.Is(err, ErrMetricsTargetNotReady) {
		t.Fatalf("err = %v, want the metrics failure", err)
	}
	if rig.dbCalls != 1 || rig.reseal.calls != 1 || rig.reseal.from != "source-secret" {
		t.Errorf("db loads = %d, reseals = %d from %q; want the restored database resealed",
			rig.dbCalls, rig.reseal.calls, rig.reseal.from)
	}
}

// An older bundle has no statistics dump. Selecting it is refused before a
// single byte is written, so the platform database is not restored either.
func TestRestoreRefusesAMetricsDBTheBundleDoesNotHold(t *testing.T) {
	raw := writeTestBundle(t, "source-secret", map[string]string{"database.dump": "PGDMP...", "library/a.jar": "JAR"})
	rig := newRig(t, "source-secret")
	rig.r.RestoreMetricsInto = func(context.Context, io.Reader, string) error {
		t.Error("loaded a dump that does not exist")
		return nil
	}
	_, err := rig.r.Restore(context.Background(), bytes.NewReader(raw), restorePass,
		PlatformRestoreSelection{Database: true, MetricsDB: true, Library: true}, false)
	if !errors.Is(err, ErrBundleHasNoMetricsDB) {
		t.Fatalf("err = %v, want ErrBundleHasNoMetricsDB", err)
	}
	if rig.dbCalls != 0 || len(rig.lib.written) != 0 {
		t.Errorf("wrote before refusing: db loads = %d, library = %v", rig.dbCalls, rig.lib.written)
	}
}
