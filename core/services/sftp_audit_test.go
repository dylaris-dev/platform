package services

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
	"dylaris-pkg/queue"
)

type sftpAuditFakeStore struct {
	store.Store
	node    models.Node
	written []models.ServerAuditEvent
}

func (f *sftpAuditFakeStore) GetServerByUUID(uuid string) (*models.Server, error) {
	return &models.Server{ID: 7, UUID: uuid, NodeID: 3}, nil
}
func (f *sftpAuditFakeStore) GetNodeByID(int) (*models.Node, error) { n := f.node; return &n, nil }
func (f *sftpAuditFakeStore) GetServerAuditState(int) (bool, bool, int, error) {
	return true, false, 0, nil
}
func (f *sftpAuditFakeStore) GetUserByUsername(string) (*models.User, error) {
	return &models.User{ID: "u-bob"}, nil
}
func (f *sftpAuditFakeStore) InsertServerAudit(e *models.ServerAuditEvent) error {
	f.written = append(f.written, *e)
	return nil
}

// SFTP changes left no row in the server audit trail. A record is written only
// when the node that sent it hosts the server and is one of ours.
func TestAnSFTPSessionsChangesReachTheServerAudit(t *testing.T) {
	rec := queue.SFTPAuditRecord{ServerUUID: "srv", Username: "bob", Deletes: 1, Paths: []string{"plugins/x.jar"}}
	for _, tc := range []struct {
		name    string
		node    models.Node
		channel string
		want    int
	}{
		{"its own node", models.Node{Token: "n1"}, queue.SFTPAuditChannel("n1"), 1},
		{"another node", models.Node{Token: "n1"}, queue.SFTPAuditChannel("n2"), 0},
		{"a customer's machine", models.Node{Token: "n1", Tags: "external"}, queue.SFTPAuditChannel("n1"), 0},
	} {
		fs := &sftpAuditFakeStore{node: tc.node}
		NewSFTPAuditService(fs, nil, nil).apply(tc.channel, rec)
		if len(fs.written) != tc.want {
			t.Errorf("%s: %d rows, want %d", tc.name, len(fs.written), tc.want)
			continue
		}
		if tc.want == 1 && (fs.written[0].ServerID != 7 || fs.written[0].ActorUserID == nil || *fs.written[0].ActorUserID != "u-bob") {
			t.Errorf("%s: row %+v", tc.name, fs.written[0])
		}
	}
}
