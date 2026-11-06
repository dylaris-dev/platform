package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CPUCore describes one logical CPU as the panel needs it for the pinning grid.
// Type is "P" (performance), "E" (efficiency) or "standard" (uniform / unknown).
// Sibling is the hyperthread partner's logical id, or -1 when none / unknown.
type CPUCore struct {
	ID          int    `json:"id"`
	Type        string `json:"type"`
	Sibling     int    `json:"sibling"`
	MaxClockMHz int    `json:"maxClockMHz"` // HW max clock, from cpuinfo_max_freq; 0 = unknown
	CacheGroup  int    `json:"cacheGroup"`  // 0-based L3 cache-domain index; -1 = unknown
	L3KB        int    `json:"l3KB"`        // L3 size of that domain in KB; 0 = unknown
}

// CPUTopology is the host CPU layout reported to Core for the pinning UI.
type CPUTopology struct {
	LogicalCount  int       `json:"logicalCount"`
	PhysicalCount int       `json:"physicalCount"`
	Hybrid        bool      `json:"hybrid"`
	Cores         []CPUCore `json:"cores"`
	ScannedAt     int64     `json:"scannedAt"`
}

var (
	cpuTopologyOnce sync.Once
	cpuTopology     *CPUTopology
)

// getCPUTopology scans the host once and caches it. A hardware change needs a
// node restart anyway (which re-runs this), so a one-shot scan is sufficient and
// avoids re-reading sysfs every heartbeat.
func getCPUTopology() *CPUTopology {
	cpuTopologyOnce.Do(func() {
		cpuTopology = scanCPUTopology()
	})
	return cpuTopology
}

// scanCPUTopology reads Linux sysfs. On non-Linux hosts (dev) or when sysfs is
// unavailable it falls back to a uniform NumCPU layout.
func scanCPUTopology() *CPUTopology {
	const base = "/sys/devices/system/cpu"
	entries, err := os.ReadDir(base)
	if err != nil {
		return uniformTopology()
	}

	var ids []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "cpu") {
			continue
		}
		n, err := strconv.Atoi(name[len("cpu"):]) // skip cpufreq, cpuidle, etc.
		if err != nil {
			continue
		}
		ids = append(ids, n)
	}
	ids = onlineCPUIDs(ids, readFileTrim(base+"/online"))
	if len(ids) == 0 {
		return uniformTopology()
	}
	sort.Ints(ids)

	// Intel hybrid (Linux >= 5.16) exposes the P/E split via per-PMU cpu lists.
	pSet := readCPUSet(base + "/../cpu_core/cpus")
	if len(pSet) == 0 {
		pSet = readCPUSet("/sys/devices/cpu_core/cpus")
	}
	eSet := readCPUSet("/sys/devices/cpu_atom/cpus")
	hybrid := len(pSet) > 0 && len(eSet) > 0

	// ARM big.LITTLE (and some others) expose per-core cpu_capacity; a spread of
	// capacities means heterogeneous cores even without the hybrid PMUs.
	caps := map[int]int{}
	maxCap := 0
	if !hybrid {
		for _, id := range ids {
			if c, ok := readIntFile(fmt.Sprintf("%s/cpu%d/cpu_capacity", base, id)); ok {
				caps[id] = c
				if c > maxCap {
					maxCap = c
				}
			}
		}
	}
	capHetero := false
	for _, c := range caps {
		if c != maxCap {
			capHetero = true
			break
		}
	}

	physical := map[string]bool{}
	cores := make([]CPUCore, 0, len(ids))
	for _, id := range ids {
		ctype := "standard"
		switch {
		case hybrid && pSet[id]:
			ctype = "P"
		case hybrid && eSet[id]:
			ctype = "E"
		case capHetero && len(caps) > 0:
			if caps[id] == maxCap {
				ctype = "P"
			} else {
				ctype = "E"
			}
		}

		sibling := -1
		for _, s := range readCPUListSorted(fmt.Sprintf("%s/cpu%d/topology/thread_siblings_list", base, id)) {
			if s != id {
				sibling = s
				break
			}
		}

		coreID, _ := readIntFile(fmt.Sprintf("%s/cpu%d/topology/core_id", base, id))
		pkgID, _ := readIntFile(fmt.Sprintf("%s/cpu%d/topology/physical_package_id", base, id))
		physical[fmt.Sprintf("%d:%d", pkgID, coreID)] = true

		cores = append(cores, CPUCore{ID: id, Type: ctype, Sibling: sibling})
	}

	// Best-effort display enrichment (X3D / V-Cache grouping + max clock). Every
	// read is independent and degrades only its own field to the absent value on
	// error, so the scan itself never fails because of these.
	cacheKeys := make([]string, len(cores))
	for i, c := range cores {
		if khz, ok := readIntFile(fmt.Sprintf("%s/cpu%d/cpufreq/cpuinfo_max_freq", base, c.ID)); ok {
			cores[i].MaxClockMHz = khzToMHz(khz)
		}
		cores[i].L3KB = parseCacheSize(readFileTrim(fmt.Sprintf("%s/cpu%d/cache/index3/size", base, c.ID)))
		cacheKeys[i] = compactCPUList(parseCPUList(readFileTrim(fmt.Sprintf("%s/cpu%d/cache/index3/shared_cpu_list", base, c.ID))))
	}
	for i, g := range cacheGroupIndices(cacheKeys) {
		cores[i].CacheGroup = g
	}

	return &CPUTopology{
		LogicalCount:  len(ids),
		PhysicalCount: len(physical),
		Hybrid:        hybrid || capHetero,
		Cores:         cores,
		ScannedAt:     time.Now().Unix(),
	}
}

func uniformTopology() *CPUTopology {
	n := runtime.NumCPU()
	cores := make([]CPUCore, n)
	for i := 0; i < n; i++ {
		cores[i] = CPUCore{ID: i, Type: "standard", Sibling: -1, MaxClockMHz: 0, CacheGroup: -1, L3KB: 0}
	}
	return &CPUTopology{
		LogicalCount:  n,
		PhysicalCount: n,
		Hybrid:        false,
		Cores:         cores,
		ScannedAt:     time.Now().Unix(),
	}
}

// readCPUSet parses a sysfs cpu list ("0-3,8,10-11") into a presence set.
// onlineCPUIDs keeps the ids the kernel lists as online. An offline CPU still
// has its cpuN directory but Docker refuses it in a cpuset, so offering it to
// the pinning grid or auto-pinning leaves a server that cannot start. An
// empty list (file unreadable) keeps every id, as before.
func onlineCPUIDs(ids []int, online string) []int {
	on := parseCPUList(online)
	if len(on) == 0 {
		return ids
	}
	set := make(map[int]bool, len(on))
	for _, id := range on {
		set[id] = true
	}
	kept := ids[:0:0]
	for _, id := range ids {
		if set[id] {
			kept = append(kept, id)
		}
	}
	return kept
}

func readCPUSet(path string) map[int]bool {
	set := map[int]bool{}
	for _, id := range parseCPUList(readFileTrim(path)) {
		set[id] = true
	}
	return set
}

func readCPUListSorted(path string) []int {
	list := parseCPUList(readFileTrim(path))
	sort.Ints(list)
	return list
}

// maxCPUID is the highest logical core id a cpuset may name: the kernel's
// NR_CPUS ceiling. parseCPUList also reads cpusets Core stored before it bounded
// them, on every container create and stats tick, so the expansion is capped
// here rather than trusted.
const maxCPUID = 8191

// parseCPUList expands "0-3,8,10-11" into [0 1 2 3 8 10 11]. Out-of-range parts
// are skipped and the total is capped at maxCPUID+1 ids.
func parseCPUList(s string) []int {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out []int
	// The kernel reads a cpuset as a bit set, so an overlapping or repeated part
	// ("0-3,2-5", "0,0") names each core once; callers count len() as cores.
	seen := map[int]bool{}
	add := func(n int) {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '-'); i >= 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err1 != nil || err2 != nil || lo < 0 || hi < lo || hi > maxCPUID {
				continue
			}
			for n := lo; n <= hi; n++ {
				if len(out) > maxCPUID {
					return out
				}
				add(n)
			}
		} else if n, err := strconv.Atoi(part); err == nil && n >= 0 && n <= maxCPUID && len(out) <= maxCPUID {
			add(n)
		}
	}
	return out
}

// compactCPUList renders sorted ids as a cpuset string, collapsing runs
// ([0 1 2 3 8] -> "0-3,8"). Inverse of parseCPUList.
func compactCPUList(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	sort.Ints(ids)
	var parts []string
	start, prev := ids[0], ids[0]
	flush := func() {
		if start == prev {
			parts = append(parts, strconv.Itoa(start))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, id := range ids[1:] {
		if id == prev+1 {
			prev = id
			continue
		}
		flush()
		start, prev = id, id
	}
	flush()
	return strings.Join(parts, ",")
}

// sanitizeCpusetForHost drops cpuset cores that do not exist on THIS host and
// returns the compacted valid subset ("" = run unpinned). This is the safety net
// for a server that moved to a node with fewer or differently-numbered cores: a
// stale cpuset baked into .node_config.json (or sent by Core) never references a
// core that is not here, so the container can always start. Logs when it changes.
func sanitizeCpusetForHost(spec, uuid string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	topo := getCPUTopology()
	valid := make(map[int]bool, len(topo.Cores))
	for _, c := range topo.Cores {
		valid[c.ID] = true
	}
	var kept []int
	for _, id := range parseCPUList(spec) {
		if valid[id] {
			kept = append(kept, id)
		}
	}
	out := compactCPUList(kept)
	if out != spec {
		log.Printf("cpuset for %s adjusted to host topology: %q -> %q (host has %d logical cores)", uuid, spec, out, topo.LogicalCount)
	}
	return out
}

func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readIntFile(path string) (int, bool) {
	s := readFileTrim(path)
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// khzToMHz converts a cpufreq kHz value (e.g. cpuinfo_max_freq) to whole MHz.
// Non-positive input yields 0 (the absent value).
func khzToMHz(khz int) int {
	if khz <= 0 {
		return 0
	}
	return khz / 1000
}

// parseCacheSize parses a sysfs cache "size" value into kilobytes. A bare number
// is treated as KB (the kernel convention), a "K"/"KB" suffix is KB, and a "M"/"MB"
// suffix is KB*1024. Empty or unparsable input yields 0.
func parseCacheSize(s string) int {
	up := strings.ToUpper(strings.TrimSpace(s))
	if up == "" {
		return 0
	}
	up = strings.TrimSuffix(up, "B") // "MB"/"KB" -> "M"/"K"
	mult := 1
	switch {
	case strings.HasSuffix(up, "M"):
		mult = 1024
		up = strings.TrimSuffix(up, "M")
	case strings.HasSuffix(up, "K"):
		up = strings.TrimSuffix(up, "K")
	}
	n, err := strconv.Atoi(strings.TrimSpace(up))
	if err != nil || n < 0 {
		return 0
	}
	return n * mult
}

// cacheGroupIndices assigns a stable 0-based cache-domain group index to each core
// given that core's canonical L3 shared-cpu-list key (e.g. "0-7"). Cores whose key
// is empty (no index3 data) get -1. Groups are ordered by ascending lowest cpu id
// in the shared list, so the domain containing cpu0 is always group 0. The returned
// slice is parallel to keys.
func cacheGroupIndices(keys []string) []int {
	lowest := map[string]int{}
	for _, k := range keys {
		if k == "" {
			continue
		}
		if _, seen := lowest[k]; seen {
			continue
		}
		min := -1
		for _, id := range parseCPUList(k) {
			if min == -1 || id < min {
				min = id
			}
		}
		lowest[k] = min
	}
	distinct := make([]string, 0, len(lowest))
	for k := range lowest {
		distinct = append(distinct, k)
	}
	sort.Slice(distinct, func(i, j int) bool {
		li, lj := lowest[distinct[i]], lowest[distinct[j]]
		if li != lj {
			return li < lj
		}
		return distinct[i] < distinct[j] // deterministic tie-break (should not occur physically)
	})
	index := make(map[string]int, len(distinct))
	for i, k := range distinct {
		index[k] = i
	}
	out := make([]int, len(keys))
	for i, k := range keys {
		if k == "" {
			out[i] = -1
			continue
		}
		out[i] = index[k]
	}
	return out
}
