package database

import (
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

// A selection is written as JSONB and read back on every run. Against a real
// Postgres because the jsonb cast, the nullable storage reference and the
// ownership join are all SQL - a fake would prove that the fake round-trips.
func TestIntegrationPlatformBackupJobRoundTrip(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)

	owner := "11111111-1111-1111-1111-111111111111"
	job := &models.PlatformBackupJob{
		Name:           "nightly",
		Schedule:       "every 24h",
		RetentionCount: 7,
		Enabled:        true,
		Selection: models.PlatformBackupSelection{
			Database: true,
			Library:  true,
			Servers: models.PlatformBackupServers{
				Mode:      models.PlatformBackupServersOwner,
				OwnerID:   &owner,
				ServerIDs: []int{3, 9},
			},
		},
	}
	id, err := st.CreatePlatformBackupJob(job)
	if err != nil {
		t.Fatalf("CreatePlatformBackupJob: %v", err)
	}

	got, err := st.GetPlatformBackupJob(id)
	if err != nil {
		t.Fatalf("GetPlatformBackupJob: %v", err)
	}
	if !got.Selection.Database || !got.Selection.Library || got.Selection.Modpacks {
		t.Errorf("components = %+v", got.Selection)
	}
	if got.Selection.Servers.Mode != models.PlatformBackupServersOwner {
		t.Errorf("mode = %q", got.Selection.Servers.Mode)
	}
	if got.Selection.Servers.OwnerID == nil || *got.Selection.Servers.OwnerID != owner {
		t.Errorf("owner = %v", got.Selection.Servers.OwnerID)
	}
	if len(got.Selection.Servers.ServerIDs) != 2 {
		t.Errorf("serverIds = %v", got.Selection.Servers.ServerIDs)
	}
	// A job that has never run must say so, not report the zero time as a run.
	if got.LastRunAt != nil || got.NextRunAt != nil {
		t.Errorf("a job that never ran reports last=%v next=%v", got.LastRunAt, got.NextRunAt)
	}
	if got.StorageID != nil {
		t.Errorf("storage = %v, want none", got.StorageID)
	}

	next := time.Now().Add(24 * time.Hour).UTC()
	if err := st.SetPlatformBackupJobSchedule(id, &next); err != nil {
		t.Fatalf("SetPlatformBackupJobSchedule: %v", err)
	}
	got, _ = st.GetPlatformBackupJob(id)
	if got.LastRunAt == nil || got.NextRunAt == nil {
		t.Fatal("the schedule did not stick")
	}

	// A manual job runs and is not scheduled again; NULL is how that is said.
	if err := st.SetPlatformBackupJobSchedule(id, nil); err != nil {
		t.Fatalf("clearing the schedule: %v", err)
	}
	got, _ = st.GetPlatformBackupJob(id)
	if got.NextRunAt != nil {
		t.Errorf("next = %v, want none", got.NextRunAt)
	}
	if got.LastRunAt == nil {
		t.Error("clearing the next run also cleared the last one")
	}
}

// BYON is decided by the NODE's owner, not the server's. Getting this backwards
// would make "all BYON servers" mean "all servers of customers", which is a
// different and much larger set on a hosted platform.
func TestIntegrationBackupTargetServersReadOwnershipFromTheNode(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)
	f := newFixture(t, st)

	// A second node, owned by the same customer who owns the servers.
	byonNode := &models.Node{Name: uniqueName("byon_"), Address: "127.0.0.2", Token: uniqueName("bt_"), Status: "online"}
	if err := st.CreateNode(byonNode); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	t.Cleanup(func() { st.DeleteNode(byonNode.ID) })
	if err := st.SetNodeOwner(byonNode.ID, &f.user.ID); err != nil {
		t.Fatalf("SetNodeOwner: %v", err)
	}

	onCustomerHardware := &models.Server{
		UUID: uniqueName("uuid_byon_"), Name: uniqueName("s_byon_"),
		NodeID: byonNode.ID, OwnerID: f.user.ID,
		GameImage: "img", Port: 25601, Memory: 1024, Status: "stopped", ServerType: "game",
	}
	sid, err := st.CreateServer(onCustomerHardware)
	if err != nil {
		t.Fatalf("CreateServer: %v", err)
	}
	onCustomerHardware.ID = int(sid)
	t.Cleanup(func() { st.DeleteServer(onCustomerHardware.ID) })

	all, err := st.ListBackupTargetServers()
	if err != nil {
		t.Fatalf("ListBackupTargetServers: %v", err)
	}

	var platform, byon *models.BackupTargetServer
	for i := range all {
		switch all[i].ID {
		case f.server.ID:
			platform = &all[i]
		case onCustomerHardware.ID:
			byon = &all[i]
		}
	}
	if platform == nil || byon == nil {
		t.Fatalf("expected both servers, got %d rows", len(all))
	}
	// Both servers belong to the SAME user. Only the node differs.
	if platform.OwnerID != byon.OwnerID {
		t.Fatalf("the fixture no longer gives both servers the same owner")
	}
	if platform.BYON {
		t.Error("a server on a platform node was reported as BYON")
	}
	if !byon.BYON {
		t.Error("a server on a customer-owned node was not reported as BYON")
	}
	if byon.UUID == "" || byon.Name == "" {
		t.Error("a target with no identity cannot be shown in a selection list")
	}
}

func TestIntegrationPlatformBackupRunRecordsWhatItSkipped(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)

	jobID, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{
		Name: "manual", Schedule: "manual", RetentionCount: 3, Enabled: true,
		Selection: models.PlatformBackupSelection{Database: true},
	})
	if err != nil {
		t.Fatalf("CreatePlatformBackupJob: %v", err)
	}

	runID, err := st.CreatePlatformBackupRun(jobID, nil)
	if err != nil {
		t.Fatalf("CreatePlatformBackupRun: %v", err)
	}
	open, err := st.GetPlatformBackupRun(runID)
	if err != nil {
		t.Fatalf("GetPlatformBackupRun: %v", err)
	}
	if open.Status != "running" || open.CompletedAt != nil {
		t.Errorf("a new run reads as %+v", open)
	}

	components := []models.PlatformBackupComponent{
		{Kind: "database", Status: models.PlatformBackupIncluded, SizeBytes: 12345},
		{Kind: "server", Ref: "404", Status: models.PlatformBackupSkipped, Message: "the selected server no longer exists"},
	}
	if err := st.FinishPlatformBackupRun(runID, "success", 12345, "platform/1.bundle", "", components); err != nil {
		t.Fatalf("FinishPlatformBackupRun: %v", err)
	}

	done, err := st.GetPlatformBackupRun(runID)
	if err != nil {
		t.Fatalf("GetPlatformBackupRun: %v", err)
	}
	// A skipped server does not make the run a failure, so the status alone
	// cannot carry that information - the component list has to.
	if done.Status != "success" {
		t.Errorf("status = %q; a skipped server is not a failed run", done.Status)
	}
	if len(done.Components) != 2 {
		t.Fatalf("components = %+v", done.Components)
	}
	if done.Components[1].Status != models.PlatformBackupSkipped || done.Components[1].Ref != "404" {
		t.Errorf("the skipped server was not recorded: %+v", done.Components[1])
	}
	if done.CompletedAt == nil || done.SizeBytes != 12345 {
		t.Errorf("run = %+v", done)
	}
}

// Retention counts SUCCESSFUL runs only. Counting every row would delete a good
// archive to make room for a failed one, which is exactly backwards.
func TestIntegrationPlatformBackupRetentionIgnoresFailedRuns(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)

	jobID, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{
		Name: "nightly", Schedule: "manual", RetentionCount: 2, Enabled: true,
		Selection: models.PlatformBackupSelection{Database: true},
	})
	if err != nil {
		t.Fatalf("CreatePlatformBackupJob: %v", err)
	}

	var runs []int
	for i := 0; i < 5; i++ {
		id, err := st.CreatePlatformBackupRun(jobID, nil)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		status := "success"
		if i == 3 {
			status = "failed"
		}
		if err := st.FinishPlatformBackupRun(id, status, 1, "k", "", nil); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
		runs = append(runs, id)
	}

	over, err := st.ListPlatformBackupRunsOverRetention(jobID, 2)
	if err != nil {
		t.Fatalf("ListPlatformBackupRunsOverRetention: %v", err)
	}
	// Four successful runs, keep two: the two oldest successful ones go. The
	// failed one is not in the count and not in the answer.
	if len(over) != 2 {
		t.Fatalf("got %d prunable runs, want 2: %+v", len(over), over)
	}
	for _, r := range over {
		if r.Status != "success" {
			t.Errorf("a %s run was offered for pruning", r.Status)
		}
		if r.ID == runs[3] {
			t.Error("the failed run was offered for pruning")
		}
	}
	// And the newest two survive.
	if over[0].ID == runs[4] || over[1].ID == runs[4] {
		t.Error("the newest run was offered for pruning")
	}
}

func TestIntegrationPlatformBackupJobDeleteTakesItsRuns(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)

	jobID, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{
		Name: "throwaway", Schedule: "manual", Enabled: true,
		Selection: models.PlatformBackupSelection{Database: true},
	})
	if err != nil {
		t.Fatalf("CreatePlatformBackupJob: %v", err)
	}
	runID, err := st.CreatePlatformBackupRun(jobID, nil)
	if err != nil {
		t.Fatalf("CreatePlatformBackupRun: %v", err)
	}
	if err := st.DeletePlatformBackupJob(jobID); err != nil {
		t.Fatalf("DeletePlatformBackupJob: %v", err)
	}
	// Rows pointing at a job that is gone would be listed by nothing and
	// cleaned up by nobody.
	if _, err := st.GetPlatformBackupRun(runID); err == nil {
		t.Error("the run outlived its job")
	}
}

// An unreadable selection must select NOTHING. The other direction - reading a
// broken configuration as "everything" - would archive the whole platform on
// the strength of a parse failure.
func TestIntegrationAnUnreadableSelectionSelectsNothing(t *testing.T) {
	db := freshSchemaDB(t)
	st := store.NewPostgresStore(db)

	jobID, err := st.CreatePlatformBackupJob(&models.PlatformBackupJob{
		Name: "corrupt", Schedule: "manual", Enabled: true,
		Selection: models.PlatformBackupSelection{Database: true, Servers: models.PlatformBackupServers{Mode: models.PlatformBackupServersAll}},
	})
	if err != nil {
		t.Fatalf("CreatePlatformBackupJob: %v", err)
	}
	if _, err := db.Exec(`UPDATE platform_backup_jobs SET selection = '"not an object"'::jsonb WHERE id = $1`, jobID); err != nil {
		t.Fatalf("corrupting the selection: %v", err)
	}

	got, err := st.GetPlatformBackupJob(jobID)
	if err != nil {
		t.Fatalf("GetPlatformBackupJob: %v", err)
	}
	if got.Selection.Database || got.Selection.Library || got.Selection.Modpacks || got.Selection.MetricsDB {
		t.Errorf("an unreadable selection reads as %+v", got.Selection)
	}
	if got.Selection.Servers.Mode == models.PlatformBackupServersAll {
		t.Error("an unreadable selection still claims every server")
	}
	if err := got.Selection.Validate(); err == nil {
		t.Error("an unreadable selection passed validation instead of refusing the run")
	}
}
