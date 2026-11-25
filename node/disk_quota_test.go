package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRepquota(t *testing.T) {
	// `repquota -P -n` on ext4, quota-tools 4.09. Block columns are 1 KiB.
	const report = `*** Report for project quotas on device /dev/sdb1
Block grace time: 7days; Inode grace time: 7days
                        Block limits                File limits
Project         used    soft    hard  grace    used  soft  hard  grace
----------------------------------------------------------------------
#0        --      20       0       0              2     0     0
#3826675829 --  1048576       0 2097152           1200     0     0
#12345    --  512000       0       0             40     0     0
#77       +-  2048000 1024000 4096000  6days    300     0     0
#78       +-  2048000 1024000 4096000   none    300     0     0
#79       ++  2048000 1024000 4096000  23:59    300   100     0  6days

`
	for _, tc := range []struct {
		name        string
		pid         uint32
		used, limit int64
	}{
		{"hard limit set", 3826675829, 1048576 * 1024, 2097152 * 1024},
		{"no limit", 12345, 512000 * 1024, 0},
		{"over soft, grace running", 77, 2048000 * 1024, 4096000 * 1024},
		{"over soft, grace expired", 78, 2048000 * 1024, 4096000 * 1024},
		{"over soft, grace in hours, inode grace too", 79, 2048000 * 1024, 4096000 * 1024},
		{"not in the report", 4242, -1, 0},
		// "#1234" must not match a prefix of another id.
		{"id prefix of another", 1234, -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			used, limit := parseRepquota(report, tc.pid)
			if used != tc.used || limit != tc.limit {
				t.Fatalf("got (%d, %d), want (%d, %d)", used, limit, tc.used, tc.limit)
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
	if used, limit := parseRepquota(report, 12345); used != -1 || limit != 0 {
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
