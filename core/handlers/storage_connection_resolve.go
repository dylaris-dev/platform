package handlers

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-core/storage"
	"dylaris-core/storage/modpack"
)

// keyCoreStorageConnectionID names the setting that, when set to a storage
// connection id, makes Core file storage build from that named connection
// instead of the inline core_storage_s3_* keys. Empty or "0" means "use the
// inline config" - the fallback the spec keeps open.
const keyCoreStorageConnectionID = "core_storage_connection_id"

// keyModpackStorageConnectionID names the setting that, when set to a storage
// connection id, makes modpack storage build from that named connection instead
// of the inline modpack_storage_s3_* keys. Empty or "0" = use the inline config.
const keyModpackStorageConnectionID = "modpack_storage_connection_id"

// storageConnIDSetting formats a connection id for persistence: 0 stores "" (no
// connection selected) so an empty setting reads back as "none".
func storageConnIDSetting(id int) string {
	if id == 0 {
		return ""
	}
	return strconv.Itoa(id)
}

// selectedStorageConnection resolves the connection referenced by the given
// id-setting. ok is false (with a nil connection and nil error) when nothing is
// selected, so a caller falls back to its inline config. A selected-but-broken
// reference (non-numeric id, or a connection that no longer loads) is an ERROR,
// not a silent fallback: the operator chose a connection, so quietly building
// from possibly-stale inline config would hide the misconfiguration.
func (s *AppState) selectedStorageConnection(idSetting string) (*models.StorageConnection, bool, error) {
	if s.Store == nil {
		return nil, false, nil
	}
	raw, _ := s.Store.GetSetting(idSetting)
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "0" {
		return nil, false, nil
	}
	id, err := strconv.Atoi(raw)
	if err != nil {
		return nil, false, fmt.Errorf("storage connection: invalid id %q in %s: %w", raw, idSetting, err)
	}
	conn, err := s.Store.GetStorageConnection(id)
	if err != nil {
		return nil, false, fmt.Errorf("storage connection %d (%s) could not be loaded: %w", id, idSetting, err)
	}
	return conn, true, nil
}

// coreStorageConfigFromConnection maps a resolved connection (its secret already
// decrypted into SecretAccessKey by the store) onto a CoreStorageConfig. The
// backend is always s3; the access key and secret come from their own columns,
// the rest from the connection's config JSONB.
func coreStorageConfigFromConnection(conn *models.StorageConnection) CoreStorageConfig {
	var c storageConnectionConfig
	if len(conn.Config) > 0 {
		_ = json.Unmarshal(conn.Config, &c)
	}
	return CoreStorageConfig{
		Backend:     "s3",
		S3Endpoint:  c.Endpoint,
		S3Region:    c.Region,
		S3Bucket:    c.Bucket,
		S3AccessKey: conn.AccessKey,
		S3SecretKey: conn.SecretAccessKey,
		S3PathStyle: c.ForcePathStyle,
		S3Prefix:    c.Prefix,
		S3SecretSet: conn.SecretAccessKey != "",
	}
}

// effectiveCoreStorageConfig returns the config Core file storage is actually
// built from. A selected storage connection overrides the inline config
// entirely; otherwise the inline config is returned unchanged. This is the
// single place the "connection overrides inline" rule lives, so the provider
// builder, the gate sync and the configured-check all agree.
func (s *AppState) effectiveCoreStorageConfig() (CoreStorageConfig, error) {
	conn, ok, err := s.selectedStorageConnection(keyCoreStorageConnectionID)
	if err != nil {
		return CoreStorageConfig{}, err
	}
	if ok {
		return coreStorageConfigFromConnection(conn), nil
	}
	return s.LoadCoreStorageConfig(), nil
}

// buildModpackStorageProvider builds the modpack storage provider, resolving a
// selected storage connection first. When modpack_storage_connection_id is set,
// the connection is built as an s3 storage.StorageProvider (which supports
// path-style and prefix, unlike modpack's native NewS3) and adapted to the
// modpack interface via NewCoreStorageProvider - the same adapter the
// "core-storage" modpack backend already uses. Otherwise it delegates to the
// inline modpack_storage_* settings unchanged, so an install with no connection
// selected behaves exactly as before.
func (s *AppState) buildModpackStorageProvider() (modpack.ModpackStorageProvider, error) {
	conn, ok, err := s.selectedStorageConnection(keyModpackStorageConnectionID)
	if err != nil {
		return nil, err
	}
	if ok {
		prov, err := storageConnectionProvider(conn)
		if err != nil {
			return nil, err
		}
		return modpack.NewCoreStorageProvider(prov), nil
	}
	return modpack.NewProviderFromSettings(s.Store.GetSetting, s.buildCoreStorageProvider)
}

// storageConnectionConfig is the non-secret portion of a connection's config
// JSONB. The access key and the secret live in their own columns, never here.
type storageConnectionConfig struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	ForcePathStyle bool   `json:"forcePathStyle"`
	Prefix         string `json:"prefix"`
}

// storageConnectionProvider builds a storage.StorageProvider from a resolved
// connection (its secret already decrypted into SecretAccessKey). The access key
// and secret come from their own columns; the rest from the config JSONB.
func storageConnectionProvider(conn *models.StorageConnection) (storage.StorageProvider, error) {
	if conn.Provider != "s3" {
		return nil, fmt.Errorf("unsupported provider %q", conn.Provider)
	}
	var cfg storageConnectionConfig
	if len(conn.Config) > 0 {
		if err := json.Unmarshal(conn.Config, &cfg); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
	}
	return storage.NewProvider("s3", "", map[string]string{
		storage.OptS3Endpoint:  cfg.Endpoint,
		storage.OptS3Region:    cfg.Region,
		storage.OptS3Bucket:    cfg.Bucket,
		storage.OptS3AccessKey: conn.AccessKey,
		storage.OptS3SecretKey: conn.SecretAccessKey,
		storage.OptS3PathStyle: boolStr(cfg.ForcePathStyle),
		storage.OptS3Prefix:    cfg.Prefix,
	})
}
