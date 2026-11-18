package access_test

import (
	"testing"

	"dylaris-core/handlers/access"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

type redactFakeStore struct {
	store.Store
	settings map[string]string
	nodes    map[int]*models.Node
}

func (f *redactFakeStore) GetSetting(k string) (string, error) { return f.settings[k], nil }
func (f *redactFakeStore) GetNodeByID(id int) (*models.Node, error) {
	return f.nodes[id], nil
}

func strp(s string) *string { return &s }

// The node's public address is what an attacker would rather have than the
// edge's: in gateway routing the containers bind no host port, so traffic sent
// at the node never passes the edge. A viewer who is not an admin, on a machine
// that is not theirs, has no use for it.
func TestNodeAddressIsWithheldFromAnOrdinaryViewer(t *testing.T) {
	platformNode := &models.Node{ID: 1}
	ownNode := &models.Node{ID: 2, OwnerID: strp("tenant")}
	externalNode := &models.Node{ID: 3, Tags: "eu, external"}
	base := func() []models.Server {
		return []models.Server{
			{ID: 10, NodeID: 1, NodeAddress: "203.0.113.9"},
			{ID: 11, NodeID: 2, NodeAddress: "198.51.100.4"},
			{ID: 12, NodeID: 3, NodeAddress: "192.0.2.7"},
		}
	}
	nodes := map[int]*models.Node{1: platformNode, 2: ownNode, 3: externalNode}

	cases := []struct {
		name     string
		settings map[string]string
		isAdmin  bool
		userID   string
		sftp     map[int]bool // servers the caller holds sftp.access on
		noSFTP   bool         // no resolver: mayUseSFTP is nil
		want     []string     // per server, "" = withheld
	}{
		{
			name:     "gateway routing, beam files, a tenant",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "beam"},
			userID:   "tenant",
			// Their OWN machine keeps its address; the platform node does not.
			want: []string{"", "198.51.100.4", ""},
		},
		{
			name:     "an admin sees both",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "beam"},
			isAdmin:  true,
			userID:   "someone",
			want:     []string{"203.0.113.9", "198.51.100.4", "192.0.2.7"},
		},
		{
			// Without the gateway the address IS how players connect, so
			// withholding it would break the panel's connect line.
			name:     "direct routing keeps it",
			settings: map[string]string{"routing_mode": "ip_port", "file_access_mode": "beam"},
			userID:   "tenant",
			want:     []string{"203.0.113.9", "198.51.100.4", "192.0.2.7"},
		},
		{
			// It is the SFTP host the customer types - but only for a caller
			// who may use SFTP on that server, and never on an external node,
			// where SFTP does not apply (the sftp-credentials route's rules).
			name:     "sftp file access keeps it where the caller holds sftp.access",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "sftp"},
			userID:   "friend",
			sftp:     map[int]bool{10: true, 12: true},
			want:     []string{"203.0.113.9", "", ""},
		},
		{
			name:     "both file modes keep it where the caller holds sftp.access",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "both"},
			userID:   "friend",
			sftp:     map[int]bool{10: true},
			want:     []string{"203.0.113.9", "", ""},
		},
		{
			// A console-only guest, or a demo viewer: the platform-wide file
			// mode used to hand every one of them the node's address.
			name:     "sftp file mode without sftp.access withholds it",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "sftp"},
			userID:   "friend",
			want:     []string{"", "", ""},
		},
		{
			name:     "sftp file mode, no resolver: only own machines keep it",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "both"},
			userID:   "tenant",
			noSFTP:   true,
			want:     []string{"", "198.51.100.4", ""},
		},
		{
			// A viewer who owns nothing: an invited friend on someone else's
			// server, which is exactly how this was found.
			name:     "an invited viewer gets neither",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "beam"},
			userID:   "friend",
			want:     []string{"", "", ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &redactFakeStore{settings: tc.settings, nodes: nodes}
			servers := base()
			mayUseSFTP := func(id int) bool { return tc.sftp[id] }
			if tc.noSFTP {
				mayUseSFTP = nil
			}
			access.RedactNodeAddress(st, func() bool { return services.GatewayRoutingEnabled(st) }, mayUseSFTP, servers, tc.isAdmin, tc.userID)
			for i := range servers {
				if servers[i].NodeAddress != tc.want[i] {
					t.Errorf("server %d address = %q, want %q", servers[i].ID, servers[i].NodeAddress, tc.want[i])
				}
			}
		})
	}
}

// The single-server form has to answer the same, or the one endpoint that uses
// it becomes the way around the list.
func TestNodeAddressIsWithheldOnASingleServerToo(t *testing.T) {
	st := &redactFakeStore{
		settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "beam"},
		nodes:    map[int]*models.Node{1: {ID: 1}},
	}
	srv := &models.Server{ID: 10, NodeID: 1, NodeAddress: "203.0.113.9"}
	access.RedactNodeAddressOne(st, func() bool { return services.GatewayRoutingEnabled(st) }, nil, srv, false, "tenant")
	if srv.NodeAddress != "" {
		t.Errorf("address = %q, want it withheld", srv.NodeAddress)
	}
}

// countingRedactStore counts node reads.
type countingRedactStore struct {
	redactFakeStore
	nodeReads int
}

func (f *countingRedactStore) GetNodeByID(id int) (*models.Node, error) {
	f.nodeReads++
	return f.redactFakeStore.GetNodeByID(id)
}

// List rows carry their node's owner, so redaction reads no node at all and
// gives the answer the node read gave: own machine keeps the address, anyone
// else's or the platform's loses it.
func TestNodeAddressFromTheJoinedOwner(t *testing.T) {
	st := &countingRedactStore{redactFakeStore: redactFakeStore{settings: map[string]string{"routing_mode": "gateway"}}}
	servers := []models.Server{
		{ID: 10, NodeID: 1, NodeAddress: "203.0.113.9", NodeOwnerLoaded: true},
		{ID: 11, NodeID: 2, NodeAddress: "198.51.100.4", NodeOwnerLoaded: true, NodeOwnerID: strp("tenant")},
		{ID: 12, NodeID: 3, NodeAddress: "192.0.2.7", NodeOwnerLoaded: true, NodeOwnerID: strp("someone-else")},
	}
	access.RedactNodeAddress(st, func() bool { return true }, nil, servers, false, "tenant")
	if st.nodeReads != 0 {
		t.Fatalf("read %d nodes, want 0", st.nodeReads)
	}
	for i, want := range []string{"", "198.51.100.4", ""} {
		if servers[i].NodeAddress != want {
			t.Errorf("server %d address %q, want %q", servers[i].ID, servers[i].NodeAddress, want)
		}
	}
}
