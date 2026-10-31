package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"dylaris-core/storage"
)

// libraryPurgedKey records that the removed Library's files are gone, so the
// purge runs once per installation rather than walking the storage every boot.
const libraryPurgedKey = "library_purged_at"

// PurgeLibraryStorage deletes everything the removed Library left under the
// Core storage "library" prefix, once.
//
// It never fails the boot. Any error is logged and the marker stays unset, so
// the next start tries again. Without a Core storage config there is nothing to
// delete yet, and the marker is left unset for the same reason: a config saved
// later may point at a storage that still holds the files.
func (s *AppState) PurgeLibraryStorage(ctx context.Context) {
	if s.Store == nil {
		return
	}
	done, err := s.Store.GetSetting(libraryPurgedKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("library purge: reading %s failed, retrying on the next start: %v", libraryPurgedKey, err)
		return
	}
	if done != "" || !s.CoreStorageConfigured() {
		return
	}
	n, err := purgeLibraryPrefix(ctx, s.buildCoreStorageProvider)
	if err != nil {
		log.Printf("library purge: failed, retrying on the next start: %v", err)
		return
	}
	if err := s.Store.SetSetting(libraryPurgedKey, time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Printf("library purge: deleted %d file(s) but could not record it, it runs again on the next start: %v", n, err)
		return
	}
	log.Printf("library purge: deleted %d file(s) of the removed Library from Core storage", n)
}

// purgeLibraryPrefix deletes the library/ namespace through a provider rooted at
// the Core storage root, and returns how many files it held. Rooted there
// because a provider scoped to library/ cannot delete its own root
// (storage.ErrDeleteRoot), and deleting the namespace as one path also removes
// the directory itself on a host path.
func purgeLibraryPrefix(ctx context.Context, build func(subPrefix string) (storage.StorageProvider, error)) (int, error) {
	root, err := build("")
	if err != nil {
		return 0, fmt.Errorf("build Core storage provider: %w", err)
	}
	files, err := storage.WalkProvider(ctx, root, CoreStoragePrefixLibrary)
	if err != nil {
		return 0, fmt.Errorf("list %s/: %w", CoreStoragePrefixLibrary, err)
	}
	if err := root.DeletePath(ctx, CoreStoragePrefixLibrary); err != nil {
		return 0, fmt.Errorf("delete %s/: %w", CoreStoragePrefixLibrary, err)
	}
	return len(files), nil
}
