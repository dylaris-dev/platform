package database

import (
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"
)

// Installing a mod is queued work, and the row that describes it is keyed on
// the PROJECT. Both facts together produced the bug this covers: the upsert
// rewrote file_name onto the new jar's name, so the name of the jar the old
// version left in mods/ was destroyed by the very write that needed it, and the
// leftover became unnameable - the panel could no longer remove it, and the
// server loaded both builds.
//
// Against a real Postgres because the upsert, the history trim and the
// install_id match are all SQL. Skipped without DYLARIS_TEST_DB_HOST.
func TestIntegrationTheReplacedVersionSurvivesTheUpsert(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	first := &models.ServerMod{
		ServerID: f.server.ID, SubServerName: "survival",
		ModrinthProjectID: "spark", ModrinthVersionID: "v1",
		Title: "Spark", FileName: "spark-1.0.jar", TargetDir: "mods",
		Status: models.ServerModInstalled, InstallID: "a1",
	}
	if _, err := st.UpsertServerMod(first); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// What the install path does, in its order: read the row it is about to
	// overwrite, file it, then overwrite.
	prev, err := st.GetServerModByProject(f.server.ID, "survival", "spark")
	if err != nil || prev == nil {
		t.Fatalf("the row to be replaced could not be read: %v", err)
	}
	if prev.FileName != "spark-1.0.jar" {
		t.Fatalf("read back %q, want spark-1.0.jar", prev.FileName)
	}
	if err := st.RecordServerModHistory(f.server.ID, "survival", prev); err != nil {
		t.Fatalf("RecordServerModHistory: %v", err)
	}
	second := *first
	second.ModrinthVersionID = "v2"
	second.FileName = "spark-1.1.jar"
	second.Status = models.ServerModInstalling
	second.InstallID = "a2"
	if _, err := st.UpsertServerMod(&second); err != nil {
		t.Fatalf("second install: %v", err)
	}

	hist, err := st.ListServerModHistory(f.server.ID, "survival")
	if err != nil {
		t.Fatalf("ListServerModHistory: %v", err)
	}
	if len(hist) != 1 || hist[0].FileName != "spark-1.0.jar" || hist[0].ModrinthVersionID != "v1" {
		t.Fatalf("history = %+v; the file name the update replaced is the one the node needs to delete", hist)
	}
}

// Three, and the OLDEST goes. Kept per project rather than per server, so a
// second mod does not push the first one's history out.
func TestIntegrationHistoryKeepsThreePerProject(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	file := func(project string, n int) *models.ServerMod {
		return &models.ServerMod{
			ServerID: f.server.ID, SubServerName: "survival",
			ModrinthProjectID: project,
			ModrinthVersionID: string(rune('a' + n)),
			FileName:          project + string(rune('0'+n)) + ".jar",
			TargetDir:         "mods", Status: models.ServerModInstalled,
		}
	}
	for n := 0; n < 5; n++ {
		if err := st.RecordServerModHistory(f.server.ID, "survival", file("spark", n)); err != nil {
			t.Fatalf("spark %d: %v", n, err)
		}
	}
	if err := st.RecordServerModHistory(f.server.ID, "survival", file("lithium", 0)); err != nil {
		t.Fatalf("lithium: %v", err)
	}

	hist, err := st.ListServerModHistory(f.server.ID, "survival")
	if err != nil {
		t.Fatalf("ListServerModHistory: %v", err)
	}
	var spark, lithium int
	for _, h := range hist {
		switch h.ModrinthProjectID {
		case "spark":
			spark++
		case "lithium":
			lithium++
		}
	}
	if spark != 3 {
		t.Errorf("kept %d spark entries, want 3", spark)
	}
	if lithium != 1 {
		t.Errorf("kept %d lithium entries, want 1; the trim is per project, not per server", lithium)
	}
	// Newest first, and the two oldest builds are the ones dropped.
	for _, h := range hist {
		if h.ModrinthProjectID == "spark" && (h.FileName == "spark0.jar" || h.FileName == "spark1.jar") {
			t.Errorf("kept the oldest entry %q instead of the newest three", h.FileName)
		}
	}
}

// A node reporting about an attempt the row has moved past must not overwrite
// the state of the one that replaced it. Two clicks in a row is all that takes,
// and the losing report is the one that arrives late.
func TestIntegrationALateReportCannotOverwriteANewerAttempt(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	row := &models.ServerMod{
		ServerID: f.server.ID, SubServerName: "survival",
		ModrinthProjectID: "spark", ModrinthVersionID: "v2",
		FileName: "spark-1.1.jar", TargetDir: "mods",
		Status: models.ServerModInstalling, InstallID: "attempt-2",
	}
	if _, err := st.UpsertServerMod(row); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	applied, err := st.SetServerModStatus(f.server.ID, "survival", "spark", "attempt-1",
		models.ServerModFailed, "download failed")
	if err != nil {
		t.Fatalf("SetServerModStatus: %v", err)
	}
	if applied {
		t.Error("a report about attempt-1 was applied to a row holding attempt-2")
	}
	got, err := st.GetServerModByProject(f.server.ID, "survival", "spark")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Status != models.ServerModInstalling {
		t.Errorf("status = %q, want installing: the late answer decided the newer attempt", got.Status)
	}

	applied, err = st.SetServerModStatus(f.server.ID, "survival", "spark", "attempt-2",
		models.ServerModInstalled, "")
	if err != nil || !applied {
		t.Fatalf("the current attempt's own report was refused: applied=%v err=%v", applied, err)
	}
	got, _ = st.GetServerModByProject(f.server.ID, "survival", "spark")
	if got.Status != models.ServerModInstalled {
		t.Errorf("status = %q, want installed", got.Status)
	}
}

// Rows that predate the reporting must not read as pending forever.
func TestIntegrationRowsWrittenBeforeStatusExistedReadAsInstalled(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)

	// Written the way the old code wrote it: no status column value at all.
	if _, err := db.Exec(`INSERT INTO server_mods
		(server_id, sub_server_name, modrinth_project_id, modrinth_version_id, title, file_name, target_dir)
		VALUES ($1,'survival','old','v1','Old','old.jar','mods')`, f.server.ID); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := st.GetServerModByProject(f.server.ID, "survival", "old")
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.Status != models.ServerModInstalled {
		t.Errorf("status = %q, want installed: an existing install must not turn into a pending one", got.Status)
	}
}

var _ = store.PostgresStore{}
