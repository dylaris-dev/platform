package handlers

import (
	"testing"

	"dylaris-core/models"
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
	base := func() []models.Server {
		return []models.Server{
			{ID: 10, NodeID: 1, NodeAddress: "203.0.113.9"},
			{ID: 11, NodeID: 2, NodeAddress: "198.51.100.4"},
		}
	}
	nodes := map[int]*models.Node{1: platformNode, 2: ownNode}

	cases := []struct {
		name     string
		settings map[string]string
		isAdmin  bool
		userID   string
		want     []string // per server, "" = withheld
	}{
		{
			name:     "gateway routing, beam files, a tenant",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "beam"},
			userID:   "tenant",
			// Their OWN machine keeps its address; the platform node does not.
			want: []string{"", "198.51.100.4"},
		},
		{
			name:     "an admin sees both",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "beam"},
			isAdmin:  true,
			userID:   "someone",
			want:     []string{"203.0.113.9", "198.51.100.4"},
		},
		{
			// Without the gateway the address IS how players connect, so
			// withholding it would break the panel's connect line.
			name:     "direct routing keeps it",
			settings: map[string]string{"routing_mode": "ip_port", "file_access_mode": "beam"},
			userID:   "tenant",
			want:     []string{"203.0.113.9", "198.51.100.4"},
		},
		{
			// It is the SFTP host the customer types.
			name:     "sftp file access keeps it",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "sftp"},
			userID:   "tenant",
			want:     []string{"203.0.113.9", "198.51.100.4"},
		},
		{
			name:     "both file modes keep it",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "both"},
			userID:   "tenant",
			want:     []string{"203.0.113.9", "198.51.100.4"},
		},
		{
			// A viewer who owns nothing: an invited friend on someone else's
			// server, which is exactly how this was found.
			name:     "an invited viewer gets neither",
			settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "beam"},
			userID:   "friend",
			want:     []string{"", ""},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &AppState{Store: &redactFakeStore{settings: tc.settings, nodes: nodes}}
			servers := base()
			redactNodeAddress(st, servers, tc.isAdmin, tc.userID)
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
	st := &AppState{Store: &redactFakeStore{
		settings: map[string]string{"routing_mode": "gateway", "file_access_mode": "beam"},
		nodes:    map[int]*models.Node{1: {ID: 1}},
	}}
	srv := &models.Server{ID: 10, NodeID: 1, NodeAddress: "203.0.113.9"}
	redactNodeAddressOne(st, srv, false, "tenant")
	if srv.NodeAddress != "" {
		t.Errorf("address = %q, want it withheld", srv.NodeAddress)
	}
}
