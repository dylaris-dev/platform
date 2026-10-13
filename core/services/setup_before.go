package services

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/redis/go-redis/v9"
)

// Setup writes the server's setup columns - active sub-server, loader, version,
// image, flags - before the command goes out, because the node needs nothing
// back to start from them. When the install then FAILED, nothing put them back:
// Core showed the new sub-server and loader while the node had not switched, a
// start ran the old sub-server, and the Content tab offered mods for a loader
// the server did not run. The columns as they were are kept here for as long
// as an install can take, and a failure report restores them.
const setupBeforeTTL = 2 * time.Hour

func setupBeforeKey(uuid string) string { return "dylaris:server:" + uuid + ":setup_before" }

type setupBefore struct {
	Target                                                     string
	Image, Command, Active, Flags, Installer, MCVersion, Build string
}

// RememberSetupBefore records srv's setup columns before a setup to target is
// dispatched. Best-effort: without it a failure leaves the columns as today.
func RememberSetupBefore(ctx context.Context, rdb *redis.Client, srv *models.Server, target string) {
	if rdb == nil || srv == nil {
		return
	}
	b, err := json.Marshal(setupBefore{
		Target: target, Image: srv.GameImage, Command: srv.StartCommand, Active: srv.ActiveSubServer,
		Flags: srv.ExtraJvmFlags, Installer: srv.InstallerType, MCVersion: srv.MinecraftVersion, Build: srv.BuildNumber,
	})
	if err == nil {
		rdb.Set(ctx, setupBeforeKey(srv.UUID), b, setupBeforeTTL)
	}
}

// RestoreSetupBefore puts srv's setup columns back to what they were before the
// setup to target, if that is what was recorded, and reports whether it did.
func RestoreSetupBefore(ctx context.Context, rdb *redis.Client, st store.Store, srv *models.Server, target string) bool {
	if rdb == nil || srv == nil {
		return false
	}
	raw, err := rdb.Get(ctx, setupBeforeKey(srv.UUID)).Result()
	if err != nil {
		return false
	}
	var b setupBefore
	if json.Unmarshal([]byte(raw), &b) != nil || b.Target != target {
		return false
	}
	if err := st.UpdateServerSetup(srv.ID, b.Image, b.Command, b.Active, b.Flags, b.Installer, b.MCVersion, b.Build); err != nil {
		log.Printf("setup of %s failed, and its previous settings could not be restored: %v", srv.UUID, err)
		return false
	}
	rdb.Del(ctx, setupBeforeKey(srv.UUID))
	return true
}
