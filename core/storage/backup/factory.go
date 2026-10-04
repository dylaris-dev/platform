package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
)

// Deps carries the runtime dependencies the `node-local` provider needs to
// talk to a Node over the existing gRPC mesh. The s3 and shared providers
// ignore it, so existing call-sites can pass a zero-value Deps when they
// don't have these handles. Pass a populated Deps any time you call into a
// node-local storage (download, delete, list) so the operation can be
// dispatched to the right Node.
type Deps struct {
	Registry  *nodegrpc.Registry // Active gRPC connections to Nodes
	NodeStore NodeStore          // Looks up a Node's ID by server UUID

	// CoreStorage builds an ALREADY-ADAPTED backup.Storage backed by the
	// shared Core file storage, scoped to the given sub-prefix. It returns a
	// Storage rather than a storage.StorageProvider on purpose: package
	// `storage` already imports this package (storage/s3provider.go uses
	// backup.Object / backup.NewS3), so importing `storage` from here would
	// be a cycle. handlers/backup.go supplies the closure, which wraps
	// buildCoreStorageProvider in storage.NewCoreStorageBackupAdapter.
	//
	// Nil for callers that never touch the "core-storage" provider.
	CoreStorage func(subPrefix string) (Storage, error)

	// Connection builds a Storage from a saved storage connection (the named,
	// reusable S3 credential set the panel manages), scoped to prefix.
	//
	// Same closure shape and the same reason as CoreStorage: resolving the
	// connection needs the store, and this package must not import it. It also
	// resolves PER CALL on purpose - that is the whole point of pointing
	// backups at a connection instead of copying its keys into this row: a
	// rotated credential takes effect everywhere at once, with nothing to
	// re-enter and nothing left stale behind.
	//
	// Nil for callers that never touch the "connection" provider.
	Connection func(connectionID int, prefix string) (Storage, error)
}

// ConnectionConfig is the backup_storages.config blob for the "connection"
// provider. It deliberately holds NO credentials: only a reference and the
// prefix under which this target writes.
type ConnectionConfig struct {
	ConnectionID int    `json:"connectionId"`
	Prefix       string `json:"prefix"`
}

// CoreStorageSubPrefix is the namespace server backups occupy inside the ONE
// configured Core file storage. It duplicates the canonical
// handlers.CoreStoragePrefixServerBackups because this package must not
// import handlers (handlers already imports this package). The two are held
// together by handlers.TestCoreStorageSubPrefixesMatch.
const CoreStorageSubPrefix = "server-backups"

// NodeStore is the narrow store surface the node-local provider depends on.
// Implemented by the existing store.Store interface — kept abstract here so
// the backup package doesn't take a hard dependency on the full store API.
type NodeStore interface {
	GetServerByUUID(uuid string) (*models.Server, error)
}

// TenantEndpoint reports whether a storage dials an endpoint a TENANT typed:
// their own s3 bucket. Core then dials it through netguard (NewTenantS3) and
// tells the node to do the same for its presigned transfers.
func TenantEndpoint(bs *models.BackupStorage) bool {
	return bs != nil && bs.OwnerID != nil && bs.Provider == "s3"
}

// Open returns a Storage implementation matching the given backup_storages
// row. The factory keeps the rest of the codebase decoupled from concrete
// providers.
//
// `deps` is used by the `node-local` provider to reach the right Node over
// the existing gRPC mesh. Pass a zero-value Deps for s3/shared (the
// providers ignore it) when the caller has no registry handy — e.g. a
// scheduler probe that only ever touches s3.
func Open(ctx context.Context, bs *models.BackupStorage, deps Deps) (Storage, error) {
	if bs == nil {
		return nil, fmt.Errorf("nil backup storage")
	}
	switch bs.Provider {
	case "local", "shared":
		// "shared" is the new UI label for what used to be "local" — the
		// provider DB column stays "local" for back-compat; either string
		// resolves to the same implementation.
		return NewLocal(bs.Config)
	case "s3":
		if TenantEndpoint(bs) {
			return NewTenantS3(ctx, bs.Config)
		}
		return NewS3(ctx, bs.Config)
	case "node-local":
		if deps.Registry == nil || deps.NodeStore == nil {
			return nil, fmt.Errorf("node-local storage requires gRPC registry and node-store handles")
		}
		return NewNodeLocal(deps.Registry, deps.NodeStore), nil
	case "core-storage":
		// Backups land on whatever the shared Core file storage is
		// configured as (path or s3), under CoreStorageSubPrefix. This is an
		// adapter, not a new backend, and it cannot presign an upload - a
		// BYON tenant node therefore uploads through Core rather than
		// directly. See storage.CoreStorageBackupAdapter.
		if deps.CoreStorage == nil {
			return nil, fmt.Errorf("core-storage backup provider requires a CoreStorage builder")
		}
		st, err := deps.CoreStorage(CoreStorageSubPrefix)
		if err != nil {
			return nil, fmt.Errorf("core-storage backup provider: %w", err)
		}
		return st, nil
	case "connection":
		if deps.Connection == nil {
			return nil, fmt.Errorf("connection backup provider requires a Connection builder")
		}
		var cfg ConnectionConfig
		if len(bs.Config) > 0 {
			if err := json.Unmarshal(bs.Config, &cfg); err != nil {
				return nil, fmt.Errorf("invalid connection config: %w", err)
			}
		}
		if cfg.ConnectionID <= 0 {
			return nil, fmt.Errorf("connection backup provider requires a connectionId")
		}
		prefix := cfg.Prefix
		if strings.TrimSpace(prefix) == "" {
			// Default to the same namespace core-storage backups use. Without
			// it a connection shared with Core file storage would write archives
			// beside library/ and modpacks/ in the bucket root - legal, but it
			// makes lifecycle rules and quota accounting per subsystem
			// impossible after the fact.
			prefix = CoreStorageSubPrefix
		}
		st, err := deps.Connection(cfg.ConnectionID, prefix)
		if err != nil {
			return nil, fmt.Errorf("connection backup provider: %w", err)
		}
		return st, nil
	default:
		return nil, fmt.Errorf("unknown backup provider: %s", bs.Provider)
	}
}
