package corestorage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/storage"
	"dylaris-core/store"
)

// resolveFakeStore serves settings + one storage connection. Embeds store.Store
// so only the two methods the resolver touches need bodies.
type resolveFakeStore struct {
	store.Store
	settings map[string]string
	conn     *models.StorageConnection
	connErr  error
	askedID  int
}

func (f *resolveFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *resolveFakeStore) GetStorageConnection(id int) (*models.StorageConnection, error) {
	f.askedID = id
	if f.connErr != nil {
		return nil, f.connErr
	}
	return f.conn, nil
}

func TestSelectedStorageConnection_NoneSelected(t *testing.T) {
	for _, raw := range []string{"", "0", "  "} {
		fs := &resolveFakeStore{settings: map[string]string{KeyConnectionID: raw}}
		s := &Manager{Store: fs}
		conn, ok, err := s.selectedStorageConnection(KeyConnectionID)
		if err != nil || ok || conn != nil {
			t.Errorf("selected(%q) = (%v, %v, %v), want (nil, false, nil)", raw, conn, ok, err)
		}
	}
}

func TestSelectedStorageConnection_Loads(t *testing.T) {
	fs := &resolveFakeStore{
		settings: map[string]string{KeyConnectionID: "3"},
		conn:     &models.StorageConnection{ID: 3, Name: "nas", Provider: "s3"},
	}
	s := &Manager{Store: fs}
	conn, ok, err := s.selectedStorageConnection(KeyConnectionID)
	if err != nil || !ok || conn == nil || conn.ID != 3 {
		t.Fatalf("selected = (%v, %v, %v), want the id-3 connection", conn, ok, err)
	}
	if fs.askedID != 3 {
		t.Errorf("store asked for id %d, want 3", fs.askedID)
	}
}

func TestSelectedStorageConnection_BadIDErrors(t *testing.T) {
	fs := &resolveFakeStore{settings: map[string]string{KeyConnectionID: "not-a-number"}}
	s := &Manager{Store: fs}
	if _, ok, err := s.selectedStorageConnection(KeyConnectionID); err == nil || ok {
		t.Fatalf("selected(bad id) = (ok=%v, err=%v), want an error and ok=false", ok, err)
	}
}

func TestSelectedStorageConnection_LoadErrorSurfaces(t *testing.T) {
	fs := &resolveFakeStore{
		settings: map[string]string{KeyConnectionID: "9"},
		connErr:  errors.New("no such row"),
	}
	s := &Manager{Store: fs}
	if _, ok, err := s.selectedStorageConnection(KeyConnectionID); err == nil || ok {
		t.Fatalf("selected(missing conn) = (ok=%v, err=%v), want an error (not a silent inline fallback)", ok, err)
	}
}

// TestEffectiveCoreStorageConfig_ConnectionOverridesInline is the core guard: a
// selected connection's credentials must win over the inline core_storage_s3_*
// settings entirely.
func TestEffectiveCoreStorageConfig_ConnectionOverridesInline(t *testing.T) {
	connCfg, _ := json.Marshal(ConnectionConfig{
		Endpoint: "https://conn.example", Region: "eu-central", Bucket: "conn-bucket",
		ForcePathStyle: true, Prefix: "px",
	})
	fs := &resolveFakeStore{
		settings: map[string]string{
			KeyConnectionID: "3",
			// Inline config is DIFFERENT, to prove it is not used.
			KeyBackend:     "s3",
			KeyS3Bucket:    "inline-bucket",
			KeyS3AccessKey: "INLINE_AK",
			KeyS3SecretKey: "inline-secret",
		},
		conn: &models.StorageConnection{
			ID: 3, Name: "nas", Provider: "s3",
			Config: connCfg, AccessKey: "CONN_AK", SecretAccessKey: "conn-secret",
		},
	}
	s := &Manager{Store: fs}
	cfg, err := s.EffectiveConfig()
	if err != nil {
		t.Fatalf("EffectiveConfig: %v", err)
	}
	if cfg.Backend != "s3" || cfg.S3Bucket != "conn-bucket" || cfg.S3AccessKey != "CONN_AK" || cfg.S3SecretKey != "conn-secret" {
		t.Fatalf("effective config did not come from the connection: %+v", cfg)
	}
	if cfg.S3Endpoint != "https://conn.example" || cfg.S3Region != "eu-central" || !cfg.S3PathStyle || cfg.S3Prefix != "px" {
		t.Fatalf("connection config fields not mapped: %+v", cfg)
	}
	if !cfg.S3SecretSet {
		t.Error("S3SecretSet = false, want true (the connection has a secret)")
	}
}

// TestBuildModpackStorageProvider_UsesSelectedConnection: with a modpack
// connection selected, a provider is built from it (not from the inline modpack
// settings).
func TestBuildModpackStorageProvider_UsesSelectedConnection(t *testing.T) {
	connCfg, _ := json.Marshal(ConnectionConfig{Bucket: "mp-bucket", Region: "eu"})
	fs := &resolveFakeStore{
		settings: map[string]string{KeyModpackConnectionID: "5"},
		conn: &models.StorageConnection{
			ID: 5, Name: "nas", Provider: "s3",
			Config: connCfg, AccessKey: "AK", SecretAccessKey: "SK",
		},
	}
	s := &Manager{Store: fs}
	prov, err := s.BuildModpackProvider()
	if err != nil {
		t.Fatalf("BuildModpackProvider: %v", err)
	}
	if prov == nil {
		t.Fatal("provider is nil; a selected connection must yield a provider")
	}
}

// TestBuildModpackStorageProvider_DelegatesWhenNoConnection: with nothing
// selected it falls through to the inline settings, which here are unconfigured,
// so the factory's (nil, nil) "not configured" result is returned unchanged.
func TestBuildModpackStorageProvider_DelegatesWhenNoConnection(t *testing.T) {
	fs := &resolveFakeStore{settings: map[string]string{}} // no connection, no modpack settings
	s := &Manager{Store: fs}
	prov, err := s.BuildModpackProvider()
	if err != nil {
		t.Fatalf("BuildModpackProvider: %v", err)
	}
	if prov != nil {
		t.Fatalf("want nil (unconfigured inline), got a provider - delegation path is wrong")
	}
}

func TestBuildModpackStorageProvider_BadIDErrors(t *testing.T) {
	fs := &resolveFakeStore{settings: map[string]string{KeyModpackConnectionID: "nope"}}
	s := &Manager{Store: fs}
	if _, err := s.BuildModpackProvider(); err == nil {
		t.Fatal("BuildModpackProvider(bad id) err = nil, want an error")
	}
}

func TestEffectiveCoreStorageConfig_FallsBackToInline(t *testing.T) {
	fs := &resolveFakeStore{
		settings: map[string]string{
			KeyConnectionID: "", // nothing selected
			KeyBackend:      "s3",
			KeyS3Bucket:     "inline-bucket",
			KeyS3SecretKey:  "inline-secret",
		},
	}
	s := &Manager{Store: fs}
	cfg, err := s.EffectiveConfig()
	if err != nil {
		t.Fatalf("EffectiveConfig: %v", err)
	}
	if cfg.S3Bucket != "inline-bucket" || cfg.S3SecretKey != "inline-secret" {
		t.Fatalf("no-connection case did not fall back to the inline config: %+v", cfg)
	}
}

// A saved connection is a different backend from Core file storage, so its
// failures must not flip Core storage's shared S3 state into reconnecting
// (every Core storage upload then waits, and the panel reports an outage that
// does not exist), nor wait on that state when Core storage is the one down.
func TestConnectionProviders_DoNotShareCoreStorageS3State(t *testing.T) {
	// Port 1 on loopback refuses at once: a connection-class failure.
	connCfg, _ := json.Marshal(ConnectionConfig{Endpoint: "http://127.0.0.1:1", Region: "eu", Bucket: "b", ForcePathStyle: true})
	conn := &models.StorageConnection{ID: 7, Name: "customer", Provider: "s3", Config: connCfg, AccessKey: "AK", SecretAccessKey: "SK"}
	cases := []struct {
		name string
		call func(t *testing.T, m *Manager, ctx context.Context) error
	}{
		{"backup builder", func(t *testing.T, m *Manager, ctx context.Context) error {
			st, err := m.ConnectionBackupBuilder()(7, "backups")
			if err != nil {
				t.Fatalf("ConnectionBackupBuilder: %v", err)
			}
			_, err = st.List(ctx, "")
			return err
		}},
		{"template store", func(t *testing.T, m *Manager, ctx context.Context) error {
			prov, _, err := m.TemplateStore(&models.Template{ID: 1, OwnerID: "u", StorageConnectionID: 7})
			if err != nil {
				t.Fatalf("TemplateStore: %v", err)
			}
			_, err = prov.ListFiles(ctx, "")
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &Manager{Store: &resolveFakeStore{settings: map[string]string{}, conn: conn}, S3: storage.NewS3Resilience()}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.call(t, m, ctx); err == nil {
				t.Fatal("call against a refusing endpoint succeeded")
			}
			if reconnecting, _, _ := m.S3.State(); reconnecting {
				t.Fatal("a storage connection's failure put Core file storage's S3 state into reconnecting")
			}
		})
	}
}
