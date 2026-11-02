package corestorage

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-core/storage"
	"dylaris-core/storage/modpack"
)

// KeyConnectionID names the setting that, when set to a storage
// connection id, makes Core file storage build from that named connection
// instead of the inline core_storage_s3_* keys. Empty or "0" means "use the
// inline config" - the fallback the spec keeps open.
const KeyConnectionID = "core_storage_connection_id"

// KeyModpackConnectionID names the setting that, when set to a storage
// connection id, makes modpack storage build from that named connection instead
// of the inline modpack_storage_s3_* keys. Empty or "0" = use the inline config.
const KeyModpackConnectionID = "modpack_storage_connection_id"

// ConnIDSetting formats a connection id for persistence: 0 stores "" (no
// connection selected) so an empty setting reads back as "none".
func ConnIDSetting(id int) string {
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
func (s *Manager) selectedStorageConnection(idSetting string) (*models.StorageConnection, bool, error) {
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

// ConfigFromConnection maps a resolved connection (its secret already
// decrypted into SecretAccessKey by the store) onto a Config. The
// backend is always s3; the access key and secret come from their own columns,
// the rest from the connection's config JSONB.
func ConfigFromConnection(conn *models.StorageConnection) Config {
	var c ConnectionConfig
	if len(conn.Config) > 0 {
		_ = json.Unmarshal(conn.Config, &c)
	}
	return Config{
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

// EffectiveConfig returns the config Core file storage is actually
// built from. A selected storage connection overrides the inline config
// entirely; otherwise the inline config is returned unchanged. This is the
// single place the "connection overrides inline" rule lives, so the provider
// builder, the gate sync and the configured-check all agree.
func (s *Manager) EffectiveConfig() (Config, error) {
	conn, ok, err := s.selectedStorageConnection(KeyConnectionID)
	if err != nil {
		return Config{}, err
	}
	if ok {
		return ConfigFromConnection(conn), nil
	}
	return s.LoadConfig(), nil
}

// BuildModpackProvider builds the modpack storage provider, resolving a
// selected storage connection first. When modpack_storage_connection_id is set,
// the connection is built as an s3 storage.StorageProvider (which supports
// path-style and prefix, unlike modpack's native NewS3) and adapted to the
// modpack interface via NewCoreStorageProvider - the same adapter the
// "core-storage" modpack backend already uses. Otherwise it delegates to the
// inline modpack_storage_* settings unchanged, so an install with no connection
// selected behaves exactly as before.
func (s *Manager) BuildModpackProvider() (modpack.ModpackStorageProvider, error) {
	conn, ok, err := s.selectedStorageConnection(KeyModpackConnectionID)
	if err != nil {
		return nil, err
	}
	if ok {
		prov, err := ConnectionProvider(conn)
		if err != nil {
			return nil, err
		}
		return modpack.NewCoreStorageProvider(prov), nil
	}
	return modpack.NewProviderFromSettings(s.Store.GetSetting, s.BuildProvider)
}

// ConnectionConfig is the non-secret portion of a connection's config
// JSONB. The access key and the secret live in their own columns, never here.
type ConnectionConfig struct {
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	ForcePathStyle bool   `json:"forcePathStyle"`
	Prefix         string `json:"prefix"`
}

// ConnectionProvider builds a storage.StorageProvider from a resolved
// connection (its secret already decrypted into SecretAccessKey). The access key
// and secret come from their own columns; the rest from the config JSONB.
func ConnectionProvider(conn *models.StorageConnection) (storage.StorageProvider, error) {
	if conn.Provider != "s3" {
		return nil, fmt.Errorf("unsupported provider %q", conn.Provider)
	}
	var cfg ConnectionConfig
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
		storage.OptS3PathStyle: strconv.FormatBool(cfg.ForcePathStyle),
		storage.OptS3Prefix:    cfg.Prefix,
	})
}
