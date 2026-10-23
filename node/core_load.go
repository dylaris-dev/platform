package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// coreLoadInterval matches the live stats cadence, so the per-core bars and
// the CPU chart move together.
const coreLoadInterval = 2 * time.Second

// coreTicks is one core's /proc/stat counters reduced to what a percentage
// needs.
type coreTicks struct{ busy, total uint64 }

// runCoreLoadPublisher publishes how busy every HOST core is, for the panel's
// per-core view of a pinned server.
//
// Host-wide on purpose: cgroup v2 has no per-CPU breakdown for a container, so
// "how much of core 3 is this server" is not measurable. "How busy is core 3"
// is, and for a server that has the core to itself it is the same number.
// Core decides whether it does. /proc/stat is not namespaced, so inside the
// node's container it still lists the host's cores.
func runCoreLoadPublisher(ctx context.Context, rdb *redis.Client, id string) {
	key := fmt.Sprintf("dylaris:node:%s:cpuload", id)
	t := time.NewTicker(coreLoadInterval)
	defer t.Stop()
	var prev map[int]coreTicks
	for {
		cur, err := readProcStatCores("/proc/stat")
		if err != nil {
			log.Printf("core load: %v", err)
			return
		}
		if prev != nil {
			b, _ := json.Marshal(map[string]any{"cores": coreLoad(prev, cur)})
			rdb.Set(ctx, key, b, 10*time.Second)
		}
		prev = cur
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func readProcStatCores(path string) (map[int]coreTicks, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	out := map[int]coreTicks{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		id, ticks, ok := parseProcStatCPU(sc.Text())
		if ok {
			out[id] = ticks
		}
	}
	return out, sc.Err()
}

// parseProcStatCPU reads one "cpuN user nice system idle iowait irq softirq
// steal ..." line. The aggregate "cpu" line is not a core and is skipped.
// idle and iowait are the time the core had nothing to run.
func parseProcStatCPU(line string) (int, coreTicks, bool) {
	f := strings.Fields(line)
	if len(f) < 9 || !strings.HasPrefix(f[0], "cpu") || f[0] == "cpu" {
		return 0, coreTicks{}, false
	}
	id, err := strconv.Atoi(f[0][3:])
	if err != nil {
		return 0, coreTicks{}, false
	}
	var t coreTicks
	// guest and guest_nice (fields 9 and 10) are already inside user and nice.
	for i, s := range f[1:min(len(f), 9)] {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, coreTicks{}, false
		}
		t.total += n
		if i != 3 && i != 4 {
			t.busy += n
		}
	}
	return id, t, true
}

// coreLoad is the busy percent of each core between two samples. A core that
// went offline in between, or whose counters did not move, is left out.
func coreLoad(prev, cur map[int]coreTicks) map[int]float64 {
	out := make(map[int]float64, len(cur))
	for id, c := range cur {
		p, ok := prev[id]
		if !ok || c.total <= p.total || c.busy < p.busy {
			continue
		}
		pct := float64(c.busy-p.busy) / float64(c.total-p.total) * 100
		out[id] = min(pct, 100)
	}
	return out
}
