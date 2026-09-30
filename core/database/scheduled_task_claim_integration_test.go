package database

import (
	"testing"
	"time"

	"dylaris-core/models"
)

// Against a real Postgres, because the claim IS a query: it compares the
// next_run a replica read with the one in the row, and a timestamp that does not
// survive the round trip unchanged would make every claim lose - no task would
// ever fire again, and a fake store could not show it.
//
// Two Core replicas can list the same due row across a leader handover. Only the
// first claim may move next_run on; the second must lose.
//
// Skipped without DYLARIS_TEST_DB_HOST, like its neighbours.
func TestIntegrationScheduledTaskClaimIsWonOnce(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)

	// A value with sub-second precision, as time.Now() produces.
	due := time.Now().UTC().Add(-time.Minute)
	task := &models.ScheduledTask{
		ServerID: f.server.ID, Name: "claim", TaskType: "restart",
		ScheduleCron: "* * * * *", Enabled: true, NextRun: &due,
	}
	id, err := st.CreateScheduledTask(task)
	if err != nil {
		t.Fatalf("CreateScheduledTask: %v", err)
	}
	t.Cleanup(func() { st.DeleteScheduledTask(id) })

	listed, err := st.ListDueScheduledTasks(time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("ListDueScheduledTasks: %v", err)
	}
	var read *time.Time
	for i := range listed {
		if listed[i].ID == id {
			read = listed[i].NextRun
		}
	}
	if read == nil {
		t.Fatal("the due task was not listed")
	}

	next := time.Now().UTC().Add(time.Minute)
	won, err := st.ClaimScheduledTaskRun(id, *read, next)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !won {
		t.Fatal("the first claim lost: the next_run read back does not compare equal to the stored one, so no task could ever fire")
	}

	// The second replica read the same row before the first claim landed.
	won, err = st.ClaimScheduledTaskRun(id, *read, next.Add(time.Minute))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if won {
		t.Fatal("the second claim also won: both replicas would fire the task")
	}

	// A disabled task is not claimed even at its own next_run.
	if err := st.SetScheduledTaskEnabled(id, false, &next); err != nil {
		t.Fatalf("SetScheduledTaskEnabled: %v", err)
	}
	won, err = st.ClaimScheduledTaskRun(id, next, next.Add(time.Hour))
	if err != nil {
		t.Fatalf("claim on a disabled task: %v", err)
	}
	if won {
		t.Fatal("a disabled task was claimed")
	}
}
