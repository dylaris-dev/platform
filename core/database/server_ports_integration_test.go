package database

import (
	"errors"
	"strconv"
	"testing"

	"dylaris-core/store"
)

// The unique key is per node AND protocol, the free-port search skips what
// the node holds, the overview search finds a port by number and by server,
// a NULL limit row is "no limit" while a missing one defers, and a deleted
// server takes its ports with it. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationServerPorts(t *testing.T) {
	_, st := integrationDB(t)
	a := newFixture(t, st)
	b := newFixture(t, st)

	p, err := st.CreateServerPort(store.ServerPort{ServerID: a.server.ID, NodeID: a.node.ID, Protocol: "udp", HostPort: 26000,
		ContainerPort: 24454, Name: "Voice", Preset: "svc", CreatedBy: a.user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateServerPort(store.ServerPort{ServerID: b.server.ID, NodeID: a.node.ID, Protocol: "udp", HostPort: 26000, ContainerPort: 1, Name: "x"}); !errors.Is(err, store.ErrPortTaken) {
		t.Fatalf("same node, protocol and port: %v, want ErrPortTaken", err)
	}
	if _, err := st.CreateServerPort(store.ServerPort{ServerID: b.server.ID, NodeID: a.node.ID, Protocol: "tcp", HostPort: 26000, ContainerPort: 1, Name: "tcp twin"}); err != nil {
		t.Fatalf("TCP shares the number with UDP: %v", err)
	}
	if _, err := st.CreateServerPort(store.ServerPort{ServerID: b.server.ID, NodeID: b.node.ID, Protocol: "udp", HostPort: 26000, ContainerPort: 1, Name: "other node"}); err != nil {
		t.Fatalf("another node has its own numbers: %v", err)
	}

	if free, err := st.FreeServerPort(a.node.ID, "udp", 26000, 26002, nil); err != nil || free != 26001 {
		t.Fatalf("FreeServerPort = %d, %v, want 26001", free, err)
	}
	if _, err := st.FreeServerPort(a.node.ID, "udp", 26000, 26000, nil); !errors.Is(err, store.ErrPortRangeFull) {
		t.Fatalf("full range: %v", err)
	}

	got, err := st.GetServerPort(p.ID)
	if err != nil || got == nil || got.Preset != "svc" || got.CreatedBy != a.user.ID {
		t.Fatalf("GetServerPort = %+v, %v", got, err)
	}
	if n, _ := st.CountServerPortsByOwner(b.user.ID); n != 2 {
		t.Fatalf("b holds %d ports, want 2", n)
	}
	rows, err := st.ListAllServerPorts(strconv.Itoa(24454))
	if err != nil || len(rows) != 1 || rows[0].ServerName != a.server.Name || rows[0].OwnerName != a.user.Username {
		t.Fatalf("search by container port = %+v, %v", rows, err)
	}
	if rows, _ := st.ListAllServerPorts(b.server.Name); len(rows) != 2 {
		t.Fatalf("search by server name: %d rows, want 2", len(rows))
	}

	scope := "user:" + a.user.ID
	if l, _ := st.GetPortLimit(scope); l != nil {
		t.Fatal("a scope without a row must say nothing")
	}
	if err := st.SetPortLimit(scope, nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := st.GetPortLimit(scope); l == nil || l.MaxPorts != nil {
		t.Fatalf("NULL row = %+v, want a row with no limit", l)
	}
	zero := 0
	st.SetPortLimit(scope, &zero)
	if l, _ := st.GetPortLimit(scope); l == nil || l.MaxPorts == nil || *l.MaxPorts != 0 {
		t.Fatalf("0 row = %+v", l)
	}
	st.DeletePortLimit(scope)

	if err := st.SetNodeExtraPortRange(a.node.ID, "27000-27009"); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.GetNodeExtraPortRanges(); r[a.node.ID] != "27000-27009" {
		t.Fatalf("node range = %q", r[a.node.ID])
	}
	st.SetNodeExtraPortRange(a.node.ID, "")
	if r, _ := st.GetNodeExtraPortRanges(); r[a.node.ID] != "" {
		t.Fatal("an empty range must go back to the global one")
	}

	if err := st.DeleteServer(a.server.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetServerPort(p.ID); got != nil {
		t.Fatal("a deleted server's port survived")
	}
}
