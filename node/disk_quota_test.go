package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestParseRepquota(t *testing.T) {
	// `repquota -P -n -p` on ext4, quota-tools 4.09. Block columns are 1 KiB;
	// -p prints each grace as seconds since the epoch, 0 when none runs.
	const report = `*** Report for project quotas on device /dev/sdb1
Block grace time: 7days; Inode grace time: 7days
                        Block limits                File limits
Project         used    soft    hard  grace    used  soft  hard  grace
----------------------------------------------------------------------
#0        --      20       0       0      0       2     0     0      0
#3826675829 --  1048576       0 2097152      0    1200     0     0      0
#12345    --  512000       0       0      0      40     0     0      0
#77       +-  2048000 1024000 4096000 1764547200    300     0     0      0
#79       ++  2048000 1024000 4096000 1764547200    300   100   900 1764547200
#80       -+     1024       0   10240      0 1000000     0 1000000      0
#81       +-  21000000       0 20971520      0 3000000     0 1310720      0
#82       +-  21000000       0 20971520 3000000     0     0
#83       --     1024       0   10240     40     0     0
#84       --     1024       0   10240      0    abc     0     0      0

`
	for _, tc := range []struct {
		name               string
		pid                uint32
		used, limit        int64
		inodes, inodeLimit int64
	}{
		{"hard limit set", 3826675829, 1048576 * 1024, 2097152 * 1024, 1200, 0},
		{"no limit", 12345, 512000 * 1024, 0, 40, 0},
		{"over soft, grace running", 77, 2048000 * 1024, 4096000 * 1024, 300, 0},
		{"over soft, inode grace too", 79, 2048000 * 1024, 4096000 * 1024, 300, 900},
		{"at the inode hard limit", 80, 1024 * 1024, 10240 * 1024, 1000000, 1000000},
		// R61-02/05: '+' from the hard limit with no soft limit set.
		{"over the block hard limit", 81, 21000000 * 1024, 20971520 * 1024, 3000000, 1310720},
		// Without -p a grace not running is blank: which column holds the
		// files is a guess, so they are not known.
		{"blank grace, over the hard limit", 82, 21000000 * 1024, 20971520 * 1024, -1, 0},
		{"blank grace", 83, 1024 * 1024, 10240 * 1024, -1, 0},
		{"files unparsable", 84, 1024 * 1024, 10240 * 1024, -1, 0},
		{"not in the report", 4242, -1, 0, 0, 0},
		// "#1234" must not match a prefix of another id.
		{"id prefix of another", 1234, -1, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			used, limit, inodes, inodeLimit := parseRepquota(report, tc.pid)
			if used != tc.used || limit != tc.limit || inodes != tc.inodes || inodeLimit != tc.inodeLimit {
				t.Fatalf("got (%d, %d, %d, %d), want (%d, %d, %d, %d)", used, limit, inodes, inodeLimit,
					tc.used, tc.limit, tc.inodes, tc.inodeLimit)
			}
		})
	}
}

// `xfs_quota -x -c "report -p -N -b -i"`: blocks then inodes on one line, each
// with a warn count and a bracketed grace that can be one word or several.
func TestParseXFSReport(t *testing.T) {
	const report = `#0                  0          0          0     00 [--------]          3          0          0     00 [--------]
3a2b-uuid        2048          0    1048576     00 [--------]     999999          0    1000000     00 [--------]
#77           2048000    1024000    4096000     00  [7 days]        300        100        900     00 [6 days]
#78              1024          0      10240     00 [--------]
`
	for _, tc := range []struct {
		name               string
		uuid               string
		pid                uint32
		used, limit        int64
		inodes, inodeLimit int64
	}{
		{"by name", "3a2b-uuid", 5, 2048 * 1024, 1048576 * 1024, 999999, 1000000},
		{"grace of two words", "", 77, 2048000 * 1024, 4096000 * 1024, 300, 900},
		{"no inode columns", "", 78, 1024 * 1024, 10240 * 1024, -1, 0},
		{"not in the report", "", 4242, -1, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			used, limit, inodes, inodeLimit := parseXFSReport(report, tc.uuid, tc.pid)
			if used != tc.used || limit != tc.limit || inodes != tc.inodes || inodeLimit != tc.inodeLimit {
				t.Fatalf("got (%d, %d, %d, %d), want (%d, %d, %d, %d)", used, limit, inodes, inodeLimit,
					tc.used, tc.limit, tc.inodes, tc.inodeLimit)
			}
		})
	}
}

// R58-05: a server at its inode hard limit fails every file create, so it is
// full for the guard however many bytes are left.
func TestDiskUsageFullOnInodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    DiskUsagePayload
		pct  float64
		full bool
	}{
		{"bytes full", DiskUsagePayload{Total: 10, Limit: 10}, 1, true},
		{"inodes full, bytes free", DiskUsagePayload{Total: 1, Limit: 10, Inodes: 1000000, InodeLimit: 1000000}, 1, true},
		{"inodes at 90%", DiskUsagePayload{Total: 1, Limit: 10, Inodes: 900, InodeLimit: 1000}, 0.9, false},
		{"no inode limit", DiskUsagePayload{Total: 1, Limit: 10, Inodes: 5000}, 0.1, false},
		{"no limits", DiskUsagePayload{Total: 1, Inodes: 5}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.usedFraction(); got != tc.pct {
				t.Fatalf("usedFraction = %v, want %v", got, tc.pct)
			}
			if got := tc.u.atLimit(); got != tc.full {
				t.Fatalf("atLimit = %v, want %v", got, tc.full)
			}
		})
	}
}

// What `repquota -Ps` printed, and why the node now asks for neither: the
// name instead of the id, and human units. Nothing in it may parse as a usage.
func TestParseRepquotaRefusesHumanUnits(t *testing.T) {
	const report = `*** Report for project quotas on device /dev/sdb1
Block grace time: 7days; Inode grace time: 7days
                        Space limits                File limits
Project         used    soft    hard  grace    used  soft  hard  grace
----------------------------------------------------------------------
#12345    --   1024M      0K   2048M           1200     0     0
`
	if used, limit, _, _ := parseRepquota(report, 12345); used != -1 || limit != 0 {
		t.Fatalf("got (%d, %d), want (-1, 0)", used, limit)
	}
}

// S10: a mount point covers a path only up to a path-component boundary, so
// /mnt/data2 is not bound to the quota database of /mnt/data.
func TestMountCoversOnlyWholePathComponents(t *testing.T) {
	for _, tc := range []struct {
		mp, path string
		want     bool
	}{
		{"/", "/mnt/data2/srv", true},
		{"/mnt/data", "/mnt/data", true},
		{"/mnt/data", "/mnt/data/srv", true},
		{"/mnt/data", "/mnt/data2", false},
		{"/mnt/data", "/mnt/data2/srv", false},
		{"/mnt/data/", "/mnt/data/srv", true},
	} {
		if got := mountCovers(tc.mp, tc.path); got != tc.want {
			t.Errorf("mountCovers(%q, %q) = %v, want %v", tc.mp, tc.path, got, tc.want)
		}
	}
}

// R53-03: in separate-budget mode only the node's own archives come off the
// server; whatever a root tenant puts in the folder stays charged.
func TestBackupEntryProject(t *testing.T) {
	const pid = 7
	for _, tc := range []struct {
		name        string
		entry       string
		depth       int
		dir, linked bool
		share       bool
		want        uint32
	}{
		{"folder, separate", ".", 0, true, false, false, 0},
		{"archive", "20261125-010203-ab12cd34.tar.gz", 1, false, false, false, 0},
		{"partial", "20261125-010203-ab12cd34.tar.gz.partial", 1, false, false, false, 0},
		{"tenant file", "r.0.0.mca", 1, false, false, false, pid},
		{"hard-linked archive name", "x.tar.gz", 1, false, true, false, pid},
		{"archive name on a directory", "x.tar.gz", 1, true, false, false, pid},
		{"archive name below a sub-directory", "x.tar.gz", 2, false, false, false, pid},
		{"folder, share", ".", 0, true, false, true, pid},
		{"archive, share", "x.tar.gz", 1, false, false, true, pid},
	} {
		if got := backupEntryProject(tc.entry, tc.depth, tc.dir, tc.linked, pid, tc.share); got != tc.want {
			t.Errorf("%s: project %d, want %d", tc.name, got, tc.want)
		}
	}
}

// R53-03: on the du path the folder's other contents stay charged too.
func TestServerDiskBytesSubtractsOnlyArchives(t *testing.T) {
	dir := t.TempDir()
	backups := filepath.Join(dir, backupDirName)
	if err := os.MkdirAll(filepath.Join(backups, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{
		"world.dat":                         1000,
		backupDirName + "/a.tar.gz":         500,
		backupDirName + "/b.tar.gz.partial": 50,
		backupDirName + "/r.0.0.mca":        300,
		backupDirName + "/sub/c.tar.gz":     200,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	total := dirSize(dir)
	if got, want := serverDiskBytes(dir, false), total-550; got != want {
		t.Errorf("separate: %d, want %d (only the archives off)", got, want)
	}
	if got := serverDiskBytes(dir, true); got != total {
		t.Errorf("share: %d, want %d", got, total)
	}
}

func TestProjectInodeLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limitMB int64
		want    int64
	}{
		{"no disk limit, no inode limit", 0, 0},
		{"negative is no limit", -5, 0},
		{"small limit gets the floor", 1, 1000000},
		{"at the floor", 15625, 1000000},
		{"20 GB", 20480, 1310720},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectInodeLimit(tc.limitMB); got != tc.want {
				t.Errorf("projectInodeLimit(%d) = %d, want %d", tc.limitMB, got, tc.want)
			}
		})
	}
}

func TestProjectLimitCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fsType  string
		limitMB int64
		want    []string
	}{
		{"xfs set", "xfs", 20480, []string{"xfs_quota", "-x", "-c", "limit -p bhard=20971520k ihard=1310720 42", "/mnt"}},
		{"xfs lift clears both", "xfs", 0, []string{"xfs_quota", "-x", "-c", "limit -p bhard=0k ihard=0 42", "/mnt"}},
		{"ext4 set", "ext4", 20480, []string{"setquota", "-P", "42", "0", "20971520", "0", "1310720", "/mnt"}},
		{"ext4 lift clears both", "ext4", 0, []string{"setquota", "-P", "42", "0", "0", "0", "0", "/mnt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qp := &QuotaProvider{fsType: tc.fsType, mountPoint: "/mnt"}
			got, err := qp.projectLimitCommand(42, tc.limitMB, projectInodeLimit(tc.limitMB))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := (&QuotaProvider{fsType: "btrfs"}).projectLimitCommand(42, 1, 0); err == nil {
		t.Error("btrfs: want an error")
	}
}

// R59-03/06: a server already holding more files than its limit's formula
// allows keeps a quarter of headroom over what it holds, so applying the limit
// never stops it; under the formula the formula stands.
func TestProjectInodeLimitFor(t *testing.T) {
	for _, tc := range []struct {
		name            string
		limitMB, inodes int64
		want            int64
	}{
		{"under the formula", 10240, 400000, 1000000},
		{"at the formula", 10240, 1000000, 1000000},
		{"over the formula", 10240, 1500000, 1875000},
		{"over, rounded up", 10240, 1000001, 1250002},
		{"no disk limit", 0, 1500000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectInodeLimitFor(tc.limitMB, tc.inodes); got != tc.want {
				t.Errorf("projectInodeLimitFor(%d, %d) = %d, want %d", tc.limitMB, tc.inodes, got, tc.want)
			}
		})
	}
}

// fakeQuotaTools stands in for repquota and setquota: report is what the
// mount's report prints (reportErr makes it fail), and every limit command
// is recorded.
func fakeQuotaTools(t *testing.T, report string, reportErr error) (reports *int, limits *[]string) {
	t.Helper()
	oldReport, oldLimit := quotaReportOutput, quotaLimitOutput
	t.Cleanup(func() { quotaReportOutput, quotaLimitOutput = oldReport, oldLimit })
	reports, limits = new(int), new([]string)
	quotaReportOutput = func(...string) ([]byte, error) {
		*reports++
		return []byte(report), reportErr
	}
	quotaLimitOutput = func(args ...string) ([]byte, error) {
		*limits = append(*limits, strings.Join(args, " "))
		return nil, nil
	}
	return reports, limits
}

func repquotaLine(uuid string, usedKB, hardKB, files, filesHard int64) string {
	return fmt.Sprintf("#%d -- %d 0 %d 0 %d 0 %d 0\n", projectID(uuid), usedKB, hardKB, files, filesHard)
}

// inodeAnchorDir points the anchor files at a fresh directory for one test.
func inodeAnchorDir(t *testing.T) {
	t.Helper()
	prevNodeID, prevDir := nodeID, nodeSecretDir
	t.Cleanup(func() { nodeID, nodeSecretDir = prevNodeID, prevDir })
	nodeID, nodeSecretDir = "node-1", t.TempDir()
}

// R59-03/06: every SetLimit reads the files the project holds, so a limit is
// never put below them; a report that cannot be read sets no inode limit
// rather than one that may stop a running server.
func TestSetLimitNeverPutsTheInodeLimitBelowTheFiles(t *testing.T) {
	for _, tc := range []struct {
		name      string
		report    string
		reportErr error
		limitMB   int64
		want      string
	}{
		{"under the formula", repquotaLine("srv", 1024, 0, 5000, 0), nil, 10240, "0 10485760 0 1000000"},
		{"over the formula", repquotaLine("srv", 1024, 0, 1500000, 0), nil, 10240, "0 10485760 0 1875000"},
		{"not in the report yet", "", nil, 10240, "0 10485760 0 1000000"},
		{"report unreadable", "", errors.New("boom"), 10240, "0 10485760 0 0"},
		{"files unreadable", fmt.Sprintf("#%d +- 21000000 0 20971520 3000000 0 0\n", projectID("srv")), nil, 10240, "0 10485760 0 0"},
		{"lift", repquotaLine("srv", 1024, 0, 1500000, 0), nil, 0, "0 0 0 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inodeAnchorDir(t)
			_, limits := fakeQuotaTools(t, tc.report, tc.reportErr)
			qp := &QuotaProvider{fsType: "ext4", mountPoint: "/mnt", available: true, rdb: newDiskGuardRedis(t)}
			if err := qp.SetLimit("srv", tc.limitMB); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("setquota -P %d %s /mnt", projectID("srv"), tc.want)
			if len(*limits) != 1 || (*limits)[0] != want {
				t.Fatalf("limit commands %q, want [%q]", *limits, want)
			}
		})
	}
}

// R60-04/05: the file cap a server holds over its formula is granted once,
// when its project had no inode limit yet, and kept on disk. Later SetLimits
// (a lift's put-back, a resize, a restart of the node or of Valkey) never
// recompute it from the files held now, so files made while the limit was off
// do not raise it.
func TestInodeAnchorIsGrantedOnce(t *testing.T) {
	inodeAnchorDir(t)
	rdb := newDiskGuardRedis(t)
	const gb10 = 10240 * 1024
	for _, step := range []struct {
		name    string
		uuid    string
		report  string
		limitMB int64
		want    int64 // ihard set
	}{
		{"new server", "new", repquotaLine("new", 0, 0, 1, 0), 10240, 1000000},
		{"new server grows past the formula, put back", "new", repquotaLine("new", 1024, 0, 1500000, 0), 10240, 1000000},
		{"pre-inode server over the formula", "old", repquotaLine("old", 1024, gb10, 1500000, 0), 10240, 1875000},
		{"repeated SetLimit", "old", repquotaLine("old", 1024, gb10, 1800000, 1875000), 10240, 1875000},
		{"lift put-back after files made while off", "old", repquotaLine("old", 1024, 0, 2400000, 0), 10240, 1875000},
		{"larger disk limit: formula wins", "old", repquotaLine("old", 1024, gb10, 1800000, 1875000), 40960, 2621440},
		{"back down: anchor kept", "old", repquotaLine("old", 1024, 4*gb10, 1800000, 2621440), 10240, 1875000},
		{"restart: a new provider keeps the anchor", "old", repquotaLine("old", 1024, 0, 2400000, 0), 10240, 1875000},
		// R61-04: Valkey keeps nothing over a restart; the anchor is on disk.
		{"Valkey restart: the lift's put-back keeps it", "old", repquotaLine("old", 1024, 0, 2400000, 0), 10240, 1875000},
		{"Valkey restart: the new server keeps none", "new", repquotaLine("new", 1024, 0, 1500000, 0), 10240, 1000000},
	} {
		t.Run(step.name, func(t *testing.T) {
			_, limits := fakeQuotaTools(t, step.report, nil)
			if strings.HasPrefix(step.name, "Valkey restart") {
				rdb = newDiskGuardRedis(t)
			}
			qp := &QuotaProvider{fsType: "ext4", mountPoint: "/mnt", available: true, rdb: rdb}
			if err := qp.SetLimit(step.uuid, step.limitMB); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("setquota -P %d 0 %d 0 %d /mnt", projectID(step.uuid), step.limitMB*1024, step.want)
			if len(*limits) != 1 || (*limits)[0] != want {
				t.Fatalf("limit commands %q, want [%q]", *limits, want)
			}
		})
	}
}

// R59-05/03: the boot's inode backfill reads the mount's report once for all
// its servers and sets a limit only where one is missing, above the files a
// server holds; a server whose limit is lifted for a recount or a restore is
// left to that work's put-back. Run twice, it changes nothing the second time.
func TestInodeBackfillReadsOneReportPerMount(t *testing.T) {
	inodeAnchorDir(t)
	base := t.TempDir()
	for _, uuid := range []string{"limited", "over", "done", "unlimited", "lifted"} {
		if err := os.Mkdir(filepath.Join(base, uuid), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	report := repquotaLine("limited", 1024, 10240*1024, 5000, 0) +
		repquotaLine("over", 1024, 10240*1024, 1500000, 0) +
		repquotaLine("done", 1024, 10240*1024, 5000, 1000000) +
		repquotaLine("unlimited", 1024, 0, 5000, 0) +
		repquotaLine("lifted", 1024, 10240*1024, 5000, 0)
	reports, limits := fakeQuotaTools(t, report, nil)
	rdb := newDiskGuardRedis(t)
	rdb.Set(context.Background(), diskLiftedKey("lifted"), 1, 0)
	qp := &QuotaProvider{fsType: "ext4", mountPoint: "/mnt", basePath: base, available: true, rdb: rdb}

	qp.backfillInodeLimits(rdb, nil)

	if *reports != 1 {
		t.Fatalf("%d reports, want 1 for the mount", *reports)
	}
	want := []string{
		fmt.Sprintf("setquota -P %d 0 10485760 0 1000000 /mnt", projectID("limited")),
		fmt.Sprintf("setquota -P %d 0 10485760 0 1875000 /mnt", projectID("over")),
	}
	sort.Strings(want)
	got := append([]string(nil), *limits...)
	sort.Strings(got)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("limit commands %q, want %q", got, want)
	}

	// The second boot sees the limits the first one set.
	report = repquotaLine("limited", 1024, 10240*1024, 5000, 1000000) +
		repquotaLine("over", 1024, 10240*1024, 1500000, 1875000) +
		repquotaLine("done", 1024, 10240*1024, 5000, 1000000) +
		repquotaLine("unlimited", 1024, 0, 5000, 0)
	_, limits = fakeQuotaTools(t, report, nil)
	qp.backfillInodeLimits(rdb, nil)
	if len(*limits) != 0 {
		t.Fatalf("second backfill set %q, want nothing", *limits)
	}
}

// The reason travels with the gauge, so the panel can say the file count, not
// the bytes, is what is full. Unset for bytes, which is all an older node sent.
func TestDiskUsageReasonNamesTheFullerSide(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    DiskUsagePayload
		want string
	}{
		{"bytes", DiskUsagePayload{Total: 95, Limit: 100, Inodes: 10, InodeLimit: 1000}, ""},
		{"files", DiskUsagePayload{Total: 10, Limit: 100, Inodes: 1000, InodeLimit: 1000}, "files"},
		{"no inode limit", DiskUsagePayload{Total: 95, Limit: 100, Inodes: 10}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.reason(); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
	data, _ := json.Marshal(DiskUsagePayload{Total: 10, Limit: 100, Inodes: 1000, InodeLimit: 1000, Warning: "full", Reason: "files"})
	for _, field := range []string{`"inodes":1000`, `"inodeLimit":1000`, `"reason":"files"`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("gauge %s lacks %s", data, field)
		}
	}
	data, _ = json.Marshal(DiskUsagePayload{Total: 10, Limit: 100})
	if strings.Contains(string(data), "inode") || strings.Contains(string(data), "reason") {
		t.Errorf("a gauge without inodes carries %s", data)
	}
}

// inodeLimitSet runs one SetLimit against report and returns the inode hard
// limit it set.
func inodeLimitSet(t *testing.T, rdb *redis.Client, uuid, report string, limitMB int64) string {
	t.Helper()
	_, limits := fakeQuotaTools(t, report, nil)
	qp := &QuotaProvider{fsType: "ext4", mountPoint: "/mnt", available: true, rdb: rdb}
	if err := qp.SetLimit(uuid, limitMB); err != nil {
		t.Fatal(err)
	}
	if len(*limits) != 1 {
		t.Fatalf("limit commands %q, want one", *limits)
	}
	f := strings.Fields((*limits)[0])
	return f[len(f)-2]
}

// R61-02/05: files that could not be read grant nothing and store nothing, so
// the first read that can grant still does.
func TestInodeAnchorNotGrantedFromAnUnreadCount(t *testing.T) {
	inodeAnchorDir(t)
	rdb := newDiskGuardRedis(t)
	pid := projectID("srv")
	for _, step := range []struct {
		name, report string
		want         string
	}{
		{"files not read", fmt.Sprintf("#%d +- 21000000 0 20971520 3000000 0 0\n", pid), "0"},
		{"no files is no count", repquotaLine("srv", 0, 20971520, 0, 0), "1310720"},
		{"first read grants", fmt.Sprintf("#%d +- 21000000 0 20971520 0 3000000 0 0 0\n", pid), "3750000"},
	} {
		if got := inodeLimitSet(t, rdb, "srv", step.report, 20480); got != step.want {
			t.Fatalf("%s: inode limit %s, want %s", step.name, got, step.want)
		}
	}
}

// R61-03: a graced server's first grant is judged against its own limit, not
// the ceiling the project holds meanwhile, so it keeps it after the grace.
func TestInodeAnchorOfAGracedServerUsesItsOwnLimit(t *testing.T) {
	inodeAnchorDir(t)
	rdb := newDiskGuardRedis(t)
	recordDiskLimit(context.Background(), rdb, "srv", 51200)
	// 3.4M files: over formula(50 GB) = 3276800, under formula(55 GB ceiling).
	if got := inodeLimitSet(t, rdb, "srv", repquotaLine("srv", 54000000, 0, 3400000, 0), 56320); got != "4250000" {
		t.Fatalf("under the ceiling: inode limit %s, want 4250000", got)
	}
	if got := inodeLimitSet(t, rdb, "srv", repquotaLine("srv", 50000000, 56320*1024, 3400000, 4250000), 51200); got != "4250000" {
		t.Fatalf("grace over: inode limit %s, want 4250000", got)
	}
}

// R61-04: an anchor an older node kept in Redis moves to disk once.
func TestInodeAnchorMovesOutOfRedis(t *testing.T) {
	inodeAnchorDir(t)
	rdb := newDiskGuardRedis(t)
	ctx := context.Background()
	rdb.Set(ctx, inodeAnchorKey("srv"), 1875000, 0)
	if got := inodeLimitSet(t, rdb, "srv", repquotaLine("srv", 1024, 0, 2400000, 0), 10240); got != "1875000" {
		t.Fatalf("inode limit %s, want 1875000", got)
	}
	if rdb.Exists(ctx, inodeAnchorKey("srv")).Val() != 0 {
		t.Error("the Redis anchor is still there")
	}
	if v, ok, err := readInodeAnchor("srv"); err != nil || !ok || v != 1875000 {
		t.Errorf("anchor on disk (%d, %v, %v), want 1875000", v, ok, err)
	}
	forgetDiskLimit(ctx, rdb, "srv")
	if _, ok, _ := readInodeAnchor("srv"); ok {
		t.Error("a deleted server kept its anchor")
	}
}
