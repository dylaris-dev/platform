package nodeops

import (
	"context"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-pkg/release"

	"github.com/redis/go-redis/v9"
)

// ModReportingSince is the release in which the node started reporting the
// outcome of an install. A node older than this performs the install and says
// nothing, so a row waiting for its answer would wait forever.
//
// Written as a constant rather than derived from the running Core's own
// RELEASE_VERSION: that value is empty in a development build, and deriving it
// would make every node look too old exactly where the feature is being worked
// on.
const ModReportingSince = "2026.09.02.8"

// InstallStatusFor decides whether an install may be recorded as PENDING, or
// has to be recorded the way it always was - as a fact at dispatch.
//
// Core and the node are deployed separately, so "new Core, old node" is not an
// edge case, it is a window every operator passes through. In that window the
// node installs the mod correctly and reports nothing, and a row left at
// "installing" would stay there for the life of the install with no way to
// clear it. The panel would show every mod on that server as pending.
//
// An unknown version counts as old. That is the safe direction and it is not
// lossy: the row is written as installed, and if a report does arrive it still
// carries the attempt id, so it is applied and corrects the row. Guessing the
// other way cannot be corrected by anything.
func InstallStatusFor(ctx context.Context, rdb *redis.Client, nodeToken string) string {
	if rdb == nil {
		return models.ServerModInstalled
	}
	// The singular loader: the plural one is a KEYS scan over the whole fleet,
	// and this runs on every install.
	hb := services.LoadHeartbeat(ctx, rdb, nodeToken)
	if hb == nil {
		return models.ServerModInstalled
	}
	have, err := release.ParseVersion(hb.ReleaseVersion)
	if err != nil {
		return models.ServerModInstalled
	}
	since, err := release.ParseVersion(ModReportingSince)
	if err != nil {
		return models.ServerModInstalled
	}
	if have.Compare(since) < 0 {
		return models.ServerModInstalled
	}
	return models.ServerModInstalling
}
