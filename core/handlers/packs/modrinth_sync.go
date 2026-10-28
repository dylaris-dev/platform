package packs

// Keeping imported packs current. A version published on Modrinth after the
// newest one a pack already has is imported automatically: by a background
// pass on the leader Core, and by the panel when someone opens the pack.
// Older missing versions are never pulled in automatically - a pack whose
// owner imported only the newest ten of hundreds stays that way until they
// sync by hand.

import (
	"context"
	"log"
	"sort"
	"time"

	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/services"
)

const (
	modrinthSyncFirstRun = 5 * time.Minute
	modrinthSyncInterval = 6 * time.Hour
	// Per pack and pass: a burst of releases is caught up over a few passes
	// rather than in one long run against Modrinth.
	modrinthSyncPerPack = 3
)

// newModrinthVersions are the project's versions published after the newest
// version this pack already has, oldest first. A pack with no Modrinth
// version yet has none: its first import is the owner's decision.
func newModrinthVersions(versions []services.ModrinthProjectVersion, builds []models.PackBuild) []services.ModrinthProjectVersion {
	have := map[string]bool{}
	var newest time.Time
	for _, b := range builds {
		if b.ModrinthVersionID == "" {
			continue
		}
		have[b.ModrinthVersionID] = true
		if b.PublishedAt != nil && b.PublishedAt.After(newest) {
			newest = *b.PublishedAt
		}
	}
	if len(have) == 0 {
		return nil
	}
	out := []services.ModrinthProjectVersion{}
	for _, v := range versions {
		if !have[v.ID] && v.DatePublished.After(newest) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DatePublished.Before(out[j].DatePublished) })
	return out
}

// StartModrinthSync runs the background pass on the leader only, so two Core
// replicas never import the same version side by side.
func (h *PacksHandler) StartModrinthSync(ctx context.Context, election leader.Election) {
	go func() {
		timer := time.NewTimer(modrinthSyncFirstRun)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if election == nil || election.IsLeader() {
					h.syncModrinthPacks(ctx)
				}
				timer.Reset(modrinthSyncInterval)
			}
		}
	}()
}

func (h *PacksHandler) syncModrinthPacks(ctx context.Context) {
	if h.pat == nil {
		return
	}
	packs, err := h.state.Store.ListModrinthBoundPacks()
	if err != nil {
		log.Printf("modrinth sync: list packs: %v", err)
		return
	}
	prov, err := h.state.BuildModpackStorageProvider()
	if err != nil || prov == nil {
		return
	}
	for i := range packs {
		if ctx.Err() != nil {
			return
		}
		pack := &packs[i]
		pat, _, err := h.pat.LoadPAT(pack.OwnerID)
		if err != nil {
			continue // owner disconnected Modrinth: nothing to sync with
		}
		mc := services.NewModrinthClient(pat)
		versions, err := mc.ProjectVersions(ctx, pack.ModrinthProjectID)
		if err != nil {
			log.Printf("modrinth sync: pack %d: versions: %v", pack.ID, err)
			continue
		}
		builds, err := h.state.Store.ListPackBuilds(pack.ID)
		if err != nil {
			continue
		}
		names := map[string]bool{}
		for _, b := range builds {
			names[b.VersionString] = true
		}
		todo := newModrinthVersions(versions, builds)
		if len(todo) > modrinthSyncPerPack {
			todo = todo[:modrinthSyncPerPack]
		}
		for j := range todo {
			res, ierr := h.importVersion(ctx, prov, pack, &todo[j], names)
			if ierr != nil {
				log.Printf("modrinth sync: pack %d version %s: %s", pack.ID, todo[j].VersionNumber, ierr.msg)
				break // the next pass retries; later versions keep their order
			}
			names[todo[j].VersionNumber] = true
			log.Printf("modrinth sync: pack %d imported %s as build %d", pack.ID, todo[j].VersionNumber, res.BuildID)
			if h.state.Events != nil {
				h.state.Events.Publish(ctx, "pack_builds.changed", map[string]interface{}{"packId": pack.ID, "ownerId": pack.OwnerID})
			}
		}
	}
}
