package store

import (
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ListServers used to select the node NAME and never the node ID, so every row
// came back with NodeID = 0.
//
// Nothing failed. 0 is a legal int, the rows were otherwise complete, and the
// two callers that only wanted names never noticed. The one that grouped BY
// NODE - the network-policy publisher - filed the whole fleet under a machine
// that does not exist and published an empty policy for every real one, which
// on the node reads as "these servers may talk to nobody extra". Measured in
// production before this test existed.
//
// The assertion is on the returned STRUCT rather than on the SQL text, because
// the failure was a missing scan target, not a missing word: a query that
// selects the column and forgets to read it fails in exactly the same way.
func TestListServersCarriesTheNodeID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewPostgresStore(db)

	now := time.Now()
	cols := []string{
		"id", "uuid", "name", "node_id", "node_name", "owner_name", "port", "status",
		"desired_state", "game_image", "is_fixed", "active_sub_server", "created_at",
		"server_type", "proxy_id",
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM servers s")).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(7, "uuid-a", "alpha", 325, "node-a", "someone", 25565, "online",
				"running", "img", true, "sub", now, "game", nil).
			AddRow(8, "uuid-b", "beta", 327, "node-b", "someone", 25565, "offline",
				"stopped", "img", true, "sub", now, "proxy", 7))

	servers, err := s.ListServers("")
	if err != nil {
		t.Fatalf("ListServers: %v", err)
	}
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2 - a scan mismatch is swallowed by the loop's continue", len(servers))
	}
	if servers[0].NodeID != 325 || servers[1].NodeID != 327 {
		t.Errorf("node ids = %d, %d; want 325, 327. Anything grouping by node lands on a machine that does not exist",
			servers[0].NodeID, servers[1].NodeID)
	}
	// The rest of the row has to survive the added column, or fixing one caller
	// broke every other.
	if servers[0].UUID != "uuid-a" || servers[1].ServerType != "proxy" {
		t.Errorf("columns shifted: %+v / %+v", servers[0], servers[1])
	}
	if servers[1].ProxyID == nil || *servers[1].ProxyID != 7 {
		t.Errorf("proxy_id did not survive: %v", servers[1].ProxyID)
	}
}
