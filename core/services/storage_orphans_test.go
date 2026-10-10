package services

import (
	"context"
	"fmt"
	"io/fs"
	"testing"
	"time"

	backupstorage "dylaris-core/storage/backup"
)

const orphanUUID = "0f8fad5b-d9cb-469f-a165-70867728950e"

func TestClassifyOrphanKey(t *testing.T) {
	cases := []struct {
		key    string
		kind   string
		uuid   string
		job    int
		mine   bool
		reason string
	}{
		{"backups/" + orphanUUID + "/job-12/20261001-020304-deadbeef.tar.gz", OrphanServerBackup, orphanUUID, 12, true, "current server backup"},
		{"backups/" + orphanUUID + "/job-3/20250101-000000.tar.gz", OrphanServerBackup, orphanUUID, 3, true, "pre-suffix server backup"},
		{"platform-backups/7/20261001-020304-bundle-123.tmp.dylaris-bundle", OrphanPlatformBackup, "", 7, true, "platform bundle"},
		{"migration-transfer/" + orphanUUID + "-1696000000.zip", OrphanMigrationTransfer, orphanUUID, 0, true, "migration transfer"},
		{"__dylaris_probe_0a1b2c.txt", OrphanProbe, "", 0, true, "probe"},
		{"__dylaris_probe_0a1b2c.multipart", OrphanProbe, "", 0, true, "multipart probe"},
		{"modpacks/abc/pack.mrpack", "", "", 0, false, "modpack"},
		{"ticket-attachments/tickets/1/x-y", "", "", 0, false, "ticket attachment"},
		{"library/server.jar", "", "", 0, false, "library"},
		{"server-backups/backups/" + orphanUUID + "/job-1/20261001-020304.tar.gz", "", "", 0, false, "another storage's folder in a shared bucket"},
		{"backups/" + orphanUUID + "/job-1/notes.txt", "", "", 0, false, "foreign file inside our folder"},
		{"backups/" + orphanUUID + "/job-1/20261001-020304.tar.gz.part", "", "", 0, false, "suffix after the extension"},
	}
	for _, tc := range cases {
		kind, uuid, job, ok := ClassifyOrphanKey(tc.key)
		if ok != tc.mine || kind != tc.kind || uuid != tc.uuid || job != tc.job {
			t.Errorf("%s: ClassifyOrphanKey(%q) = %q %q %d %v, want %q %q %d %v",
				tc.reason, tc.key, kind, uuid, job, ok, tc.kind, tc.uuid, tc.job, tc.mine)
		}
	}
}

func TestClassifyOrphans(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	ours := func(n int) string {
		return fmt.Sprintf("backups/%s/job-1/20261001-02030%d.tar.gz", orphanUUID, n)
	}
	objs := []backupstorage.Object{
		{Key: ours(1), Size: 10, LastModified: old},                           // orphan
		{Key: ours(2), Size: 20, LastModified: old},                           // referenced
		{Key: ours(3), Size: 30, LastModified: now.Add(-time.Hour)},           // too young
		{Key: ours(4), Size: 40},                                              // age unknown
		{Key: ours(5), Size: 50, LastModified: now.Add(-OrphanMinAge)},        // exactly 24h: old enough
		{Key: "modpacks/x/pack.mrpack", Size: 100, LastModified: old},         // unclassified
		{Key: "ticket-attachments/tickets/1/a", Size: 200, LastModified: old}, // unclassified
	}
	scan := ClassifyOrphans(objs, map[string]bool{ours(2): true}, now)

	if len(scan.Candidates) != 2 || scan.Candidates[0].Key != ours(1) || scan.Candidates[1].Key != ours(5) {
		t.Fatalf("candidates = %+v, want %s and %s", scan.Candidates, ours(1), ours(5))
	}
	if scan.Candidates[0].Kind != OrphanServerBackup || scan.Candidates[0].ServerUUID != orphanUUID || scan.Candidates[0].JobID != 1 {
		t.Errorf("candidate details = %+v", scan.Candidates[0])
	}
	checks := []struct {
		name      string
		got, want int64
	}{
		{"candidate bytes", scan.CandidateBytes, 60},
		{"referenced", int64(scan.ReferencedCount), 1},
		{"referenced bytes", scan.ReferencedBytes, 20},
		{"recent (young + unknown age)", int64(scan.RecentCount), 2},
		{"recent bytes", scan.RecentBytes, 70},
		{"unclassified", int64(scan.UnclassifiedCount), 2},
		{"unclassified bytes", scan.UnclassifiedBytes, 300},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// fakeOrphanStore is an in-memory bucket.
type fakeOrphanStore struct {
	objs    map[string]backupstorage.Object
	deleted []string
}

func (f *fakeOrphanStore) List(_ context.Context, _ string) ([]backupstorage.Object, error) {
	out := make([]backupstorage.Object, 0, len(f.objs))
	for _, o := range f.objs {
		out = append(out, o)
	}
	return out, nil
}

func (f *fakeOrphanStore) Stat(_ context.Context, key string) (backupstorage.Object, error) {
	o, ok := f.objs[key]
	if !ok {
		return backupstorage.Object{}, fs.ErrNotExist
	}
	return o, nil
}

func (f *fakeOrphanStore) Delete(_ context.Context, key string) error {
	delete(f.objs, key)
	f.deleted = append(f.deleted, key)
	return nil
}

// The scan is a snapshot; the delete must not act on it. A key that a backup
// started naming after the scan, or that was rewritten since, stays.
func TestDeleteOrphansRechecksEveryKey(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-72 * time.Hour)
	k := func(n int) string { return fmt.Sprintf("backups/%s/job-1/20261001-02030%d.tar.gz", orphanUUID, n) }
	st := &fakeOrphanStore{objs: map[string]backupstorage.Object{
		k(1):                     {Key: k(1), Size: 1, LastModified: old},
		k(2):                     {Key: k(2), Size: 2, LastModified: old},
		k(3):                     {Key: k(3), Size: 3, LastModified: old},
		"modpacks/x/pack.mrpack": {Key: "modpacks/x/pack.mrpack", Size: 9, LastModified: old},
	}}
	referenced := map[string]bool{}
	refs := func(context.Context) (map[string]bool, error) { return referenced, nil }

	scan, err := ScanOrphans(context.Background(), st, refs, OrphanLookups{}, now)
	if err != nil || len(scan.Candidates) != 3 {
		t.Fatalf("scan = %+v, %v; want three candidates", scan, err)
	}

	// Between the scan and the delete: a run starts naming k(2), and k(3) is
	// rewritten.
	referenced[k(2)] = true
	st.objs[k(3)] = backupstorage.Object{Key: k(3), Size: 3, LastModified: now.Add(-time.Minute)}

	res, err := DeleteOrphans(context.Background(), st, refs, OrphanLookups{},
		[]string{k(1), k(2), k(3), "modpacks/x/pack.mrpack", k(1), k(9)}, false, func() time.Time { return now })
	if err != nil {
		t.Fatalf("DeleteOrphans: %v", err)
	}
	want := map[string]bool{k(1): true, k(2): false, k(3): false, "modpacks/x/pack.mrpack": false, k(9): false}
	if len(res) != len(want) {
		t.Fatalf("got %d results, want %d (duplicates collapse): %+v", len(res), len(want), res)
	}
	for _, r := range res {
		if r.Deleted != want[r.Key] {
			t.Errorf("%s deleted=%v (%s), want %v", r.Key, r.Deleted, r.Reason, want[r.Key])
		}
		if !r.Deleted && r.Reason == "" {
			t.Errorf("%s was refused without a reason", r.Key)
		}
	}
	if len(st.deleted) != 1 || st.deleted[0] != k(1) {
		t.Fatalf("store saw deletes %v, want only %s", st.deleted, k(1))
	}

	if _, err := DeleteOrphans(context.Background(), st, refs, OrphanLookups{}, make([]string, MaxOrphanDeleteKeys+1), false, time.Now); err == nil {
		t.Fatal("a request over the key cap was accepted")
	}
}

// A file nothing names whose server and job are both still there is what a
// real backup looks like after a database restore. It gets its own list and
// is refused by the delete unless includeLive is set.
func TestLiveOrphansAreKeptApart(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-72 * time.Hour)
	const goneUUID = "11111111-2222-3333-4444-555555555555"
	live := fmt.Sprintf("backups/%s/job-1/20261001-020301.tar.gz", orphanUUID)    // server + job exist
	jobGone := fmt.Sprintf("backups/%s/job-2/20261001-020302.tar.gz", orphanUUID) // job deleted
	srvGone := fmt.Sprintf("backups/%s/job-1/20261001-020303.tar.gz", goneUUID)   // server deleted
	platLive := "platform-backups/5/20261001-020304-x.dylaris-bundle"             // platform job exists
	platGone := "platform-backups/6/20261001-020305-x.dylaris-bundle"
	objs := map[string]backupstorage.Object{}
	for _, k := range []string{live, jobGone, srvGone, platLive, platGone} {
		objs[k] = backupstorage.Object{Key: k, Size: 1, LastModified: old}
	}
	look := OrphanLookups{
		ServerExists:         func(uuid string) bool { return uuid == orphanUUID },
		BackupJobExists:      func(id int) bool { return id == 1 },
		PlatformBackupExists: func(id int) bool { return id == 5 },
	}
	refs := func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }

	st := &fakeOrphanStore{objs: objs}
	scan, err := ScanOrphans(context.Background(), st, refs, look, now)
	if err != nil {
		t.Fatal(err)
	}
	keys := func(cs []OrphanCandidate) map[string]bool {
		m := map[string]bool{}
		for _, c := range cs {
			m[c.Key] = true
		}
		return m
	}
	if l := keys(scan.Live); len(l) != 2 || !l[live] || !l[platLive] {
		t.Fatalf("live = %v, want %s and %s", l, live, platLive)
	}
	if c := keys(scan.Candidates); len(c) != 3 || !c[jobGone] || !c[srvGone] || !c[platGone] {
		t.Fatalf("candidates = %v", c)
	}
	if scan.LiveBytes != 2 || scan.CandidateBytes != 3 {
		t.Fatalf("bytes live=%d candidates=%d, want 2 and 3", scan.LiveBytes, scan.CandidateBytes)
	}

	clock := func() time.Time { return now }
	res, err := DeleteOrphans(context.Background(), st, refs, look, []string{live, platLive, jobGone}, false, clock)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if want := r.Key == jobGone; r.Deleted != want {
			t.Errorf("without includeLive: %s deleted=%v (%s), want %v", r.Key, r.Deleted, r.Reason, want)
		}
	}
	res, err = DeleteOrphans(context.Background(), st, refs, look, []string{live}, true, clock)
	if err != nil || len(res) != 1 || !res[0].Deleted {
		t.Fatalf("with includeLive the live file must go: %+v, %v", res, err)
	}
}

// Real server ids are "<uuid>_<random>" (validate.ServerUUID), not a bare
// 36-char UUID. Production 2026-10-10: the three real backups of server 32 were
// all "unclassified", so a real orphan of any such server could never be found.
const suffixedUUID = "f3e13e73-63fe-4d73-a3df-fa10dad2c981_mz26xsfr6r"

func TestOrphanKeysAcceptRealServerIDs(t *testing.T) {
	backup := "backups/" + suffixedUUID + "/job-15/20261001-020304-deadbeef.tar.gz"
	transfer := "migration-transfer/" + suffixedUUID + "-0a1b2c3d4e5f.zip"
	for key, want := range map[string]string{backup: OrphanServerBackup, transfer: OrphanMigrationTransfer} {
		kind, uuid, _, ok := ClassifyOrphanKey(key)
		if !ok || kind != want || uuid != suffixedUUID {
			t.Errorf("ClassifyOrphanKey(%q) = %q %q %v, want %q %q", key, kind, uuid, ok, want, suffixedUUID)
		}
	}

	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	orphan := "backups/" + suffixedUUID + "/job-15/20261002-020304-cafebabe.tar.gz"
	scan := ClassifyOrphans([]backupstorage.Object{
		{Key: backup, Size: 13, LastModified: old},
		{Key: orphan, Size: 7, LastModified: old},
	}, map[string]bool{backup: true}, now)
	if scan.ReferencedCount != 1 {
		t.Errorf("referenced = %d, want the referenced backup counted as referenced", scan.ReferencedCount)
	}
	if len(scan.Candidates) != 1 || scan.Candidates[0].Key != orphan || scan.Candidates[0].ServerUUID != suffixedUUID {
		t.Errorf("candidates = %+v, want only %s", scan.Candidates, orphan)
	}
	if scan.UnclassifiedCount != 0 {
		t.Errorf("unclassified = %d, want 0", scan.UnclassifiedCount)
	}
}

// Unclassified files are listed read-only, capped, so an operator can see WHAT
// is being left alone rather than only how much.
func TestUnclassifiedFilesAreListedAndCapped(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	objs := []backupstorage.Object{{Key: "modpacks/x/pack.mrpack", Size: 100, LastModified: now}}
	for i := 0; i < MaxUnclassifiedListed+5; i++ {
		objs = append(objs, backupstorage.Object{Key: fmt.Sprintf("library/%03d.jar", i), Size: 1})
	}
	scan := ClassifyOrphans(objs, nil, now)
	if scan.UnclassifiedCount != MaxUnclassifiedListed+6 {
		t.Fatalf("unclassified count = %d", scan.UnclassifiedCount)
	}
	if len(scan.Unclassified) != MaxUnclassifiedListed {
		t.Fatalf("listed %d unclassified, want the cap %d", len(scan.Unclassified), MaxUnclassifiedListed)
	}
	if u := scan.Unclassified[0]; u.Key != "modpacks/x/pack.mrpack" || u.Size != 100 || u.LastModified == nil || !u.LastModified.Equal(now) {
		t.Errorf("first unclassified = %+v", u)
	}
	if scan.Unclassified[1].LastModified != nil {
		t.Errorf("an unknown modification time reads as %v", scan.Unclassified[1].LastModified)
	}
	if empty := ClassifyOrphans(nil, nil, now); empty.Unclassified == nil {
		t.Error("an empty scan answers null instead of an empty list")
	}
}
