package services

import (
	"context"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/store"
	"fmt"
	"log"
	"time"
)

type ModrinthWatchChecker struct {
	Store  store.Store
	Cache  AvailabilityCache
	Leader leader.Election
}

func (c *ModrinthWatchChecker) Start(ctx context.Context) {
	go func() {
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if c.Leader == nil || c.Leader.IsLeader() {
					if err := c.RunOnce(ctx); err != nil {
						log.Printf("modrinth watch check: %v", err)
					}
				}
				timer.Reset(time.Minute)
			}
		}
	}()
}

func (c *ModrinthWatchChecker) RunOnce(ctx context.Context) error {
	watches, err := c.Store.ListModrinthWatches(ctx, "", true)
	if err != nil {
		return err
	}
	if len(watches) == 0 {
		return nil
	}
	all, err := FetchGameVersions(ctx)
	if err != nil {
		return err
	}
	releases := ReleaseVersions(all)
	for _, w := range watches {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if w.BuildID != nil {
			pack, err := c.Store.GetPack(w.PackID)
			if err != nil {
				return err
			}
			allowed := pack != nil && pack.OwnerID == w.UserID
			if pack != nil && !allowed {
				user, err := c.Store.GetUserByID(w.UserID)
				if err != nil {
					return err
				}
				if user != nil && user.IsAdmin {
					allowed = true
				} else {
					rights, err := c.Store.PackRightsFor(pack.OwnerID, w.UserID, pack.ID)
					if err != nil {
						return err
					}
					allowed = rights.Found
				}
			}
			if !allowed {
				if err := c.Store.DeleteModrinthWatch(ctx, w.ID, w.UserID); err != nil {
					return err
				}
				continue
			}
		}
		targets := WatchTargets(w, releases)
		items := make([]CompatItem, 0, len(w.Projects))
		for _, p := range w.Projects {
			items = append(items, CompatItem{Key: p.ProjectID, ProjectID: p.ProjectID, Title: p.Title, Side: CompatSideBoth})
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		matrix, checkErr := BuildMatrix(checkCtx, CompatRequest{Items: items, Loader: w.Loader, Current: w.Current, Targets: targets, Cache: c.Cache})
		cancel()
		versions := []models.ModrinthWatchVersion{}
		failure := ""
		if checkErr != nil {
			failure = "Modrinth availability could not be checked. Retrying in one hour."
		} else {
			for _, line := range matrix.Lines {
				for _, row := range line.Versions {
					missing := []string{}
					for _, m := range row.Missing {
						missing = append(missing, m.Title)
					}
					versions = append(versions, models.ModrinthWatchVersion{Minecraft: row.Minecraft, Available: matrix.Checked - len(row.Missing), Missing: missing})
				}
			}
		}
		if err := c.Store.CompleteModrinthWatch(ctx, w, versions, failure); err != nil {
			return fmt.Errorf("save watch result: %w", err)
		}
	}
	return nil
}

// Ignore snapshots and already delivered releases. Each run is bounded; older
// unfulfilled targets remain due on later days, rather than flooding Modrinth.
func WatchTargets(w models.ModrinthWatch, releases []string) []string {
	seen := map[string]bool{}
	for _, v := range w.Delivered {
		seen[v] = true
	}
	wanted := map[string]bool{}
	for _, v := range w.Targets {
		wanted[v] = true
	}
	if w.Future {
		for _, v := range SelectCompatTargets(releases, w.Current, CompatModeAllNewer, "") {
			wanted[v] = true
		}
	}
	out := []string{}
	for _, v := range releases {
		if wanted[v] && !seen[v] {
			out = append(out, v)
		}
	}
	if len(out) > 32 {
		// Rotate the bounded window each day so an old missing release cannot
		// be starved indefinitely by newer future targets.
		offset := int((w.NextCheck.Unix() / 86400) % int64(len(out)))
		if offset < 0 {
			offset = 0
		}
		out = append(out[offset:], out[:offset]...)[:32]
	}
	return out
}
