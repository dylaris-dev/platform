package handlers

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// maxCachedResponseBytes caps what one cache entry may weigh.
//
// Measured against the live API: a project's version list runs 290 KB for
// Sodium filtered to one loader, 494 KB unfiltered, and 1.19 MB for Fabric API.
// The proxy keys those per filter combination for an hour and the shipped Redis
// runs with no maxmemory, so without a cap the tail of that distribution is what
// decides how much memory the control plane has left. Over the cap the response
// is served straight through: one extra upstream call, no stored megabyte.
const maxCachedResponseBytes = 512 << 10
