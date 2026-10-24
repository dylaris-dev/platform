package handlers

import (
	"fmt"

	"dylaris-core/storage"
	backupstorage "dylaris-core/storage/backup"
)

// CoreStorageBackupBuilder returns the closure that opens the shared Core file
// storage as a backup backend. Exported because the backup scheduler lives in
// package services and is wired from main.go, which cannot reach the
// unexported provider builder - and a scheduler without this is refused by
// backupstorage.Open for every "core-storage" job, which is how the reaper's
// storage probe came to be inert for that provider.
//
// The returned closure resolves PER CALL, never caching: the shared Core file
// storage config can change under a running Core, and every other provider in
// this codebase resolves per request too.
func (s *AppState) CoreStorageBackupBuilder() func(subPrefix string) (backupstorage.Storage, error) {
	return func(subPrefix string) (backupstorage.Storage, error) {
		prov, err := s.buildCoreStorageProvider(subPrefix)
		if err != nil {
			return nil, err
		}
		return storage.NewCoreStorageBackupAdapter(prov), nil
	}
}

// ConnectionBackupBuilder returns the closure that opens a saved storage
// connection as a backup backend, scoped to prefix. Exported for the same
// reason as CoreStorageBackupBuilder: the scheduler lives in package services
// and is wired from main.go, and without this every "connection" job would be
// refused by backupstorage.Open.
//
// Resolves PER CALL. That is the entire benefit of referencing a connection
// rather than copying its keys into the backup row: rotating the credential in
// one place takes effect everywhere, with no stale copy left behind.
func (s *AppState) ConnectionBackupBuilder() func(connectionID int, prefix string) (backupstorage.Storage, error) {
	return func(connectionID int, prefix string) (backupstorage.Storage, error) {
		if s.Store == nil {
			return nil, fmt.Errorf("storage connection %d: no store", connectionID)
		}
		conn, err := s.Store.GetStorageConnection(connectionID)
		if err != nil {
			return nil, fmt.Errorf("storage connection %d could not be loaded: %w", connectionID, err)
		}
		cfg := coreStorageConfigFromConnection(conn)
		if err := validateCoreStorageConfig(cfg); err != nil {
			return nil, fmt.Errorf("storage connection %q is not usable: %w", conn.Name, err)
		}
		// Same provider construction Core file storage uses, so a connection
		// behaves identically whichever subsystem points at it - including the
		// connection's own prefix, which nests ABOVE the backup prefix.
		prov, err := newStorageProviderForConfig(cfg, prefix, s.StorageGate, s.StorageS3)
		if err != nil {
			return nil, fmt.Errorf("storage connection %q: %w", conn.Name, err)
		}
		return storage.NewCoreStorageBackupAdapter(prov), nil
	}
}
