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

// P14: a node sent the Beam peer as "ip:port", and InsertServerAudit casts it
// to INET, so Postgres refused every Beam row. Old nodes still send it that way.
func TestTheAuditAddressIsWhatAnInetColumnTakes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"10.0.1.7:43122", "10.0.1.7"},
		{"[fd00::5]:5100", "fd00::5"},
		{"10.0.1.7", "10.0.1.7"},
		{"2001:db8::/64", "2001:db8::/64"},
		{"unknown", ""},
		{"", ""},
	} {
		fs := &sftpAuditFakeStore{node: models.Node{Token: "n1"}}
		NewSFTPAuditService(fs, nil, nil).apply(queue.SFTPAuditChannel("n1"),
			queue.SFTPAuditRecord{ServerUUID: "srv", Username: "bob", Writes: 1, Via: "beam", RemoteIP: tc.in})
		if len(fs.written) != 1 || fs.written[0].IPAddress != tc.want {
			t.Errorf("RemoteIP %q: rows = %+v, want IPAddress %q", tc.in, fs.written, tc.want)
		}
	}
}

type sftpAuditByIDStore struct {
	sftpAuditFakeStore
}

func (f *sftpAuditByIDStore) GetUserByID(id string) (*models.User, error) {
	if id == "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b" {
		return &models.User{ID: id}, nil
	}
	return nil, nil
}

// R53-05: the actor was resolved by the name in the record when the row came
// in, so after a rename a still-open Beam session's changes went to whoever
// took the old name. A record with the user id is resolved by it; one from an
// older node or ticket still by name.
func TestTheAuditActorIsResolvedByUserIDWhenTheRecordHasOne(t *testing.T) {
	for _, tc := range []struct {
		name, userID string
		want         *string
	}{
		{"record with the id", "6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b", strPtr("6f1c2a7e-3b4d-4e5f-8a9b-0c1d2e3f4a5b")},
		{"record from an older node", "", strPtr("u-bob")},
		{"id of a deleted account", "00000000-0000-4000-8000-000000000000", nil},
		{"not an id", "bob", strPtr("u-bob")},
	} {
		fs := &sftpAuditByIDStore{sftpAuditFakeStore{node: models.Node{Token: "n1"}}}
		NewSFTPAuditService(fs, nil, nil).apply(queue.SFTPAuditChannel("n1"),
			queue.SFTPAuditRecord{ServerUUID: "srv", Username: "bob", UserID: tc.userID, Writes: 1, Via: "beam"})
		if len(fs.written) != 1 {
			t.Fatalf("%s: %d rows", tc.name, len(fs.written))
		}
		got := fs.written[0].ActorUserID
		if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
			t.Errorf("%s: actor = %v, want %v", tc.name, got, tc.want)
		}
		if fs.written[0].Metadata["username"] != "bob" {
			t.Errorf("%s: username = %v, want bob kept for display", tc.name, fs.written[0].Metadata["username"])
		}
	}
}
