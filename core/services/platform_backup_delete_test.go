package services

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"dylaris-core/models"
)

type fakePlatformJobDeleteStore struct {
	runs       []models.PlatformBackupRun
	jobDeleted bool
	log        []string
}

func (f *fakePlatformJobDeleteStore) ListPlatformBackupRuns(_, _ int) ([]models.PlatformBackupRun, error) {
	return append([]models.PlatformBackupRun(nil), f.runs...), nil
}

func (f *fakePlatformJobDeleteStore) DeletePlatformBackupRun(id int) error {
	for i, r := range f.runs {
		if r.ID == id {
			f.runs = append(f.runs[:i], f.runs[i+1:]...)
			break
		}
	}
	f.log = append(f.log, "row "+strconv.Itoa(id))
	return nil
}

func (f *fakePlatformJobDeleteStore) DeletePlatformBackupJob(int) error {
	f.jobDeleted = true
	f.log = append(f.log, "job")
	return nil
}

func TestDeletePlatformBackupJobRemovesBundlesFirst(t *testing.T) {
	st := &fakePlatformJobDeleteStore{runs: []models.PlatformBackupRun{
		{ID: 1, StorageKey: "platform-backups/9/a.dylaris-bundle"},
		{ID: 2, StorageKey: ""}, // a failed run that wrote nothing
		{ID: 3, StorageKey: "platform-backups/9/b.dylaris-bundle"},
	}}
	del := func(_ context.Context, run *models.PlatformBackupRun) error {
		st.log = append(st.log, "object "+run.StorageKey)
		return nil
	}
	if err := DeletePlatformBackupJob(context.Background(), st, 9, del); err != nil {
		t.Fatalf("DeletePlatformBackupJob: %v", err)
	}
	want := []string{
		"object platform-backups/9/a.dylaris-bundle", "row 1",
		"row 2",
		"object platform-backups/9/b.dylaris-bundle", "row 3",
		"job",
	}
	if strings.Join(st.log, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v\nwant    %v", st.log, want)
	}
}

func TestDeletePlatformBackupJobKeepsWhatItCouldNotDelete(t *testing.T) {
	st := &fakePlatformJobDeleteStore{runs: []models.PlatformBackupRun{
		{ID: 1, StorageKey: "platform-backups/9/a.dylaris-bundle"},
		{ID: 2, StorageKey: "platform-backups/9/b.dylaris-bundle"},
	}}
	del := func(_ context.Context, run *models.PlatformBackupRun) error {
		if run.ID == 2 {
			return errors.New("bucket said no")
		}
		return nil
	}
	err := DeletePlatformBackupJob(context.Background(), st, 9, del)
	if err == nil {
		t.Fatal("a bundle that could not be deleted was reported as success")
	}
	if st.jobDeleted {
		t.Fatal("the job was deleted although its bundle is still in the bucket; its row was the last record of it")
	}
	if len(st.runs) != 1 || st.runs[0].ID != 2 {
		t.Fatalf("remaining runs = %+v, want only run 2", st.runs)
	}
}

func TestDeletePlatformBackupJobRefusesWhileRunning(t *testing.T) {
	st := &fakePlatformJobDeleteStore{runs: []models.PlatformBackupRun{
		{ID: 2, Status: "running"},
		{ID: 1, Status: "success", StorageKey: "platform-backups/9/a.dylaris-bundle"},
	}}
	called := false
	del := func(context.Context, *models.PlatformBackupRun) error { called = true; return nil }
	if err := DeletePlatformBackupJob(context.Background(), st, 9, del); !errors.Is(err, ErrPlatformBackupRunning) {
		t.Fatalf("err = %v, want ErrPlatformBackupRunning", err)
	}
	if called || st.jobDeleted || len(st.runs) != 2 {
		t.Fatal("a refused delete still removed something")
	}
}
