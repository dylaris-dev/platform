package storageapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/corestorage"
)

// connBackedStore serves settings plus storage connections by id, so a test can
// put the inline settings and the selected connection in different places.
type connBackedStore struct {
	storageMigrationFakeStore
	conns map[int]*models.StorageConnection
}

func (f *connBackedStore) GetStorageConnection(id int) (*models.StorageConnection, error) {
	c, ok := f.conns[id]
	if !ok {
		return nil, errTestNoConn
	}
	return c, nil
}

func newConnBackedStore(conns map[int]*models.StorageConnection) *connBackedStore {
	return &connBackedStore{storageMigrationFakeStore: *newStorageMigrationFakeStore(), conns: conns}
}

func testS3Connection(id int, bucket, prefix string) *models.StorageConnection {
	cfg, _ := json.Marshal(corestorage.ConnectionConfig{Endpoint: "https://s3.example.com", Bucket: bucket, Prefix: prefix})
	return &models.StorageConnection{ID: id, Provider: "s3", Config: cfg, AccessKey: "AK", SecretAccessKey: "SK"}
}

// The same-location refusal must judge where the data LIVES, which for a
// connection-backed store is the connection, not the stale inline keys beside
// it. Comparing the inline keys let a migration copy a store onto itself, pass
// verification against itself and then delete the only copy.
func TestResolveTarget_RefusesTheEffectiveSourceLocation(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(fs *connBackedStore)
		dataSet  string
		target   services.StorageTargetConfig
		wantErr  error
		wantNote string
	}{
		{
			name: "core storage on connection 3, target is connection 3",
			setup: func(fs *connBackedStore) {
				seedInlineS3(fs, "core_storage_", "stale-inline-bucket")
				fs.kv[corestorage.KeyConnectionID] = "3"
			},
			dataSet: CoreStorageDataSetID,
			target:  services.StorageTargetConfig{ConnectionID: 3},
			wantErr: corestorage.ErrTargetSameLocation,
		},
		{
			name: "core storage on connection 3, inline target naming the same bucket and prefix",
			setup: func(fs *connBackedStore) {
				fs.kv[corestorage.KeyConnectionID] = "3"
			},
			dataSet: CoreStorageDataSetID,
			target:  services.StorageTargetConfig{Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "live", S3Prefix: "p", S3AccessKey: "k", S3SecretKey: "s"},
			wantErr: corestorage.ErrTargetSameLocation,
		},
		{
			name: "modpacks on connection 3 (inline settings say local), target is connection 3",
			setup: func(fs *connBackedStore) {
				fs.kv["modpack_storage_provider"] = "local"
				fs.kv["modpack_storage_paths"] = `["/srv/old-modpacks"]`
				fs.kv[corestorage.KeyModpackConnectionID] = "3"
			},
			dataSet: "modpacks",
			target:  services.StorageTargetConfig{ConnectionID: 3},
			wantErr: corestorage.ErrTargetSameLocation,
		},
		{
			name: "modpacks on inline s3 bucket root, target connection on the same bucket root",
			setup: func(fs *connBackedStore) {
				fs.kv["modpack_storage_provider"] = "s3"
				fs.kv["modpack_storage_s3_endpoint"] = "https://s3.example.com"
				fs.kv["modpack_storage_s3_bucket"] = "packs"
			},
			dataSet: "modpacks",
			target:  services.StorageTargetConfig{ConnectionID: 4},
			wantErr: corestorage.ErrTargetSameLocation,
		},
		{
			// The control: a different connection is a real move.
			name: "core storage on connection 3, target is connection 4",
			setup: func(fs *connBackedStore) {
				fs.kv[corestorage.KeyConnectionID] = "3"
			},
			dataSet: CoreStorageDataSetID,
			target:  services.StorageTargetConfig{ConnectionID: 4},
		},
		{
			// modpack's inline s3 backend reads at the bucket root and has no
			// path-style option, so a target it could not read back is refused.
			name: "modpacks to an inline s3 target with a prefix",
			setup: func(fs *connBackedStore) {
				fs.kv["modpack_storage_provider"] = "local"
				fs.kv["modpack_storage_paths"] = `["/srv/old-modpacks"]`
			},
			dataSet:  "modpacks",
			target:   services.StorageTargetConfig{Backend: "s3", S3Bucket: "new", S3Prefix: "p", S3AccessKey: "k", S3SecretKey: "s"},
			wantNote: "storage connection",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newConnBackedStore(map[int]*models.StorageConnection{
				3: testS3Connection(3, "live", "p"),
				4: testS3Connection(4, "packs", ""),
			})
			fs.kv[corestorage.KeyBackend] = "s3"
			tc.setup(fs)
			res := newResolver(testDeps(Deps{Store: fs}))
			_, _, err := res.ResolveTarget(context.Background(), tc.dataSet, tc.target)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ResolveTarget err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantNote != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantNote) {
					t.Fatalf("ResolveTarget err = %v, want a refusal mentioning %q", err, tc.wantNote)
				}
			default:
				if err != nil {
					t.Fatalf("ResolveTarget err = %v, want nil", err)
				}
			}
		})
	}
}

func seedInlineS3(fs *connBackedStore, prefix, bucket string) {
	fs.kv[prefix+"s3_endpoint"] = "https://s3.example.com"
	fs.kv[prefix+"s3_bucket"] = bucket
	fs.kv[prefix+"s3_access_key"] = "k"
	fs.kv[prefix+"s3_secret_key"] = "s"
}

// A modpacks target must hold its objects where the switched settings will read
// them. The s3 readers (modpack.S3Provider, and ConnectionProvider for a
// connection) are rooted at the bucket / connection prefix with no "modpacks"
// sub-directory, so the target label - built from the same sub-prefix as the
// target provider - must carry none either.
func TestResolveTarget_ModpacksS3TargetMatchesTheSwitchedReader(t *testing.T) {
	cases := []struct {
		name      string
		target    services.StorageTargetConfig
		wantLabel string
	}{
		{"inline s3", services.StorageTargetConfig{Backend: "s3", S3Endpoint: "https://s3.example.com", S3Bucket: "new", S3AccessKey: "k", S3SecretKey: "s"}, "s3:https://s3.example.com/new"},
		{"connection with a prefix", services.StorageTargetConfig{ConnectionID: 3}, "s3:https://s3.example.com/live/p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := newConnBackedStore(map[int]*models.StorageConnection{3: testS3Connection(3, "live", "p")})
			fs.kv["modpack_storage_provider"] = "local"
			fs.kv["modpack_storage_paths"] = `["/srv/old-modpacks"]`
			res := newResolver(testDeps(Deps{Store: fs}))
			_, label, err := res.ResolveTarget(context.Background(), "modpacks", tc.target)
			if err != nil {
				t.Fatalf("ResolveTarget: %v", err)
			}
			if label != tc.wantLabel {
				t.Fatalf("target label = %q, want %q (objects would land under a sub-prefix the switched reader never looks in)", label, tc.wantLabel)
			}
		})
	}
}

// The modpacks source and its label follow the selected connection, the way
// every live reader (Manager.BuildModpackProvider) does.
func TestResolve_ModpacksFollowsTheSelectedConnection(t *testing.T) {
	fs := newConnBackedStore(map[int]*models.StorageConnection{3: testS3Connection(3, "live", "p")})
	fs.kv["modpack_storage_provider"] = "local"
	fs.kv["modpack_storage_paths"] = `["/srv/old-modpacks"]`
	fs.kv[corestorage.KeyModpackConnectionID] = "3"
	res := newResolver(testDeps(Deps{Store: fs}))

	_, label, err := res.Resolve(context.Background(), "modpacks")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if label != "s3:https://s3.example.com/live/p" {
		t.Fatalf("modpacks source label = %q, want the selected connection", label)
	}
}
