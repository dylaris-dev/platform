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
		if w.BuildID != nil || w.FollowPack {
			allowed, err := watchPackReadable(c.Store, w)
			if err != nil {
				return err
			}
			if !allowed {
				if err = c.Store.DeleteModrinthWatch(ctx, w.ID, w.UserID); err != nil {
					return err
				}
				continue
			}
		}
		if w.FollowPack {
			if sourceErr := RefreshPackWatchSource(c.Store, &w); sourceErr != nil {
				if err := c.Store.CompleteModrinthWatch(ctx, w, nil, sourceErr.Error()); err != nil {
					return err
				}
				continue
			}
		}
		targets := WatchTargets(w, releases)
		items := make([]CompatItem, 0, len(w.Projects))
		for _, p := range w.Projects {
			items = append(items, watchCompatItem(p))
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
		if w.BuildID != nil || w.FollowPack {
			allowed, err := watchPackReadable(c.Store, w)
			if err != nil {
				return err
			}
			if !allowed {
				if err = c.Store.DeleteModrinthWatch(ctx, w.ID, w.UserID); err != nil {
					return err
				}
				continue
			}
		}
		if w.FollowPack {
			current := w
			if sourceErr := RefreshPackWatchSource(c.Store, &current); sourceErr != nil {
				if err := c.Store.CompleteModrinthWatch(ctx, current, nil, sourceErr.Error()); err != nil {
					return err
				}
				continue
			}
			if !sameWatchSource(w, current) {
				continue
			}
			w.Title = current.Title
			w.Unlinked = current.Unlinked
			w.CheckedBuildVersion = current.CheckedBuildVersion
			w.Projects = current.Projects
		}
		if err := c.Store.CompleteModrinthWatch(ctx, w, versions, failure); err != nil {
			return fmt.Errorf("save watch result: %w", err)
		}
	}
	return nil
}

// RefreshPackWatchSource captures the current recommendation at check time.
// Delivery rechecks its ID, so changing recommendations during an upstream
// lookup leaves the job due instead of announcing an obsolete source.
func RefreshPackWatchSource(st store.Store, w *models.ModrinthWatch) error {
	p, err := st.GetPack(w.PackID)
	if err != nil || p == nil {
		return fmt.Errorf("Could not read the pack recommendation. Retrying in one hour.")
	}
	builds, err := st.ListPackBuilds(p.ID)
	if err != nil {
		return fmt.Errorf("Could not read released builds. Retrying in one hour.")
	}
	b := models.RecommendedPackBuild(p, builds)
	w.Projects = []models.ModrinthWatchProject{}
	w.Unlinked = 0
	w.CheckedBuildID = 0
	w.CheckedBuildVersion = ""
	w.Title = p.InternalName
	if b == nil {
		return fmt.Errorf("No Recommended build or release yet. Choose a recommendation or publish a release.")
	}
	w.CheckedBuildID = b.ID
	w.CheckedBuildVersion = b.VersionString
	w.Title = p.InternalName + " / " + b.VersionString
	rows, err := st.ListBuildContent(b.ID)
	if err != nil {
		return fmt.Errorf("Could not read the recommended build. Retrying in one hour.")
	}
	seen := map[string]bool{}
	for _, e := range rows {
		if e.ContentType != models.ContentTypeMod {
			continue
		}
		if e.ModrinthProjectID == "" {
			w.Unlinked++
			continue
		}
		if !seen[e.ModrinthProjectID] {
			w.Projects = append(w.Projects, models.ModrinthWatchProject{ProjectID: e.ModrinthProjectID, Title: e.PrettyName})
			seen[e.ModrinthProjectID] = true
		}
	}
	if len(w.Projects) == 0 {
		return fmt.Errorf("The recommended build has no linked mods to check; local content needs manual review.")
	}
	if len(w.Projects) > 512 {
		return fmt.Errorf("The recommended build exceeds the 512 linked-mod alert limit.")
	}
	return nil
}

func watchCompatItem(p models.ModrinthWatchProject) CompatItem {
	if p.ContentType == "shader" {
		p.ContentType = models.ContentTypeShaderpack
	}
	return CompatItem{Key: p.ProjectID, ProjectID: p.ProjectID, Title: p.Title, ContentType: p.ContentType, Side: CompatSideBoth}
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

func watchPackReadable(st store.Store, w models.ModrinthWatch) (bool, error) {
	p, err := st.GetPack(w.PackID)
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, nil
	}
	if p.OwnerID == w.UserID {
		return true, nil
	}
	user, err := st.GetUserByID(w.UserID)
	if err != nil {
		return false, err
	}
	if user != nil && user.IsAdmin {
		return true, nil
	}
	rights, err := st.PackRightsFor(p.OwnerID, w.UserID, p.ID)
	return rights.Found, err
}

func sameWatchSource(a, b models.ModrinthWatch) bool {
	if a.CheckedBuildID != b.CheckedBuildID || len(a.Projects) != len(b.Projects) {
		return false
	}
	ids := map[string]bool{}
	for _, project := range a.Projects {
		ids[project.ProjectID] = true
	}
	for _, project := range b.Projects {
		if !ids[project.ProjectID] {
			return false
		}
	}
	return true
}
