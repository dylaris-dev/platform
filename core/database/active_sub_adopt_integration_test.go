package database

import (
	"testing"

	"dylaris-core/models"
)

// R36-03: Core follows the node onto a sub-server only while that sub-server's
// install record exists, so a delete that removed it first is never adopted.
func TestIntegrationAdoptActiveSubServerNeedsTheInstallRecord(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	if err := st.UpdateServerActiveSubServer(f.server.ID, "modded"); err != nil {
		t.Fatalf("UpdateServerActiveSubServer: %v", err)
	}
	t.Cleanup(func() { st.DeleteSubServerInstall(f.server.ID, "main") })

	if ok, err := st.AdoptServerActiveSubServer(f.server.ID, "modded", "main", "paper", "1.21.4", "100"); err != nil || ok {
		t.Fatalf("adopt without a record = %v %v, want refused", ok, err)
	}
	if err := st.UpsertSubServerInstall(models.SubServerInstall{ServerID: f.server.ID, SubServerName: "main", InstallerType: "paper"}); err != nil {
		t.Fatalf("UpsertSubServerInstall: %v", err)
	}
	if ok, err := st.AdoptServerActiveSubServer(f.server.ID, "other", "main", "paper", "1.21.4", "100"); err != nil || ok {
		t.Fatalf("adopt from a stale name = %v %v, want refused", ok, err)
	}
	if ok, err := st.AdoptServerActiveSubServer(f.server.ID, "modded", "main", "paper", "1.21.4", "100"); err != nil || !ok {
		t.Fatalf("adopt with the record = %v %v, want done", ok, err)
	}
	srv, err := st.GetServerByID(f.server.ID)
	if err != nil || srv.ActiveSubServer != "main" || srv.InstallerType != "paper" || srv.MinecraftVersion != "1.21.4" {
		t.Fatalf("after adopt = %+v %v", srv, err)
	}
}
