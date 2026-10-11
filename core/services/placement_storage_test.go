package services

import (
	"testing"

	"dylaris-core/store"
)

type headroomFakeStore struct {
	store.Store
	value string
}

func (f headroomFakeStore) GetSetting(key string) (string, error) {
	if key != DiskHeadroomSetting {
		return "", nil
	}
	return f.value, nil
}

// The headroom is a floor, so every unusable input must fall back to the
// DEFAULT, never to something more permissive - a permissive fallback would
// quietly defeat the whole setting.
func TestDiskHeadroomGB(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int64
	}{
		{name: "unset falls back to the default", value: "", want: DefaultDiskHeadroomGB},
		{name: "garbage falls back to the default", value: "lots", want: DefaultDiskHeadroomGB},
		{name: "below the minimum falls back to the default", value: "1", want: DefaultDiskHeadroomGB},
		{name: "zero falls back to the default", value: "0", want: DefaultDiskHeadroomGB},
		{name: "negative falls back to the default", value: "-100", want: DefaultDiskHeadroomGB},
		{name: "exactly the minimum is honoured", value: "10", want: MinDiskHeadroomGB},
		{name: "a larger value is honoured", value: "200", want: 200},
		{name: "surrounding whitespace is tolerated", value: " 120 ", want: 120},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DiskHeadroomGB(headroomFakeStore{value: tt.value}); got != tt.want {
				t.Errorf("DiskHeadroomGB(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}

	if got := DiskHeadroomGB(nil); got != DefaultDiskHeadroomGB {
		t.Errorf("DiskHeadroomGB(nil) = %d, want %d", got, DefaultDiskHeadroomGB)
	}
}

const gib = int64(1024 * 1024 * 1024)

func hbPaths(spec ...any) []HeartbeatStoragePath {
	out := make([]HeartbeatStoragePath, 0, len(spec)/2)
	for i := 0; i+1 < len(spec); i += 2 {
		out = append(out, HeartbeatStoragePath{
			Path:      spec[i].(string),
			FreeBytes: int64(spec[i+1].(int)) * gib,
		})
	}
	return out
}

// Core must predict the path the NODE picks. Measuring the largest path instead
// is what let the scheduler admit a server onto capacity the node would not use.
func TestPickPlacementPath(t *testing.T) {
	storage := hbPaths("/small", 10, "/big", 500, "/mid", 100)

	tests := []struct {
		name      string
		placement NodePlacement
		wantPath  string
		wantFree  int64
	}{
		{
			name:      "auto takes the most free space",
			placement: NodePlacement{Mode: "auto"},
			wantPath:  "/big",
			wantFree:  500 * gib,
		},
		{
			// The case the old max-free logic got wrong.
			name:      "manual takes the operator's first path even when it is the smallest",
			placement: NodePlacement{Mode: "manual", Order: []string{"/small", "/big"}},
			wantPath:  "/small",
			wantFree:  10 * gib,
		},
		{
			name:      "manual with an empty order falls back to the node's own path order",
			placement: NodePlacement{Mode: "manual"},
			wantPath:  "/small",
			wantFree:  10 * gib,
		},
		{
			name:      "manual skips a path reporting no free space",
			placement: NodePlacement{Mode: "manual", Order: []string{"/full", "/mid"}},
			wantPath:  "/mid",
			wantFree:  100 * gib,
		},
		{
			name:      "an unrecognised mode behaves as auto",
			placement: NodePlacement{Mode: "nonsense"},
			wantPath:  "/big",
			wantFree:  500 * gib,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := storage
			if tt.name == "manual skips a path reporting no free space" {
				s = append(hbPaths("/full", 0), storage...)
			}
			gotPath, gotFree := PickPlacementPath(s, tt.placement)
			if gotPath != tt.wantPath || gotFree != tt.wantFree {
				t.Errorf("PickPlacementPath() = (%q, %d), want (%q, %d)", gotPath, gotFree, tt.wantPath, tt.wantFree)
			}
		})
	}
}

// When every path in the order is unusable the node falls through to free-space
// selection. Core must mirror that, or it would reject a placement the node
// would happily accept.
func TestPickPlacementPathMirrorsTheNodesFallback(t *testing.T) {
	storage := hbPaths("/full", 0, "/ok", 200)
	got, free := PickPlacementPath(storage, NodePlacement{Mode: "manual", Order: []string{"/full"}})
	if got != "/ok" || free != 200*gib {
		t.Errorf("PickPlacementPath() = (%q, %d), want (/ok, %d)", got, free, 200*gib)
	}
}

func TestPickPlacementPathWithNoStorage(t *testing.T) {
	if path, free := PickPlacementPath(nil, NodePlacement{}); path != "" || free != 0 {
		t.Errorf("PickPlacementPath(nil) = (%q, %d), want (\"\", 0)", path, free)
	}
}

func TestCheckDiskCapacityHeadroom(t *testing.T) {
	storage := hbPaths("/a", 1000)

	tests := []struct {
		name   string
		wantGB int64
		want   bool
	}{
		{name: "comfortably fits", wantGB: 100, want: true},
		{name: "exactly fits against the headroom", wantGB: 1000 - 50, want: true},
		{name: "one GB past the headroom does not fit", wantGB: 1000 - 50 + 1, want: false},
		{name: "far too large", wantGB: 5000, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := CheckDiskCapacity(DiskCapacityRequest{
				Storage: storage, HeadroomGB: 50, WantMB: tt.wantGB * 1024,
			})
			if res.Fits != tt.want {
				t.Errorf("Fits = %t, want %t (free %d GB, available %d GB)", res.Fits, tt.want, res.FreeGB, res.AvailableGB)
			}
		})
	}
}

// The scenario that motivated this: 20 servers with a 50 GB limit have promised
// a terabyte the moment they are created, even while their directories are
// nearly empty. Free space alone says yes; commitment says no.
func TestCheckDiskCapacityCountsPromisesNotJustUsage(t *testing.T) {
	storage := []HeartbeatStoragePath{{
		Path: "/a", TotalBytes: 1000 * gib, FreeBytes: 900 * gib, UsedBytes: 100 * gib,
		ServerUUIDs: make([]string, 0, 20),
	}}
	limits := map[string]int64{}
	for i := 0; i < 20; i++ {
		uuid := string(rune('a'+i)) + "-server"
		storage[0].ServerUUIDs = append(storage[0].ServerUUIDs, uuid)
		limits[uuid] = 50 * 1024 // 50 GB, in MB
	}

	committed := CommittedBytesByPath(storage, limits)
	if got := committed["/a"] / gib; got != 1000 {
		t.Fatalf("committed = %d GB, want 1000 GB", got)
	}

	req := DiskCapacityRequest{Storage: storage, Unwritten: UnwrittenBytesByPath(storage, limits, nil), HeadroomGB: 50, WantMB: 50 * 1024}
	res := CheckDiskCapacity(req)
	if res.Fits {
		t.Errorf("Fits = true, want false: 900 GB free but %d GB of it is already promised", res.UnwrittenGB)
	}

	// Without the commitment map this is exactly the old, wrong answer.
	if loose := CheckDiskCapacity(DiskCapacityRequest{Storage: storage, HeadroomGB: 50, WantMB: 50 * 1024}); !loose.Fits {
		t.Error("without commitment the check should still pass; the test is asserting the DIFFERENCE commitment makes")
	}
}

// Servers that already wrote their data must not be counted twice: what is on
// disk is already out of FreeBytes, and only the unwritten remainder is still a
// reservation.
func TestCheckDiskCapacitySubtractsOnlyUnwrittenPromises(t *testing.T) {
	storage := []HeartbeatStoragePath{{
		Path: "/a", FreeBytes: 500 * gib, UsedBytes: 500 * gib, ServerUUIDs: []string{"s1"},
	}}
	// Promised 400 GB, the server itself measured 500 GB: nothing is left to
	// reserve, so the full 500 GB free (minus headroom) is available.
	res := CheckDiskCapacity(DiskCapacityRequest{
		Storage:    storage,
		Unwritten:  UnwrittenBytesByPath(storage, map[string]int64{"s1": 400 * 1024}, map[string]int64{"s1": 500 * gib}),
		HeadroomGB: 50,
		WantMB:     400 * 1024,
	})
	if !res.Fits {
		t.Errorf("Fits = false, want true: unwritten = %d GB, available = %d GB", res.UnwrittenGB, res.AvailableGB)
	}
	if res.UnwrittenGB != 0 {
		t.Errorf("UnwrittenGB = %d, want 0 - written bytes must not be reserved twice", res.UnwrittenGB)
	}
}

// A server with no limit promises nothing, so it cannot reserve anything.
func TestCommittedBytesIgnoresUnlimitedServers(t *testing.T) {
	storage := []HeartbeatStoragePath{{Path: "/a", ServerUUIDs: []string{"limited", "unlimited"}}}
	got := CommittedBytesByPath(storage, map[string]int64{"limited": 10 * 1024, "unlimited": 0})
	if want := int64(10) * gib; got["/a"] != want {
		t.Errorf("committed = %d, want %d", got["/a"], want)
	}
}

// The percentage must be PROJECTED (written + still promised), not current
// usage. Reporting current usage would call a path 10% full right up to the
// moment twenty servers grow into their limits at once - which is the surprise
// this whole feature exists to remove.
func TestPathStatusesProjectPromises(t *testing.T) {
	tests := []struct {
		name        string
		totalGB     int64
		freeGB      int64
		usedGB      int64
		committedGB int64
		headroomGB  int64
		wantStatus  string
		wantPercent int
	}{
		{
			name:    "empty and uncommitted is ok",
			totalGB: 1000, freeGB: 1000, usedGB: 0, committedGB: 0, headroomGB: 50,
			wantStatus: DiskStatusOK, wantPercent: 0,
		},
		{
			// Nearly empty on disk, but almost everything is spoken for. The
			// headroom is exactly consumed, not yet breached, so this is the
			// last warning before placements start being refused.
			name:    "nearly empty but heavily promised is critical",
			totalGB: 1000, freeGB: 950, usedGB: 50, committedGB: 950, headroomGB: 50,
			wantStatus: DiskStatusCritical, wantPercent: 95,
		},
		{
			name:    "past the warn threshold only",
			totalGB: 1000, freeGB: 900, usedGB: 100, committedGB: 850, headroomGB: 50,
			wantStatus: DiskStatusWarning, wantPercent: 85,
		},
		{
			// Promises already exceed what is physically there.
			name:    "over-promised past the headroom is breached",
			totalGB: 1000, freeGB: 900, usedGB: 100, committedGB: 1000, headroomGB: 50,
			wantStatus: DiskStatusBreached, wantPercent: 100,
		},
		{
			name:    "a node reporting no total is unknown, not full",
			totalGB: 0, freeGB: 0, usedGB: 0, committedGB: 0, headroomGB: 50,
			wantStatus: DiskStatusUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage := []HeartbeatStoragePath{{
				Path:       "/a",
				TotalBytes: tt.totalGB * gib, FreeBytes: tt.freeGB * gib, UsedBytes: tt.usedGB * gib,
			}}
			committed := map[string]int64{"/a": tt.committedGB * gib}
			// Every used byte belongs to a server in these cases.
			unwritten := map[string]int64{"/a": max(0, tt.committedGB-tt.usedGB) * gib}

			got := PathStatuses(storage, committed, unwritten, tt.headroomGB, DefaultDiskWarnPercent, DefaultDiskCritPercent)
			if len(got) != 1 {
				t.Fatalf("PathStatuses returned %d entries, want 1", len(got))
			}
			if got[0].Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q (projected %d%%, available %d GB)",
					got[0].Status, tt.wantStatus, got[0].ProjectedPercent, got[0].AvailableGB)
			}
			if tt.wantStatus != DiskStatusUnknown && got[0].ProjectedPercent != tt.wantPercent {
				t.Errorf("ProjectedPercent = %d, want %d", got[0].ProjectedPercent, tt.wantPercent)
			}
		})
	}
}

// The enforcement mode and thresholds must fall back to safe values, and a
// warning that fires after critical would never be seen.
func TestDiskEnforcementAndThresholds(t *testing.T) {
	if got := DiskEnforcement(nil); got != DiskEnforcementSoft {
		t.Errorf("DiskEnforcement(nil) = %q, want %q - soft is the safe default", got, DiskEnforcementSoft)
	}
	if got := DiskEnforcement(headroomFakeStore{}); got != DiskEnforcementSoft {
		t.Errorf("unset DiskEnforcement = %q, want %q", got, DiskEnforcementSoft)
	}

	warn, critical := DiskThresholds(nil)
	if warn != DefaultDiskWarnPercent || critical != DefaultDiskCritPercent {
		t.Errorf("DiskThresholds(nil) = (%d, %d), want (%d, %d)", warn, critical, DefaultDiskWarnPercent, DefaultDiskCritPercent)
	}
}

// A node that reports nothing is unknown, not full: refusing every placement on
// a missing metric would take the fleet out of service on a reporting glitch.
func TestCheckDiskCapacityTreatsUnknownAsFitting(t *testing.T) {
	for _, tt := range []struct {
		name    string
		storage []HeartbeatStoragePath
	}{
		{name: "no storage reported", storage: nil},
		{name: "zero free reported", storage: hbPaths("/a", 0)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := CheckDiskCapacity(DiskCapacityRequest{Storage: tt.storage, HeadroomGB: 50, WantMB: 500 * 1024})
			if !res.Fits {
				t.Error("Fits = false, want true (unknown, not full)")
			}
			if !res.Undetermined {
				t.Error("Undetermined = false, want true so callers can tell a real answer from a missing metric")
			}
		})
	}
}

// The production shape that exposed the old formula: one server with an 80 GiB
// limit that has written 18.2 GB, on a 432 GB disk whose filesystem reports
// 228 GB used (OS, images, other workloads). The old formula subtracted the
// FILESYSTEM usage from the promise, clamped to zero and called all 204 GB
// promisable. The server can still write ~61.8 GB of it.
func TestUnwrittenIsPerServerNotFilesystemUsage(t *testing.T) {
	const gb = int64(1000 * 1000 * 1000)
	storage := []HeartbeatStoragePath{{
		Path: "/srv", TotalBytes: 432 * gb, UsedBytes: 228 * gb, FreeBytes: 204 * gb,
		ServerUUIDs: []string{"s329"},
	}}
	limits := map[string]int64{"s329": 80 * 1024}
	used := map[string]int64{"s329": 18_200_000_000}

	unwritten := UnwrittenBytesByPath(storage, limits, used)
	want := 80*gib - 18_200_000_000
	if unwritten["/srv"] != want {
		t.Fatalf("unwritten = %d, want %d", unwritten["/srv"], want)
	}

	res := CheckDiskCapacity(DiskCapacityRequest{Storage: storage, Unwritten: unwritten, HeadroomGB: 50, WantMB: 1024})
	if wantAvail := (204*gb - want - 50*gib) / gib; res.AvailableGB != wantAvail {
		t.Errorf("AvailableGB = %d, want %d", res.AvailableGB, wantAvail)
	}
	if res.UnwrittenGB != want/gib {
		t.Errorf("UnwrittenGB = %d, want %d", res.UnwrittenGB, want/gib)
	}

	st := PathStatuses(storage, CommittedBytesByPath(storage, limits), unwritten, 50, DefaultDiskWarnPercent, DefaultDiskCritPercent)
	if wantPct := int((228*gb + want) * 100 / (432 * gb)); st[0].ProjectedPercent != wantPct {
		t.Errorf("ProjectedPercent = %d, want %d", st[0].ProjectedPercent, wantPct)
	}
	if st[0].UnwrittenGB != want/gib {
		t.Errorf("status UnwrittenGB = %d, want %d", st[0].UnwrittenGB, want/gib)
	}
}

func TestUnwrittenBytesByPath(t *testing.T) {
	storage := []HeartbeatStoragePath{{Path: "/a", ServerUUIDs: []string{"a", "b", "c", "d"}}}
	limits := map[string]int64{"a": 10 * 1024, "b": 10 * 1024, "c": 10 * 1024, "d": 0}
	used := map[string]int64{"a": 4 * gib, "b": 12 * gib, "d": 99 * gib} // c unmeasured
	// a: 6 left; b: over its limit, 0 (never negative); c: unknown, whole 10;
	// d: unlimited, promised nothing.
	if got, want := UnwrittenBytesByPath(storage, limits, used)["/a"], 16*gib; got != want {
		t.Errorf("unwritten = %d GiB, want %d GiB", got/gib, want/gib)
	}
}
