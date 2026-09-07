package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"dylaris-core/store"
)

// sharedObjectStore answers only the reference count. The embedded nil
// store.Store panics on anything else.
type sharedObjectStore struct {
	store.Store

	counts map[string]int
	err    error
	asked  []string
}

func (f *sharedObjectStore) CountModversionsByStorageKey(key string) (int, error) {
	f.asked = append(f.asked, key)
	if f.err != nil {
		return 0, f.err
	}
	return f.counts[key], nil
}

// recordingProvider records deletes. Every other method errors: nothing else
// may be reached on this path, and an error says so louder than a zero value.
type recordingProvider struct {
	deleted []string
}

func (p *recordingProvider) Delete(_ context.Context, key string) error {
	p.deleted = append(p.deleted, key)
	return nil
}

var errUnexpectedProviderCall = fmt.Errorf("provider method must not be reached on the delete path")

func (p *recordingProvider) Put(context.Context, string, []byte) error {
	return errUnexpectedProviderCall
}
func (p *recordingProvider) Get(context.Context, string) ([]byte, error) {
	return nil, errUnexpectedProviderCall
}
func (p *recordingProvider) Stat(context.Context, string) (int64, bool, error) {
	return 0, false, errUnexpectedProviderCall
}
func (p *recordingProvider) PutStream(context.Context, string, io.Reader, int64) error {
	return errUnexpectedProviderCall
}
func (p *recordingProvider) Stream(context.Context, string) (io.ReadCloser, int64, error) {
	return nil, 0, errUnexpectedProviderCall
}
func (p *recordingProvider) DownloadURL(context.Context, string, time.Duration) (string, error) {
	return "", errUnexpectedProviderCall
}

func (p *recordingProvider) UploadURL(context.Context, string, time.Duration) (string, error) {
	return "", nil
}

// A content object is not owned by one modversion row. MigrateBuild's
// copyUploadedContent creates a NEW row for the SAME storage key on purpose, so
// that updating one build cannot rewrite the other's row. The delete used to be
// unconditional on the reasoning that "the DB no longer references it" - true of
// the row just rewritten, not of the object.
func TestASharedContentObjectIsNotDeleted(t *testing.T) {
	const key = "packs/owner-1/mods/sodium/sodium-u-0badc0de.zip"

	tests := []struct {
		name        string
		count       int
		countErr    error
		wantDeleted bool
		why         string
	}{
		{
			name:        "another build still points at it",
			count:       1,
			wantDeleted: false,
			why:         "deleting it makes the other build's render fail on a key that is simply gone",
		},
		{
			name:        "nothing references it any more",
			count:       0,
			wantDeleted: true,
			why:         "an unreferenced object is dead storage; the delete has to still happen",
		},
		{
			name:        "the count could not be read",
			countErr:    errors.New("connection reset by peer"),
			wantDeleted: false,
			why:         "an orphan costs storage, a wrong delete costs someone's build",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := &sharedObjectStore{counts: map[string]int{key: tt.count}, err: tt.countErr}
			prov := &recordingProvider{}
			h := &PacksHandler{state: &AppState{Store: fs}}

			h.deleteIfUnreferenced(context.Background(), prov, key)

			deleted := len(prov.deleted) == 1 && prov.deleted[0] == key
			if deleted != tt.wantDeleted {
				t.Errorf("deleted=%v, want %v: %s", deleted, tt.wantDeleted, tt.why)
			}
		})
	}
}

// An empty key must not even be asked about, let alone deleted.
func TestAnEmptyStorageKeyIsNeverDeleted(t *testing.T) {
	fs := &sharedObjectStore{counts: map[string]int{}}
	prov := &recordingProvider{}
	h := &PacksHandler{state: &AppState{Store: fs}}

	h.deleteIfUnreferenced(context.Background(), prov, "")

	if len(prov.deleted) != 0 {
		t.Errorf("deleted %v for an empty key", prov.deleted)
	}
	if len(fs.asked) != 0 {
		t.Errorf("asked the store about an empty key: %v", fs.asked)
	}
}

// Editing text used to Put the new bytes straight back onto mv.StorageKey. The
// object can carry a second modversion row (copyUploadedContent creates one on
// purpose so an update of one build cannot rewrite the other's row), so writing
// in place rewrote the source build's file anyway - the same damage the copy was
// built to prevent, reached through the storage layer instead of the DB.
func TestAnEditedContentKeyIsFreshAndStaysInItsDirectory(t *testing.T) {
	const oldKey = "packs/11111111-1111-4111-8111-111111111111/mods/sodium/sodium-u-0badc0de.zip"
	const dir = "packs/11111111-1111-4111-8111-111111111111/mods/sodium"

	first := editedContentKey(oldKey, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if first == oldKey {
		t.Fatal("the edit overwrote the shared object in place")
	}
	if got := pathDir(first); got != dir {
		t.Errorf("edited key left its directory: %s (dir %s, want %s)", first, got, dir)
	}

	// Same text, same key: re-saving must not grow a chain of objects.
	if again := editedContentKey(oldKey, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); again != first {
		t.Errorf("re-saving identical text produced a second key: %s then %s", first, again)
	}
	// Different text, different key: two edits must not collide on one object.
	if other := editedContentKey(oldKey, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"); other == first {
		t.Errorf("two different edits share one key: %s", other)
	}
	// A second edit chains off the FIRST edit's key and must still stay put.
	if second := editedContentKey(first, "cccccccccccccccccccccccccccccccc"); pathDir(second) != dir {
		t.Errorf("a second edit left the directory: %s", second)
	}
}

func pathDir(key string) string {
	i := len(key) - 1
	for ; i >= 0; i-- {
		if key[i] == '/' {
			return key[:i]
		}
	}
	return ""
}

// The key derivation above is only half of it: the handler has to actually
// STORE at the fresh key. A Put back onto mv.StorageKey would pass every test
// above while still rewriting the shared object.
func TestSetContentTextStoresAtTheFreshKey(t *testing.T) {
	raw, err := os.ReadFile("packs_text.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	src := string(raw)

	if strings.Contains(src, "prov.Put(r.Context(), mv.StorageKey") {
		t.Error("SetContentText writes back onto the shared storage key again")
	}
	if !strings.Contains(src, "prov.Put(r.Context(), newKey") {
		t.Error("SetContentText no longer stores the edited zip at a fresh key")
	}
	if !strings.Contains(src, "h.deleteIfUnreferenced(r.Context(), prov, oldKey)") {
		t.Error("the old object is no longer released through the reference-counted delete")
	}
}
