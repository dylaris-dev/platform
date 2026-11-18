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
	// The target's install record, which the edit form and the mod checks
	// prefer over the servers row; Install nil with InstallKnown means there
	// was none. A record written before these fields existed restores only
	// the columns.
	InstallKnown bool                     `json:",omitempty"`
	Install      *models.SubServerInstall `json:",omitempty"`
	// The run intent before the dispatch flipped it to online (IntendOnline).
	// Empty in a record from before the field: the intent is then left alone.
	DesiredState string `json:",omitempty"`
	// The target's mod rows before the setup dropped them (RememberSetupMods).
	// Absent unless the setup changes them.
	ModsKnown bool               `json:",omitempty"`
	Mods      []models.ServerMod `json:",omitempty"`
}

// RememberSetupBefore records srv's setup columns and target's install record
// before a setup or reinstall to target is dispatched. Best-effort: without it
// a failure leaves them as today.
func RememberSetupBefore(ctx context.Context, rdb *redis.Client, st store.Store, srv *models.Server, target string) {
	if rdb == nil || srv == nil {
		return
	}
	before := setupBefore{
		Target: target, Image: srv.GameImage, Command: srv.StartCommand, Active: srv.ActiveSubServer,
		Flags: srv.ExtraJvmFlags, Installer: srv.InstallerType, MCVersion: srv.MinecraftVersion, Build: srv.BuildNumber,
		DesiredState: srv.DesiredState,
	}
	if st != nil {
		if in, err := st.GetSubServerInstall(srv.ID, target); err == nil {
			before.InstallKnown, before.Install = true, in
		}
	}
	b, err := json.Marshal(before)
	if err == nil {
		rdb.Set(ctx, setupBeforeKey(srv.UUID), b, setupBeforeTTL)
	}
}

// RememberSetupMods adds target's mod rows to the record RememberSetupBefore
// wrote, for a setup that is about to drop or rewrite them. A node that puts
// the files back after a failure says so, and RestoreSetupMods then puts the
// rows back to match. Reports whether they were recorded.
func RememberSetupMods(ctx context.Context, rdb *redis.Client, st store.Store, srv *models.Server, target string) bool {
	if rdb == nil || st == nil || srv == nil {
		return false
	}
	b, ok := readSetupBefore(ctx, rdb, srv, target)
	if !ok {
		return false
	}
	mods, err := st.ListServerMods(srv.ID, target)
	if err != nil {
		log.Printf("setup of %s: could not record the mod rows of %s: %v", srv.UUID, target, err)
		return false
	}
	b.ModsKnown, b.Mods = true, mods
	raw, err := json.Marshal(b)
	return err == nil && rdb.Set(ctx, setupBeforeKey(srv.UUID), raw, setupBeforeTTL).Err() == nil
}

// RestoreSetupMods puts target's mod rows back as RememberSetupMods recorded
// them, for a failed setup the node reports left the files as they were. It
// leaves the record for RestoreSetupBefore and reports whether it restored.
func RestoreSetupMods(ctx context.Context, rdb *redis.Client, st store.Store, srv *models.Server, target string) bool {
	if rdb == nil || st == nil || srv == nil {
		return false
	}
	b, ok := readSetupBefore(ctx, rdb, srv, target)
	if !ok || !b.ModsKnown {
		return false
	}
	if err := st.ReplaceServerMods(srv.ID, target, b.Mods); err != nil {
		log.Printf("setup of %s failed, and the mod rows of %s could not be restored: %v", srv.UUID, target, err)
		return false
	}
	// ReplaceServerMods carries a pin over only from rows still there, and the
	// setup had dropped them.
	paused := map[string]bool{}
	for _, m := range b.Mods {
		if m.UpdatesPaused {
			paused[m.ModrinthProjectID] = true
		}
	}
	if len(paused) > 0 {
		if rows, err := st.ListServerMods(srv.ID, target); err == nil {
			for _, m := range rows {
				if paused[m.ModrinthProjectID] && !m.UpdatesPaused {
					st.SetServerModUpdatesPaused(m.ID, srv.ID, true)
				}
			}
		}
	}
	return true
}

func readSetupBefore(ctx context.Context, rdb *redis.Client, srv *models.Server, target string) (setupBefore, bool) {
	var b setupBefore
	raw, err := rdb.Get(ctx, setupBeforeKey(srv.UUID)).Result()
	if err != nil || json.Unmarshal([]byte(raw), &b) != nil || b.Target != target {
		return b, false
	}
	return b, true
}

// RestoreSetupBefore puts srv's setup columns back to what they were before the
// setup to target, if that is what was recorded, and reports whether it did.
// srv is the server as it is now: its desired state is what the dispatch left.
func RestoreSetupBefore(ctx context.Context, rdb *redis.Client, st store.Store, srv *models.Server, target string) bool {
	if rdb == nil || srv == nil {
		return false
	}
	b, ok := readSetupBefore(ctx, rdb, srv, target)
	if !ok {
		return false
	}
	if err := st.UpdateServerSetup(srv.ID, b.Image, b.Command, b.Active, b.Flags, b.Installer, b.MCVersion, b.Build); err != nil {
		log.Printf("setup of %s failed, and its previous settings could not be restored: %v", srv.UUID, err)
		return false
	}
	if b.InstallKnown {
		var ierr error
		if b.Install != nil {
			in := *b.Install
			in.ServerID = srv.ID // not carried by the JSON
			ierr = st.UpsertSubServerInstall(in)
		} else {
			ierr = st.DeleteSubServerInstall(srv.ID, target)
		}
		if ierr != nil {
			log.Printf("setup of %s failed, and the install record of %s could not be restored: %v", srv.UUID, target, ierr)
		}
	}
	// A failed or refused install ran nothing, so it must not leave the server
	// meant to run: the reconciler would start one its owner had stopped. Only
	// over the online the dispatch wrote, so a stop since is kept; a start the
	// owner issued meanwhile cannot be told apart from it and is undone too.
	if b.DesiredState != "" && b.DesiredState != "online" && srv.DesiredState == "online" {
		if err := st.UpdateServerDesiredState(srv.ID, b.DesiredState); err != nil {
			log.Printf("setup of %s failed, and its run intent could not be put back: %v", srv.UUID, err)
		} else {
			PublishDesiredState(ctx, rdb, srv.UUID, b.DesiredState)
		}
	}
	rdb.Del(ctx, setupBeforeKey(srv.UUID))
	return true
}
