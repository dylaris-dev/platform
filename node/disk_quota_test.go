package main

import "testing"

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
