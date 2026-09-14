package storagemigrate

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"dylaris-core/storage"
	"dylaris-core/storage/backup"
	"dylaris-core/storage/modpack"
)

func TestServerBackupsDataSetID_RoundTrip(t *testing.T) {
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"server-backups:3", 3, true},
		{"server-backups:0", 0, true},
		{"server-backups:127", 127, true},
		{"server-backups:", 0, false},
		{"server-backups:abc", 0, false},
		{"server-backups:-1", 0, false},
		{"library", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		t.Run("id="+c.in, func(t *testing.T) {
			got, ok := ParseServerBackupsDataSetID(c.in)
			if ok != c.wantOK || got != c.want {
				t.Fatalf("ParseServerBackupsDataSetID(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.wantOK)
			}
		})
	}
	if got := ServerBackupsDataSetID(3); got != "server-backups:3" {
		t.Errorf("ServerBackupsDataSetID(3) = %q, want server-backups:3", got)
	}
	back, ok := ParseServerBackupsDataSetID(ServerBackupsDataSetID(42))
	if !ok || back != 42 {
		t.Errorf("round trip broke: got (%d, %v)", back, ok)
	}
}

func TestProviderDataSet_ListIsRecursiveOpenWriteDelete(t *testing.T) {
	ctx := context.Background()
	prov := &storage.LocalProvider{BasePath: t.TempDir()}
	ds := NewProviderDataSet(DataSetLibrary, "Library", prov)

	if ds.ID() != DataSetLibrary || ds.Label() != "Library" {
		t.Fatalf("ID/Label = %q/%q", ds.ID(), ds.Label())
	}
	if err := ds.Write(ctx, "a/b/deep.jar", strings.NewReader("xyz"), 3); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := ds.Write(ctx, "top.jar", strings.NewReader("q"), 1); err != nil {
		t.Fatalf("Write: %v", err)
	}

	refs, err := ds.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// StorageProvider.ListFiles is one level only, so a non-recursive
	// adapter returns just top.jar and fails here.
	if len(refs) != 2 || refs[0].Key != "a/b/deep.jar" || refs[1].Key != "top.jar" {
		t.Fatalf("List = %+v, want [a/b/deep.jar top.jar] ascending", refs)
	}
	if refs[0].Size != 3 {
		t.Errorf("deep.jar size = %d, want 3", refs[0].Size)
	}

	rc, err := ds.Open(ctx, "a/b/deep.jar")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "xyz" {
		t.Errorf("Open = %q, want xyz", got)
	}

	if err := ds.Delete(ctx, "a/b/deep.jar"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := ds.Delete(ctx, "a/b/deep.jar"); err != nil {
		t.Fatalf("second Delete (idempotent): %v", err)
	}
}

func TestProviderDataSet_OpenMissingIsErrNotExist(t *testing.T) {
	ds := NewProviderDataSet(DataSetLibrary, "Library", &storage.LocalProvider{BasePath: t.TempDir()})
	_, err := ds.Open(context.Background(), "nope.jar")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open(missing) err = %v, want errors.Is(err, fs.ErrNotExist)", err)
	}
}

// fakeBackupStorage is a backup.Storage double whose Get can be made to fail
// with a non-not-found error. Local to this file; never redeclared.
type fakeBackupStorage struct {
	objects map[string][]byte
	getErr  map[string]error
	// notFoundErr is what Get returns for an absent key. Defaults to a raw,
	// NON-fs.ErrNotExist error, mirroring backup.S3Storage.Get which does not
	// normalize NoSuchKey (only Delete does).
	notFoundErr error
	deleted     []string
	// provider is what Provider() reports. Defaults to "s3"; tests override it
	// to exercise the node-local rejection in NewBackupDataSet.
	provider string
}

func newFakeBackupStorage() *fakeBackupStorage {
	return &fakeBackupStorage{
		objects:     map[string][]byte{},
		getErr:      map[string]error{},
		notFoundErr: errRawNoSuchKey,
		provider:    "s3",
	}
}

func (f *fakeBackupStorage) Provider() string { return f.provider }
func (f *fakeBackupStorage) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.objects[key] = b
	return nil
}
func (f *fakeBackupStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	if err := f.getErr[key]; err != nil {
		return nil, err
	}
	b, ok := f.objects[key]
	if !ok {
		return nil, f.notFoundErr
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}
func (f *fakeBackupStorage) Delete(_ context.Context, key string) error {
	f.deleted = append(f.deleted, key)
	delete(f.objects, key)
	return nil
}
func (f *fakeBackupStorage) List(_ context.Context, prefix string) ([]backup.Object, error) {
	out := []backup.Object{}
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, backup.Object{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}
func (f *fakeBackupStorage) Stat(_ context.Context, key string) (backup.Object, error) {
	b, ok := f.objects[key]
	if !ok {
		return backup.Object{}, f.notFoundErr
	}
	return backup.Object{Key: key, Size: int64(len(b))}, nil
}
func (f *fakeBackupStorage) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}
func (f *fakeBackupStorage) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", backup.ErrUploadURLUnsupported
}

func (*fakeBackupStorage) CreateMultipart(context.Context, string) (string, error) {
	return "", backup.ErrMultipartUnsupported
}

func (*fakeBackupStorage) UploadPartURL(context.Context, string, string, int32, time.Duration) (string, error) {
	return "", backup.ErrMultipartUnsupported
}

func (*fakeBackupStorage) CompleteMultipart(context.Context, string, string, int64) (int64, error) {
	return 0, backup.ErrMultipartUnsupported
}

func (*fakeBackupStorage) AbortMultipart(context.Context, string, string) error {
	return backup.ErrMultipartUnsupported
}

func (*fakeBackupStorage) ListMultipart(context.Context, string, string) (backup.MultipartUsage, error) {
	return backup.MultipartUsage{}, backup.ErrMultipartUnsupported
}

// errRawNoSuchKey stands in for the un-normalized S3 "NoSuchKey" that
// backup.S3Storage.Get returns verbatim.
var errRawNoSuchKey = errors.New("operation error S3: GetObject, api error NoSuchKey: The specified key does not exist.")

func TestBackupDataSet_ListIsSortedAndOpenNormalizesNotFound(t *testing.T) {
	ctx := context.Background()
	st := newFakeBackupStorage()
	ds, err := NewBackupDataSet(ServerBackupsDataSetID(3), "Server backups (Hetzner S3)", st)
	if err != nil {
		t.Fatalf("NewBackupDataSet: %v", err)
	}

	if ds.ID() != "server-backups:3" {
		t.Fatalf("ID = %q, want server-backups:3", ds.ID())
	}
	_ = ds.Write(ctx, "srv-2/b.tar.gz", strings.NewReader("bb"), 2)
	_ = ds.Write(ctx, "srv-1/a.tar.gz", strings.NewReader("a"), 1)

	refs, err := ds.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 || refs[0].Key != "srv-1/a.tar.gz" || refs[1].Key != "srv-2/b.tar.gz" {
		t.Fatalf("List = %+v, want ascending by key", refs)
	}

	// backup.Storage.List is ALREADY recursive on local and s3, so the
	// adapter must not walk it a second time.
	rc, err := ds.Open(ctx, "srv-1/a.tar.gz")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rc.Close()

	// The load-bearing bit: S3Storage.Get returns a RAW NoSuchKey, not an
	// fs.ErrNotExist. The adapter must normalize it, or the copy loop would
	// treat a genuinely missing target object as a hard failure.
	_, err = ds.Open(ctx, "srv-1/gone.tar.gz")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open(missing) err = %v, want errors.Is(err, fs.ErrNotExist)", err)
	}
}

func TestBackupDataSet_OpenDoesNotSwallowRealFailures(t *testing.T) {
	ctx := context.Background()
	st := newFakeBackupStorage()
	throttled := errors.New("operation error S3: GetObject, api error SlowDown: Please reduce your request rate.")
	st.objects["srv-1/a.tar.gz"] = []byte("a")
	st.getErr["srv-1/a.tar.gz"] = throttled

	ds, err := NewBackupDataSet(ServerBackupsDataSetID(3), "Server backups", st)
	if err != nil {
		t.Fatalf("NewBackupDataSet: %v", err)
	}
	_, err = ds.Open(ctx, "srv-1/a.tar.gz")
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a throttle was normalized to fs.ErrNotExist (%v); that would let the copy loop overwrite live data", err)
	}
	if !errors.Is(err, throttled) {
		t.Fatalf("Open err = %v, want it to wrap %v", err, throttled)
	}
}

func TestNewBackupDataSet_RejectsNodeLocal(t *testing.T) {
	st := newFakeBackupStorage()
	st.provider = "node-local"

	ds, err := NewBackupDataSet(ServerBackupsDataSetID(9), "Server backups (node-local)", st)
	if err == nil {
		t.Fatal("NewBackupDataSet(node-local) returned no error; a node-local source that cannot be " +
			"enumerated would silently migrate as an empty data set")
	}
	if ds != nil {
		t.Fatalf("NewBackupDataSet(node-local) returned a non-nil DataSet %v alongside an error", ds)
	}
}

func TestNewBackupDataSet_AcceptsEnumerableProviders(t *testing.T) {
	for _, provider := range []string{"s3", "local", "shared", "core-storage"} {
		t.Run(provider, func(t *testing.T) {
			st := newFakeBackupStorage()
			st.provider = provider
			ds, err := NewBackupDataSet(ServerBackupsDataSetID(1), "Server backups", st)
			if err != nil {
				t.Fatalf("NewBackupDataSet(%s) returned an error: %v", provider, err)
			}
			if ds == nil {
				t.Fatalf("NewBackupDataSet(%s) returned a nil DataSet with no error", provider)
			}
		})
	}
}

func TestNormalizeBackupNotFound(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		wantNotExist bool
	}{
		{"nil stays nil", nil, false},
		{"already fs.ErrNotExist", fs.ErrNotExist, true},
		{"os-style wrapped", errors.Join(errors.New("open x"), fs.ErrNotExist), true},
		{"raw NoSuchKey text", errRawNoSuchKey, true},
		{"NotFound text", errors.New("api error NotFound: Not Found"), true},
		{"throttle is not missing", errors.New("api error SlowDown"), false},
		{"permission is not missing", errors.New("api error AccessDenied"), false},

		// The typed branch is the one real S3 traffic takes: S3Storage.Get
		// returns a raw AWS SDK error that satisfies smithy.APIError, so the
		// string fallback below it is unreachable in production. These cases
		// cover it directly rather than only through non-smithy doubles.
		{"typed NoSuchKey", &smithy.GenericAPIError{Code: "NoSuchKey", Message: "The specified key does not exist."}, true},
		{"typed NotFound", &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"}, true},
		{"typed throttle is not missing", &smithy.GenericAPIError{Code: "SlowDown", Message: "Please reduce your request rate."}, false},
		{"typed missing-bucket is not missing", &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "The specified bucket does not exist."}, false},

		// A typed error short-circuits before the substring fallback, so a
		// non-matching code wins even when its message contains "NotFound".
		// Without that early return a throttle could be read as "missing",
		// which is the direction that makes the copy loop overwrite live data.
		{"typed non-matching code beats NotFound in the message", &smithy.GenericAPIError{Code: "SlowDown", Message: "NotFound appears here"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeBackupNotFound("k", c.err)
			if c.err == nil {
				if got != nil {
					t.Fatalf("normalizeBackupNotFound(nil) = %v, want nil", got)
				}
				return
			}
			if errors.Is(got, fs.ErrNotExist) != c.wantNotExist {
				t.Fatalf("normalizeBackupNotFound(%v) -> %v, wantNotExist %v", c.err, got, c.wantNotExist)
			}
		})
	}
}

// fakeModpackKeys is a ModpackKeySource double. Local to this file.
type fakeModpackKeys struct {
	keys    []string
	sha512s map[string]string
	keysErr error
}

func (f *fakeModpackKeys) ListModpackStorageKeys() ([]string, error) {
	return f.keys, f.keysErr
}
func (f *fakeModpackKeys) ListModversionSHA512ByStorageKey() (map[string]string, error) {
	return f.sha512s, nil
}

func TestModpackDataSet_IDIsTheCallersNotAConstant(t *testing.T) {
	// ID() becomes StorageManifest.DataSet, and it is what a per-data-set
	// manifest lookup filters on. Two modpack-shaped data sets exist (the
	// settings-configured backend and the modpacks namespace inside the shared
	// Core file storage); returning a constant here collapsed them onto one
	// manifest key, so each row showed the other's captures.
	prov := modpack.NewCoreStorageProvider(&storage.LocalProvider{BasePath: t.TempDir()})
	for _, id := range []string{DataSetModpacks, "modpacks@core-storage"} {
		if got := NewModpackDataSet(id, "Modpacks", prov, &fakeModpackKeys{}).ID(); got != id {
			t.Errorf("ID() = %q, want %q", got, id)
		}
	}
}

func TestModpackDataSet_ListComesFromTheDatabase(t *testing.T) {
	ctx := context.Background()
	prov := modpack.NewCoreStorageProvider(&storage.LocalProvider{BasePath: t.TempDir()})
	if err := prov.Put(ctx, "a/pack.mrpack", []byte("aaa")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := prov.Put(ctx, "orphan.mrpack", []byte("zz")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	ds := NewModpackDataSet(DataSetModpacks, "Modpacks", prov, &fakeModpackKeys{
		keys:    []string{"a/pack.mrpack", "", "missing.mrpack"},
		sha512s: map[string]string{},
	})

	refs, err := ds.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Blank keys are skipped; a referenced-but-absent key is skipped too
	// (Stat reports it does not exist); an ORPHAN in storage that no DB row
	// references is invisible - a genuine, documented limitation.
	if len(refs) != 1 || refs[0].Key != "a/pack.mrpack" || refs[0].Size != 3 {
		t.Fatalf("List = %+v, want exactly [{a/pack.mrpack 3}]", refs)
	}
	for _, r := range refs {
		if r.Key == "orphan.mrpack" {
			t.Fatal("an orphan appeared in the modpack key space; it cannot, by construction")
		}
	}
}

func TestModpackDataSet_ListNormalizesLeadingSlash(t *testing.T) {
	ctx := context.Background()
	prov := modpack.NewCoreStorageProvider(&storage.LocalProvider{BasePath: t.TempDir()})
	if err := prov.Put(ctx, "a/pack.mrpack", []byte("aaa")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A DB row with a leading slash must resolve to the SAME object as the
	// slash-free form, not Stat as absent and get silently dropped.
	ds := NewModpackDataSet(DataSetModpacks, "Modpacks", prov, &fakeModpackKeys{
		keys:    []string{"/a/pack.mrpack"},
		sha512s: map[string]string{},
	})

	refs, err := ds.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 || refs[0].Key != "a/pack.mrpack" || refs[0].Size != 3 {
		t.Fatalf("List = %+v, want exactly [{a/pack.mrpack 3}] (leading slash normalized)", refs)
	}
}

func TestModpackDataSet_OpenWriteDeleteRoundTrip(t *testing.T) {
	ctx := context.Background()
	prov := modpack.NewCoreStorageProvider(&storage.LocalProvider{BasePath: t.TempDir()})
	ds := NewModpackDataSet(DataSetModpacks, "Modpacks", prov, &fakeModpackKeys{})

	if err := ds.Write(ctx, "x/pack.mrpack", strings.NewReader("payload"), 7); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rc, err := ds.Open(ctx, "x/pack.mrpack")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "payload" {
		t.Errorf("Open = %q, want payload", got)
	}
	if err := ds.Delete(ctx, "x/pack.mrpack"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := ds.Delete(ctx, "x/pack.mrpack"); err != nil {
		t.Fatalf("second Delete (idempotent): %v", err)
	}
}

func TestModpackDataSet_OpenMissingIsErrNotExist(t *testing.T) {
	// modpack.ErrNotFound is the package's own sentinel; the DataSet contract
	// demands fs.ErrNotExist comparability.
	prov := modpack.NewCoreStorageProvider(&storage.LocalProvider{BasePath: t.TempDir()})
	ds := NewModpackDataSet(DataSetModpacks, "Modpacks", prov, &fakeModpackKeys{})
	_, err := ds.Open(context.Background(), "gone.mrpack")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open(missing) err = %v, want errors.Is(err, fs.ErrNotExist)", err)
	}
}

func TestModpackDataSet_ExposesChecksumHints(t *testing.T) {
	prov := modpack.NewCoreStorageProvider(&storage.LocalProvider{BasePath: t.TempDir()})
	ds := NewModpackDataSet(DataSetModpacks, "Modpacks", prov, &fakeModpackKeys{
		keys:    []string{"a.jar"},
		sha512s: map[string]string{"a.jar": "deadbeef"},
	})
	hinter, ok := ds.(ChecksumHinter)
	if !ok {
		t.Fatal("the modpack data set must implement ChecksumHinter")
	}
	algo, sum, ok := hinter.ChecksumHint("a.jar")
	if !ok || algo != "sha512" || sum != "deadbeef" {
		t.Fatalf("ChecksumHint = (%q, %q, %v), want (sha512, deadbeef, true)", algo, sum, ok)
	}
	if _, _, ok := hinter.ChecksumHint("b.jar"); ok {
		t.Error("ChecksumHint reported a hint for a key with none")
	}
}

func TestProviderDataSet_IsNotAChecksumHinter(t *testing.T) {
	// Only modpacks carry third-party hashes. Nothing else should pretend to.
	ds := NewProviderDataSet(DataSetLibrary, "Library", &storage.LocalProvider{BasePath: t.TempDir()})
	if _, ok := ds.(ChecksumHinter); ok {
		t.Fatal("the provider data set must NOT implement ChecksumHinter")
	}
}
