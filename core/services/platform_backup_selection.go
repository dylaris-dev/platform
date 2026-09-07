package services

import (
	"strconv"

	"dylaris-core/models"
)

// SelectBackupServers turns a saved selection into the servers one run covers,
// plus the ids it named that no longer exist.
//
// Pure, over a list the caller has already fetched, rather than five variants
// of a WHERE clause. The fleet is tens of servers, the whole list is one query
// the run needs anyway, and the two interesting cases - a deleted server and
// the BYON distinction - are both easier to get right and to test in Go than in
// SQL. ponytail: linear over the fleet; push it into SQL if a platform ever
// holds enough servers for that to matter.
//
// A missing id is RETURNED, not dropped and not an error. A selection outlives
// what it names, so an id pointing at a deleted server is the ordinary state of
// a healthy configuration, and the run has to carry on and say what it skipped.
func SelectBackupServers(all []models.BackupTargetServer, sel models.PlatformBackupServers) (picked []models.BackupTargetServer, missing []int) {
	switch sel.Mode {
	case models.PlatformBackupServersAll:
		return append([]models.BackupTargetServer{}, all...), nil

	case models.PlatformBackupServersBYON:
		for _, s := range all {
			if s.BYON {
				picked = append(picked, s)
			}
		}
		return picked, nil

	case models.PlatformBackupServersOwner:
		if sel.OwnerID == nil || *sel.OwnerID == "" {
			// A selection that names no owner selects nobody's servers, not
			// everybody's. Getting this backwards would archive the whole
			// platform under a configuration that reads as one user's.
			return nil, nil
		}
		for _, s := range all {
			if s.OwnerID == *sel.OwnerID {
				picked = append(picked, s)
			}
		}
		return picked, nil

	case models.PlatformBackupServersList:
		byID := make(map[int]models.BackupTargetServer, len(all))
		for _, s := range all {
			byID[s.ID] = s
		}
		seen := make(map[int]bool, len(sel.ServerIDs))
		for _, id := range sel.ServerIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			if s, ok := byID[id]; ok {
				picked = append(picked, s)
				continue
			}
			missing = append(missing, id)
		}
		return picked, missing
	}

	// "none", and anything a future version wrote that this one does not know.
	// Selecting nothing is the safe reading of an unknown mode: the alternative
	// is archiving servers on the strength of a word we cannot interpret.
	return nil, nil
}

// SkippedServerComponents renders the ids a selection named and could not find
// as run components, so a completed run says which servers were not in it.
func SkippedServerComponents(missing []int) []models.PlatformBackupComponent {
	out := make([]models.PlatformBackupComponent, 0, len(missing))
	for _, id := range missing {
		out = append(out, models.PlatformBackupComponent{
			Kind:    "server",
			Ref:     strconv.Itoa(id),
			Status:  models.PlatformBackupSkipped,
			Message: "the selected server no longer exists",
		})
	}
	return out
}
