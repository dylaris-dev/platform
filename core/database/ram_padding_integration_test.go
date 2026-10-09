package database

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Capacity counts the container, not the booked RAM: memory + the padding
// resolved server ?? node ?? global ?? 512. Against a real Postgres because
// the resolution for the sum happens in SQL.
func TestIntegrationRAMPaddingCountsInCapacityAndResolves(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	f := newFixture(t, st) // server memory 1024, no overrides

	check := func(label string, wantSum int64, wantOverride *int, wantEffective int) {
		t.Helper()
		sum, _, err := st.SumAllocatedByNode(f.node.ID)
		if err != nil {
			t.Fatalf("%s: SumAllocatedByNode: %v", label, err)
		}
		if sum != wantSum {
			t.Errorf("%s: allocated %d MB, want %d", label, sum, wantSum)
		}
		srv, err := st.GetServerByID(f.server.ID)
		if err != nil {
			t.Fatalf("%s: GetServerByID: %v", label, err)
		}
		if (srv.RAMPaddingMB == nil) != (wantOverride == nil) || (srv.RAMPaddingMB != nil && *srv.RAMPaddingMB != *wantOverride) {
			t.Errorf("%s: override %v, want %v", label, srv.RAMPaddingMB, wantOverride)
		}
		if srv.EffectiveRAMPaddingMB != wantEffective {
			t.Errorf("%s: effective %d, want %d", label, srv.EffectiveRAMPaddingMB, wantEffective)
		}
	}
	p := func(n int) *int { return &n }

	check("nothing set", 1024+512, nil, 512)

	if err := st.SetNodeRAMPadding(f.node.ID, p(768)); err != nil {
		t.Fatal(err)
	}
	if n, err := st.GetNodeByID(f.node.ID); err != nil || n.RAMPaddingMB == nil || *n.RAMPaddingMB != 768 {
		t.Fatalf("node override read back as %v (%v)", n.RAMPaddingMB, err)
	}
	check("node 768", 1024+768, nil, 768)

	if err := st.SetServerRAMPadding(f.server.ID, p(0)); err != nil {
		t.Fatal(err)
	}
	check("server 0 beats node", 1024, p(0), 0)

	if err := st.SetServerRAMPadding(f.server.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeRAMPadding(f.node.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(models.RAMPaddingSetting, "256"); err != nil {
		t.Fatal(err)
	}
	check("global 256", 1024+256, nil, 256)

	// Garbage in the setting must not strip every container of its padding.
	if err := st.SetSetting(models.RAMPaddingSetting, "lots"); err != nil {
		t.Fatal(err)
	}
	check("garbled global", 1024+512, nil, 512)

	// A second server created with its own override is counted with it.
	second := &models.Server{
		UUID: f.server.UUID + "_2", Name: f.server.Name + "_2", NodeID: f.node.ID, OwnerID: f.server.OwnerID,
		GameImage: "img", Port: 25601, Memory: 2048, Status: "stopped", ServerType: "game", RAMPaddingMB: p(1000),
	}
	sid, err := st.CreateServer(second)
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	t.Cleanup(func() { st.DeleteServer(int(sid)) })
	if sum, _, _ := st.SumAllocatedByNode(f.node.ID); sum != 1024+512+2048+1000 {
		t.Errorf("with a second server: allocated %d, want %d", sum, 1024+512+2048+1000)
	}
}
