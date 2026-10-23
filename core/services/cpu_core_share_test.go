package services

import (
	"reflect"
	"testing"

	"dylaris-core/models"
)

func TestPinnedCoreSharing(t *testing.T) {
	all := []int{0, 1, 2, 3, 4, 5, 6, 7}
	srv := func(id int, mode, cpuset string) models.Server {
		return models.Server{ID: id, CPUPinningMode: mode, Cpuset: cpuset}
	}
	target := srv(1, "auto", "0-3")
	cases := []struct {
		name       string
		target     models.Server
		others     []models.Server
		pool       string
		wantCores  []int
		wantShared map[int]bool
	}{
		{"alone", target, []models.Server{target}, "", []int{0, 1, 2, 3}, map[int]bool{}},
		{"disjoint pinned neighbour", target, []models.Server{srv(2, "manual", "4-5")}, "", []int{0, 1, 2, 3}, map[int]bool{}},
		{"overlapping pinned neighbour", target, []models.Server{srv(2, "auto", "3-4")}, "", []int{0, 1, 2, 3}, map[int]bool{3: true}},
		{"shared-mode neighbour, no pool, runs everywhere", target, []models.Server{srv(2, "shared", "")}, "", []int{0, 1, 2, 3}, map[int]bool{0: true, 1: true, 2: true, 3: true}},
		{"shared-mode neighbour kept off by the node pool", target, []models.Server{srv(2, "shared", "")}, "4-7", []int{0, 1, 2, 3}, map[int]bool{}},
		{"a stale cpuset on a shared-mode neighbour is not its set", target, []models.Server{srv(2, "shared", "0")}, "5-7", []int{0, 1, 2, 3}, map[int]bool{}},
		{"target not pinned", srv(1, "shared", "0-3"), nil, "", nil, nil},
	}
	for _, c := range cases {
		cores, shared := PinnedCoreSharing(c.target, c.others, c.pool, all)
		if !reflect.DeepEqual(cores, c.wantCores) || !reflect.DeepEqual(shared, c.wantShared) {
			t.Errorf("%s: got cores=%v shared=%v, want %v %v", c.name, cores, shared, c.wantCores, c.wantShared)
		}
	}
}
