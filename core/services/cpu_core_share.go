package services

import "dylaris-core/models"

// PinnedCoreSharing returns the cores target is pinned to and, per core,
// whether another server on the node may run there too.
//
// A server that is not pinned returns nil: it floats over the node's pool, so
// no core is its own and a per-core view would only show other servers' load.
// Another server's cores are its EFFECTIVE set - a shared-mode server runs
// anywhere in the node pool, and an empty pool means every core (allCores).
// Stopped servers count too: whether a core is exclusive must not flip each
// time a neighbour starts.
func PinnedCoreSharing(target models.Server, others []models.Server, nodeCpuset string, allCores []int) ([]int, map[int]bool) {
	if target.CPUPinningMode != "auto" && target.CPUPinningMode != "manual" {
		return nil, nil
	}
	cores := parseCpusetList(target.Cpuset)
	if len(cores) == 0 {
		return nil, nil
	}
	mine := make(map[int]bool, len(cores))
	for _, c := range cores {
		mine[c] = true
	}
	shared := map[int]bool{}
	for _, o := range others {
		if o.ID == target.ID {
			continue
		}
		set := parseCpusetList(models.EffectiveCpuset(o.CPUPinningMode, o.Cpuset, nodeCpuset))
		if len(set) == 0 {
			set = allCores
		}
		for _, c := range set {
			if mine[c] {
				shared[c] = true
			}
		}
	}
	return cores, shared
}
