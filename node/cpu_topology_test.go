package main

import (
	"reflect"
	"strconv"
	"testing"
)

func TestParseCPUList(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"", nil},
		{"0", []int{0}},
		{"0-3", []int{0, 1, 2, 3}},
		{"0-3,8,10-11", []int{0, 1, 2, 3, 8, 10, 11}},
		{" 0 - 2 , 5 ", []int{0, 1, 2, 5}},
		{"3-1", nil},      // inverted range ignored
		{"x,2", []int{2}}, // junk token ignored
	}
	for _, c := range cases {
		got := parseCPUList(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseCPUList(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCompactCPUList(t *testing.T) {
	cases := []struct {
		in   []int
		want string
	}{
		{nil, ""},
		{[]int{2}, "2"},
		{[]int{0, 1, 2, 3}, "0-3"},
		{[]int{3, 1, 0, 2, 8}, "0-3,8"}, // unsorted input
	}
	for _, c := range cases {
		if got := compactCPUList(c.in); got != c.want {
			t.Errorf("compactCPUList(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeCpusetForHost(t *testing.T) {
	// getCPUTopology() falls back to a uniform NumCPU layout on the test host,
	// so cores 0..N-1 exist. A core well beyond that must be dropped.
	n := getCPUTopology().LogicalCount
	if n < 1 {
		t.Skip("no cores reported")
	}
	// In-range core is kept.
	if got := sanitizeCpusetForHost("0", "test"); got != "0" {
		t.Fatalf("in-range core dropped: %q", got)
	}
	// Far out-of-range core is dropped -> empty (unpinned).
	if got := sanitizeCpusetForHost(strconv.Itoa(n+100), "test"); got != "" {
		t.Fatalf("out-of-range core not dropped: %q", got)
	}
	// Empty stays empty.
	if got := sanitizeCpusetForHost("", "test"); got != "" {
		t.Fatalf("empty should stay empty: %q", got)
	}
}

func TestUniformTopology(t *testing.T) {
	topo := uniformTopology()
	if topo.LogicalCount != len(topo.Cores) {
		t.Fatalf("LogicalCount %d != len(Cores) %d", topo.LogicalCount, len(topo.Cores))
	}
	if topo.LogicalCount == 0 {
		t.Fatal("expected at least one core")
	}
	for i, c := range topo.Cores {
		if c.ID != i || c.Type != "standard" || c.Sibling != -1 {
			t.Fatalf("core %d unexpected: %+v", i, c)
		}
		if c.MaxClockMHz != 0 || c.CacheGroup != -1 || c.L3KB != 0 {
			t.Fatalf("core %d display fields must be absent (0/-1/0): %+v", i, c)
		}
	}
	if topo.Hybrid {
		t.Fatal("uniform topology must not be hybrid")
	}
}

func TestKhzToMHz(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 0},
		{-5, 0},
		{999, 0}, // sub-MHz truncates to 0
		{1000, 1},
		{3200000, 3200},
		{4800000, 4800},
	}
	for _, c := range cases {
		if got := khzToMHz(c.in); got != c.want {
			t.Errorf("khzToMHz(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParseCacheSize(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"garbage", 0},
		{"512", 512}, // bare digits are KB (kernel convention)
		{"32768K", 32768},
		{"512KB", 512},
		{"1M", 1024},
		{"96M", 98304},
		{"96MB", 98304},
		{" 96 MB ", 98304}, // surrounding whitespace tolerated
	}
	for _, c := range cases {
		if got := parseCacheSize(c.in); got != c.want {
			t.Errorf("parseCacheSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCacheGroupIndices(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want []int
	}{
		{"all empty (no cache data)", []string{"", "", ""}, []int{-1, -1, -1}},
		{"two even CCDs", []string{"0-3", "0-3", "0-3", "0-3", "4-7", "4-7", "4-7", "4-7"}, []int{0, 0, 0, 0, 1, 1, 1, 1}},
		{"ordered by lowest member, not appearance", []string{"4-7", "4-7", "0-3", "0-3"}, []int{1, 1, 0, 0}},
		{"mixed with a missing core", []string{"0-3", "", "0-3"}, []int{0, -1, 0}},
		{"empty input", nil, []int{}},
	}
	for _, c := range cases {
		got := cacheGroupIndices(c.keys)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: cacheGroupIndices(%v) = %v, want %v", c.name, c.keys, got, c.want)
		}
	}
}

// L38: a present but offline CPU (SMT off via nosmt) is not a core Docker
// accepts, so the scan keeps only what /sys/devices/system/cpu/online lists.
func TestOnlineCPUIDs(t *testing.T) {
	all := []int{0, 1, 2, 3, 4, 5, 6, 7}
	cases := []struct {
		online string
		want   []int
	}{
		{"0-3", []int{0, 1, 2, 3}},
		{"0,2,4-5", []int{0, 2, 4, 5}},
		{"0-7", all},
		{"", all}, // unreadable: keep the directory scan
	}
	for _, c := range cases {
		if got := onlineCPUIDs(all, c.online); !reflect.DeepEqual(got, c.want) {
			t.Errorf("onlineCPUIDs(%q) = %v, want %v", c.online, got, c.want)
		}
	}
}
