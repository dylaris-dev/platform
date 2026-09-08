package services

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
	"dylaris-core/services/bundle"
	"dylaris-core/store"
)

// Reading a platform bundle back.
//
// The database is restored into a NEW, empty database the operator names, never
// over the one Core is running on. Same shape as the in-panel database
// migration next door, and for the same reason: the live Core keeps working
// throughout, a failure costs nothing, and the operator switches over by
// restarting with the new DB_* values once they can see it worked. Dropping and
// recreating every table underneath a running Core is the alternative, and it
// turns a recovery into a second outage when it goes wrong.
//
// Components are selected at restore time. A bundle says what it holds; the
// operator says what should come back. "Only the Library" is a real case and
// must not require restoring a database with it.

// PlatformRestoreSelection is what an operator asked to bring back.
type PlatformRestoreSelection struct {
	Database bool `json:"database"`
	Library  bool `json:"library"`
	Modpacks bool `json:"modpacks"`
}

// Any reports whether anything at all was selected.
func (s PlatformRestoreSelection) Any() bool { return s.Database || s.Library || s.Modpacks }

// PlatformRestoreResult is what happened, per part.
type PlatformRestoreResult struct {
	// Source is the release that WROTE the bundle, so an operator looking at a
	// restore knows what produced it.
	Source     string                           `json:"source"`
	CreatedAt  time.Time                        `json:"createdAt"`
	Components []models.PlatformBackupComponent `json:"components"`
	// Reseal is what happened to the at-rest credentials in the restored
	// database. Nil when no database was restored.
	Reseal *store.ResealReport `json:"reseal,omitempty"`
	// Warnings are things that succeeded but that the operator has to know.
	Warnings []string `json:"warnings,omitempty"`
}

// CoreStorageWriter writes one object into a scoped Core storage area.
type CoreStorageWriter interface {
	Write(ctx context.Context, key string, r io.Reader) error
}

// ResealTarget is the slice of a store bound to the RESTORED database.
type ResealTarget interface {
	ResealAtRest(fromSecret, toSecret string) (*store.ResealReport, error)
}

// ErrBundleWrongPassphrase is a passphrase that does not open the bundle.
//
// Re-exported from the crypto package so a caller can name the one failure it
// will see most often without reaching past this package for it.
var ErrBundleWrongPassphrase = crypto.ErrWrongPassphrase

// ErrNothingSelected is a restore that would do nothing.
var ErrNothingSelected = errors.New("choose at least one component to restore")

// ErrTargetNotEmpty is a target database that already holds tables.
var ErrTargetNotEmpty = errors.New("the target database is not empty")

// PlatformRestorer reads a bundle and puts its parts back.
type PlatformRestorer struct {
	// ClusterSecret is THIS instance's secret, the one the restored credentials
	// have to end up sealed under.
	ClusterSecret string
	WorkDir       string

	// RestoreDatabaseInto loads the dump into the operator's target database.
	RestoreDatabaseInto func(ctx context.Context, src io.Reader) error
	// TargetIsEmpty answers whether that database already holds anything.
	TargetIsEmpty func(ctx context.Context) (bool, error)
	// OpenTarget binds a store to the RESTORED database, for the reseal.
	OpenTarget func(ctx context.Context) (ResealTarget, io.Closer, error)
	// OpenCoreStorageWriter scopes the shared Core storage to one area.
	OpenCoreStorageWriter func(area string) (CoreStorageWriter, error)
}

// Inspect reads what a bundle is, without restoring anything.
//
// The plaintext header comes back even when the passphrase is wrong or absent,
// because identifying a file is not the same as opening it: an operator holding
// three bundles needs to know which is which before typing anything.
func Inspect(src io.Reader, passphrase string) (*bundle.Header, *bundle.Manifest, error) {
	if passphrase == "" {
		h, _, err := bundle.ReadHeader(src)
		return h, nil, err
	}
	h, m, _, err := bundle.Open(src, passphrase)
	return h, m, err
}

// Restore reads src and puts the selected components back.
//
// A wrong passphrase is refused by bundle.Open before a single byte of payload
// is decrypted, which is the whole reason the verifier sits in the header.
func (r *PlatformRestorer) Restore(ctx context.Context, src io.Reader, passphrase string,
	sel PlatformRestoreSelection, overwriteTarget bool) (*PlatformRestoreResult, error) {

	if !sel.Any() {
		return nil, ErrNothingSelected
	}
	header, manifest, tr, err := bundle.Open(src, passphrase)
	if err != nil {
		return nil, err
	}

	res := &PlatformRestoreResult{Source: header.Source, CreatedAt: header.CreatedAt}
	if manifest != nil && manifest.Source != "" {
		res.Source = manifest.Source
	}

	if sel.Database {
		if err := r.checkTargetEmpty(ctx, overwriteTarget); err != nil {
			return nil, err
		}
	}

	// Writers are opened lazily and ONCE, so a bundle with ten thousand library
	// files does not rebuild the provider ten thousand times.
	writers := map[string]CoreStorageWriter{}
	areaWriter := func(area string) (CoreStorageWriter, error) {
		if w, ok := writers[area]; ok {
			return w, nil
		}
		if r.OpenCoreStorageWriter == nil {
			return nil, errors.New("no Core file storage is configured")
		}
		w, err := r.OpenCoreStorageWriter(area)
		if err != nil {
			return nil, err
		}
		writers[area] = w
		return w, nil
	}

	counts := map[string]int64{}
	restoredDB := false

	for {
		hdr, terr := tr.Next()
		if errors.Is(terr, io.EOF) {
			break
		}
		if terr != nil {
			// A truncated bundle reaches here rather than as a clean end; the
			// chunked framing refuses to report EOF without its final chunk.
			return nil, fmt.Errorf("reading the bundle: %w", terr)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		// Checked on the RAW name and before anything is dispatched on it. An
		// entry that cleans to a path outside its area - "library/../../etc/x" -
		// matches no case below and would otherwise be dropped in the same
		// silence as a member from a newer Dylaris. Ignoring what we do not
		// recognise is right; ignoring an escape attempt is not, and the two
		// look identical once the name has been cleaned.
		if !safeBundleKey(hdr.Name) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("skipped an unsafe entry %q", hdr.Name))
			continue
		}
		name := path.Clean(hdr.Name)
		switch {
		case name == "database.dump":
			if !sel.Database {
				continue
			}
			if err := r.restoreDatabase(ctx, tr); err != nil {
				res.Components = append(res.Components, models.PlatformBackupComponent{
					Kind: "database", Status: models.PlatformBackupFailed, Message: err.Error(),
				})
				return res, err
			}
			restoredDB = true
			res.Components = append(res.Components, models.PlatformBackupComponent{
				Kind: "database", Status: models.PlatformBackupIncluded, SizeBytes: hdr.Size,
			})

		case strings.HasPrefix(name, "library/"), strings.HasPrefix(name, "modpacks/"):
			area, key, _ := strings.Cut(name, "/")
			if (area == "library" && !sel.Library) || (area == "modpacks" && !sel.Modpacks) {
				continue
			}
			w, err := areaWriter(area)
			if err != nil {
				res.Components = append(res.Components, models.PlatformBackupComponent{
					Kind: area, Status: models.PlatformBackupFailed, Message: err.Error(),
				})
				return res, err
			}
			if err := w.Write(ctx, key, io.LimitReader(tr, hdr.Size)); err != nil {
				res.Components = append(res.Components, models.PlatformBackupComponent{
					Kind: area, Status: models.PlatformBackupFailed, Message: fmt.Sprintf("%s: %v", key, err),
				})
				return res, fmt.Errorf("%s/%s: %w", area, key, err)
			}
			counts[area] += hdr.Size
		}
	}

	for _, area := range []string{"library", "modpacks"} {
		if (area == "library" && !sel.Library) || (area == "modpacks" && !sel.Modpacks) {
			continue
		}
		res.Components = append(res.Components, models.PlatformBackupComponent{
			Kind: area, Status: models.PlatformBackupIncluded, SizeBytes: counts[area],
		})
	}

	if restoredDB {
		if err := r.resealRestored(ctx, manifest, res); err != nil {
			return res, err
		}
		res.Warnings = append(res.Warnings,
			"The database was restored into the target you named. This Core is still running on its own database: restart it with the new DB_* values once you have checked the result.")
	}
	return res, nil
}

func (r *PlatformRestorer) checkTargetEmpty(ctx context.Context, overwrite bool) error {
	if r.TargetIsEmpty == nil || overwrite {
		return nil
	}
	empty, err := r.TargetIsEmpty(ctx)
	if err != nil {
		return fmt.Errorf("checking the target database: %w", err)
	}
	if !empty {
		// Refused rather than merged. A restore over an existing schema leaves
		// rows from two installations in one database, and nothing afterwards
		// says which came from where.
		return ErrTargetNotEmpty
	}
	return nil
}

// restoreDatabase spools the dump before loading it.
//
// Spooled because pg_restore reads its input more than once for a custom-format
// archive - it reads the table of contents, then the data - and a tar member is
// a forward-only stream.
func (r *PlatformRestorer) restoreDatabase(ctx context.Context, src io.Reader) error {
	if r.RestoreDatabaseInto == nil {
		return errors.New("no target database was given")
	}
	if err := os.MkdirAll(r.WorkDir, 0o700); err != nil {
		return fmt.Errorf("work directory: %w", err)
	}
	f, err := os.CreateTemp(r.WorkDir, "restore-*.dump")
	if err != nil {
		return err
	}
	path := f.Name()
	defer func() {
		f.Close()
		os.Remove(path)
	}()

	if _, err := io.Copy(f, src); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return r.RestoreDatabaseInto(ctx, f)
}

// resealRestored moves every at-rest value in the RESTORED database from the
// secret that wrote the bundle to this instance's own.
//
// Without this the restore looks like it worked and nothing does: every node
// fails authentication, every storage provider build fails, and the Modrinth
// token and mail credential read as empty - because the ciphertext in the dump
// only opens under the source installation's CLUSTER_SECRET.
func (r *PlatformRestorer) resealRestored(ctx context.Context, m *bundle.Manifest, res *PlatformRestoreResult) error {
	if m == nil || m.ClusterSecret == "" {
		res.Warnings = append(res.Warnings,
			"This bundle carries no cluster secret, so the stored credentials could not be re-encrypted for this installation. Nodes will need to re-pair and storage credentials will need to be entered again.")
		return nil
	}
	if r.ClusterSecret == "" {
		return errors.New("this Core has no cluster secret, so the restored credentials cannot be re-encrypted")
	}
	if m.ClusterSecret == r.ClusterSecret {
		// The same installation, or one deliberately given the same secret.
		// Nothing to move, and ResealAtRest refuses a no-op rotation anyway.
		return nil
	}
	if r.OpenTarget == nil {
		return errors.New("the restored database cannot be reached to re-encrypt its credentials")
	}
	target, closer, err := r.OpenTarget(ctx)
	if err != nil {
		return fmt.Errorf("opening the restored database: %w", err)
	}
	if closer != nil {
		defer closer.Close()
	}
	rep, err := target.ResealAtRest(m.ClusterSecret, r.ClusterSecret)
	if err != nil {
		return fmt.Errorf("re-encrypting the restored credentials: %w", err)
	}
	res.Reseal = rep
	if rep.Unreadable() > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"%d stored credentials could not be re-encrypted and are unreadable on this installation. They were left exactly as they are, so the secret that wrote them can still open them.",
			rep.Unreadable()))
	}
	return nil
}

// safeBundleKey rejects a member name that would write outside its area.
//
// A bundle is a file somebody handed us, and it may have been assembled by hand.
// path.Clean collapses "a/../b" but leaves a leading "../" in place, which is
// exactly the case that matters.
func safeBundleKey(key string) bool {
	if key == "" {
		return false
	}
	clean := path.Clean(key)
	if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
