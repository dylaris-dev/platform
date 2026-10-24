package storageapi

import (
	"encoding/json"
	"testing"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
)

// TestTargetCoreStorageConfig_ResolvesConnection: a migration target that names
// a connection is built from the connection's credentials (decrypted in the
// store), with ConnectionID preserved so a switch persists the reference.
func TestTargetCoreStorageConfig_ResolvesConnection(t *testing.T) {
	connCfg, _ := json.Marshal(handlers.StorageConnectionConfig{Endpoint: "https://e", Region: "eu", Bucket: "cb", ForcePathStyle: true, Prefix: "px"})
	fs := &resolveFakeStore{conn: &models.StorageConnection{ID: 5, Provider: "s3", Config: connCfg, AccessKey: "AK", SecretAccessKey: "SK"}}
	res := NewStorageDataSetResolver(&handlers.AppState{Store: fs})

	cfg, err := res.targetCoreStorageConfig(services.StorageTargetConfig{ConnectionID: 5})
	if err != nil {
		t.Fatalf("targetCoreStorageConfig: %v", err)
	}
	if cfg.Backend != "s3" || cfg.S3Bucket != "cb" || cfg.S3AccessKey != "AK" || cfg.S3SecretKey != "SK" {
		t.Fatalf("config not from the connection: %+v", cfg)
	}
	if cfg.ConnectionID != 5 || !cfg.S3PathStyle || cfg.S3Prefix != "px" || cfg.S3Endpoint != "https://e" {
		t.Fatalf("connection fields not mapped/preserved: %+v", cfg)
	}
}

func TestTargetCoreStorageConfig_InlinePassThrough(t *testing.T) {
	res := NewStorageDataSetResolver(&handlers.AppState{Store: &resolveFakeStore{}})
	cfg, err := res.targetCoreStorageConfig(services.StorageTargetConfig{Backend: "s3", S3Bucket: "inline", S3AccessKey: "k", S3SecretKey: "s"})
	if err != nil {
		t.Fatalf("targetCoreStorageConfig: %v", err)
	}
	if cfg.S3Bucket != "inline" || cfg.ConnectionID != 0 {
		t.Fatalf("inline target not passed through: %+v", cfg)
	}
}

func TestTargetCoreStorageConfig_MissingConnectionErrors(t *testing.T) {
	fs := &resolveFakeStore{connErr: errTestNoConn}
	res := NewStorageDataSetResolver(&handlers.AppState{Store: fs})
	if _, err := res.targetCoreStorageConfig(services.StorageTargetConfig{ConnectionID: 9}); err == nil {
		t.Fatal("targetCoreStorageConfig(missing conn) err = nil, want an error")
	}
}

var errTestNoConn = testConnErr{}

type testConnErr struct{}

func (testConnErr) Error() string { return "no such connection" }

// TestSwitchModpackConfig_PersistsConnectionID: switching modpacks to a
// connection target persists modpack_storage_connection_id.
func TestSwitchModpackConfig_PersistsConnectionID(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	res := NewStorageDataSetResolver(&handlers.AppState{Store: fs})

	cfg := handlers.CoreStorageConfig{Backend: "s3", ConnectionID: 5, S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "s"}
	if err := res.switchModpackConfig(cfg); err != nil {
		t.Fatalf("switchModpackConfig: %v", err)
	}
	if fs.kv[handlers.KeyModpackStorageConnectionID] != "5" {
		t.Errorf("modpack connection id not persisted, got %q", fs.kv[handlers.KeyModpackStorageConnectionID])
	}
}

// TestSwitchModpackConfig_InlineClearsStaleConnectionID: switching modpacks to an
// inline target clears a previously-selected connection (the flagged bug).
func TestSwitchModpackConfig_InlineClearsStaleConnectionID(t *testing.T) {
	fs := newCoreStorageHTTPFakeStore()
	fs.kv[handlers.KeyModpackStorageConnectionID] = "9" // a previously-selected connection
	res := NewStorageDataSetResolver(&handlers.AppState{Store: fs})

	cfg := handlers.CoreStorageConfig{Backend: "s3", ConnectionID: 0, S3Bucket: "b", S3AccessKey: "k", S3SecretKey: "s"}
	if err := res.switchModpackConfig(cfg); err != nil {
		t.Fatalf("switchModpackConfig: %v", err)
	}
	if fs.kv[handlers.KeyModpackStorageConnectionID] != "" {
		t.Errorf("inline switch left a stale connection id: %q", fs.kv[handlers.KeyModpackStorageConnectionID])
	}
}
