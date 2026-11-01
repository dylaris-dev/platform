package storage

import (
	"context"
	"encoding/json"
	"strings"

	"dylaris-core/storage/backup"
)

// NewTenantS3Provider is an S3Provider on a bucket a TENANT connected. It is
// built on backup.NewTenantS3, so every dial goes through netguard; the
// endpoint is tenant-typed and must not reach our internal network.
func NewTenantS3Provider(ctx context.Context, raw json.RawMessage, prefix string) (StorageProvider, error) {
	client, err := backup.NewTenantS3(ctx, raw)
	if err != nil {
		return nil, err
	}
	return &S3Provider{os: client, prefix: strings.Trim(prefix, "/")}, nil
}
