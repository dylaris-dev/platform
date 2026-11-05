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

// A Beam change is recorded as one, not as SFTP. A customer's machine serves
// no SFTP but does serve Beam - its only file path - so its Beam records count.
func TestABeamRecordIsWrittenAsABeamChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		node models.Node
	}{
		{"its own node", models.Node{Token: "n1"}},
		{"a customer's machine", models.Node{Token: "n1", Tags: "external"}},
	} {
		fs := &sftpAuditFakeStore{node: tc.node}
		NewSFTPAuditService(fs, nil, nil).apply(queue.SFTPAuditChannel("n1"),
			queue.SFTPAuditRecord{ServerUUID: "srv", Username: "bob", Writes: 1, Via: "beam"})
		if len(fs.written) != 1 || fs.written[0].EventType != ServerAuditEventBeamChanges || fs.written[0].UserAgent != "beam" {
			t.Errorf("%s: rows = %+v", tc.name, fs.written)
		}
	}
}

// G32: a customer's machine names the user in its own Beam records. It was
// resolved to the actor like a record from one of ours, so a BYON node could
// put any platform user's name on a change. Kept, but as the node's claim.
func TestABeamRecordFromACustomersMachineIsMarkedAsItsClaim(t *testing.T) {
	for _, tc := range []struct {
		name     string
		node     models.Node
		actor    bool
		reported any
	}{
		{"its own node", models.Node{Token: "n1"}, true, nil},
		{"a customer's machine", models.Node{Token: "n1", Tags: "external"}, false, "external-node"},
	} {
		fs := &sftpAuditFakeStore{node: tc.node}
		NewSFTPAuditService(fs, nil, nil).apply(queue.SFTPAuditChannel("n1"),
			queue.SFTPAuditRecord{ServerUUID: "srv", Username: "bob", Writes: 1, Via: "beam"})
		if len(fs.written) != 1 {
			t.Fatalf("%s: %d rows", tc.name, len(fs.written))
		}
		ev := fs.written[0]
		if (ev.ActorUserID != nil) != tc.actor {
			t.Errorf("%s: actor = %v, want set=%v", tc.name, ev.ActorUserID, tc.actor)
		}
		if got := ev.Metadata["reported_by"]; got != tc.reported {
			t.Errorf("%s: reported_by = %v, want %v", tc.name, got, tc.reported)
		}
	}
}
