package database

import (
	"testing"

	"dylaris-core/models"
)

// The scan finishes only while it is still owed: a reinstall that reset the
// state to pending, or cleared it, while the scan ran must not be overwritten
// with the result of hashing the old mods/.
func TestIntegrationModpackScanCompletesOnlyWhilePending(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	sub := "scan-sub"
	if err := st.UpsertSubServerInstall(models.SubServerInstall{ServerID: f.server.ID, SubServerName: sub, InstallerType: "technic"}); err != nil {
		t.Fatalf("UpsertSubServerInstall: %v", err)
	}
	t.Cleanup(func() {
		st.DeleteSubServerInstall(f.server.ID, sub)
		st.ReplaceServerModpackContents(f.server.ID, sub, nil)
	})
	rows := []models.ServerModpackContent{{ModrinthProjectID: "P1", ModrinthVersionID: "v1", FileName: "a.jar", Side: models.SideBoth}}

	if scan, jars, _, err := st.GetModpackScan(f.server.ID, "no-such-sub"); err != nil || scan != "" || jars != 0 {
		t.Fatalf("missing row = %q %d %v, want empty", scan, jars, err)
	}
	if ok, err := st.CompleteModpackScan(f.server.ID, sub, 0, rows, 4); err != nil || ok {
		t.Fatalf("complete on an unscheduled row = %v %v, want refused", ok, err)
	}
	if err := st.SetModpackScan(f.server.ID, sub, "pending", 0); err != nil {
		t.Fatalf("SetModpackScan: %v", err)
	}
	_, _, gen, err := st.GetModpackScan(f.server.ID, sub)
	if err != nil {
		t.Fatalf("GetModpackScan: %v", err)
	}
	// A reinstall owes a new scan while this one runs: pending again, new gen.
	if err := st.SetModpackScan(f.server.ID, sub, "pending", 0); err != nil {
		t.Fatalf("SetModpackScan again: %v", err)
	}
	if ok, err := st.CompleteModpackScan(f.server.ID, sub, gen, rows, 4); err != nil || ok {
		t.Fatalf("complete of a superseded scan = %v %v, want refused", ok, err)
	}
	_, _, gen, _ = st.GetModpackScan(f.server.ID, sub)
	if ok, err := st.CompleteModpackScan(f.server.ID, sub, gen, rows, 4); err != nil || !ok {
		t.Fatalf("complete while pending = %v %v, want stored", ok, err)
	}
	scan, jars, _, err := st.GetModpackScan(f.server.ID, sub)
	if err != nil || scan != "done" || jars != 4 {
		t.Fatalf("after complete = %q %d %v, want done 4", scan, jars, err)
	}
	if got, err := st.ListServerModpackContents(f.server.ID, sub); err != nil || len(got) != 1 || got[0].FileName != "a.jar" {
		t.Fatalf("contents = %+v %v, want the one row", got, err)
	}
	if ok, err := st.CompleteModpackScan(f.server.ID, sub, gen, nil, 0); err != nil || ok {
		t.Fatalf("second complete = %v %v, want refused", ok, err)
	}
	if got, _ := st.ListServerModpackContents(f.server.ID, sub); len(got) != 1 {
		t.Fatalf("refused complete changed the contents: %+v", got)
	}
}
