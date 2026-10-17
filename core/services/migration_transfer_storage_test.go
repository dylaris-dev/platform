package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
)

// A cross-LAN move rides the default backup storage. Production's default is a
// storage connection, and only "s3" used to be accepted: every BYON move
// across LANs failed although the same bucket held that server's backups.
func TestOpenTransferStorage(t *testing.T) {
	var opened int
	conn := func(id int, prefix string) (backupstorage.Storage, error) {
		opened = id
		return nil, errors.New("stub: reached the connection builder")
	}
	cfg, _ := json.Marshal(backupstorage.ConnectionConfig{ConnectionID: 7, Prefix: "backups"})

	_, err := openTransferStorage(context.Background(), &models.BackupStorage{Provider: "connection", Config: cfg}, conn)
	if opened != 7 {
		t.Fatalf("a connection-backed default storage never reached its builder (err %v)", err)
	}

	for _, bs := range []*models.BackupStorage{nil, {Provider: "local"}, {Provider: "node-local"}} {
		if _, err := openTransferStorage(context.Background(), bs, conn); err == nil || !strings.Contains(err.Error(), "requires an S3/R2 backup storage") {
			t.Errorf("%+v: err %v, want the S3/R2 requirement", bs, err)
		}
	}
}
