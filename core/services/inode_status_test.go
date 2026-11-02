package services

import "testing"

// Inodes run out with empty files long before a disk limit; the capacity
// picture says how many are left, and -1 when the node did not say.
func TestPathStatusesReportFreeInodes(t *testing.T) {
	st := PathStatuses([]HeartbeatStoragePath{
		{Path: "/a", TotalBytes: 100 << 30, FreeBytes: 50 << 30, InodesTotal: 1000, InodesFree: 30},
		{Path: "/b", TotalBytes: 100 << 30, FreeBytes: 50 << 30},
	}, nil, nil, 0, 80, 90)
	if st[0].InodesFreePercent != 3 || st[1].InodesFreePercent != -1 {
		t.Fatalf("inodes free = %d, %d; want 3, -1", st[0].InodesFreePercent, st[1].InodesFreePercent)
	}
}
