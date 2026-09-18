package handlers

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"path"
	"strings"
	"time"

	"dylaris-core/models"
	"dylaris-core/services"
)

// mrpackSnapshotMaxBytes caps the .mrpack we pull just to read its manifest.
// The archive is small relative to the mods it references; 200 MiB matches the
// node's maxMrpackSize ceiling and guards against a hostile upstream.
const mrpackSnapshotMaxBytes = 200 << 20

// snapshotModpackContents captures the modpack's Modrinth-identified members for
// the Content-tab cross-check, keyed by (serverID, subServer). It branches on
// the ORIGINAL installer type (captured before the pack->modpack rewrite in
// SetupServer):
//   - "pack":    ListBuildContent(packBuild.ID), keep entries with a project id.
//   - "modpack": fetch mrpackURL, ParseMrpackContents.
//   - anything else (vanilla/loader/upload): clear any stale snapshot.
//
// Always non-fatal: any failure is logged and swallowed so a snapshot problem
// can never block a server install/reinstall (spec 5.3, 7).
func (h *ServerHandler) snapshotModpackContents(serverID int, subServer, originalInstallerType string, packBuild *models.PackBuild, mrpackURL string) {
	switch originalInstallerType {
	case "pack":
		if packBuild == nil {
			log.Printf("modpack snapshot: pack install for server %d has no build; skipping", serverID)
			return
		}
		content, err := h.state.Store.ListBuildContent(packBuild.ID)
		if err != nil {
			log.Printf("modpack snapshot: ListBuildContent(%d) failed: %v", packBuild.ID, err)
			return
		}
		rows := rowsFromBuildContent(serverID, subServer, content)
		if err := h.state.Store.ReplaceServerModpackContents(serverID, subServer, rows); err != nil {
			log.Printf("modpack snapshot write for server %d sub %q failed: %v", serverID, subServer, err)
		}
	case "modpack":
		data, err := h.fetchMrpackForSnapshot(mrpackURL)
		if err != nil {
			log.Printf("modpack snapshot: fetch %q failed: %v", mrpackURL, err)
			return
		}
		entries, err := services.ParseMrpackContents(data)
		if err != nil {
			log.Printf("modpack snapshot: parse %q failed: %v", mrpackURL, err)
			return
		}
		rows := rowsFromMrpackEntries(serverID, subServer, entries)
		if err := h.state.Store.ReplaceServerModpackContents(serverID, subServer, rows); err != nil {
			log.Printf("modpack snapshot write for server %d sub %q failed: %v", serverID, subServer, err)
		}
	default:
		// Not a modpack server (vanilla/loader/upload): clear any stale snapshot
		// so a reinstall away from a modpack silences the cross-check.
		if err := h.state.Store.ReplaceServerModpackContents(serverID, subServer, nil); err != nil {
			log.Printf("modpack snapshot clear for server %d sub %q failed: %v", serverID, subServer, err)
		}
	}
}

// rowsFromBuildContent maps builder-pack contents to snapshot rows, keeping only
// Modrinth-identified members (a plain uploaded jar has no project id and is not
// cross-checkable).
func rowsFromBuildContent(serverID int, subServer string, content []models.BuildContentEntry) []models.ServerModpackContent {
	rows := make([]models.ServerModpackContent, 0, len(content))
	for _, e := range content {
		pid := strings.TrimSpace(e.ModrinthProjectID)
		if pid == "" {
			continue
		}
		rows = append(rows, models.ServerModpackContent{
			ServerID:              serverID,
			SubServerName:         subServer,
			ModrinthProjectID:     pid,
			ModrinthVersionID:     strings.TrimSpace(e.ModrinthVersionID),
			ModrinthVersionNumber: strings.TrimSpace(e.ModrinthVersionNumber),
			FileName:              path.Base(e.TargetPath),
			Side:                  normalizeSide(e.Side),
		})
	}
	return rows
}

// rowsFromMrpackEntries maps parsed .mrpack entries to snapshot rows. The
// manifest carries no version number (only the version id in the CDN URL), so
// ModrinthVersionNumber is left empty; the cross-check keys on the version id.
func rowsFromMrpackEntries(serverID int, subServer string, entries []services.MrpackEntry) []models.ServerModpackContent {
	rows := make([]models.ServerModpackContent, 0, len(entries))
	for _, e := range entries {
		pid := strings.TrimSpace(e.ProjectID)
		if pid == "" {
			continue
		}
		rows = append(rows, models.ServerModpackContent{
			ServerID:          serverID,
			SubServerName:     subServer,
			ModrinthProjectID: pid,
			ModrinthVersionID: strings.TrimSpace(e.VersionID),
			FileName:          e.FileName,
			Side:              normalizeSide(e.Side),
		})
	}
	return rows
}

// normalizeSide coerces a side string to one of the canonical values, defaulting
// unknown/empty to "both".
func normalizeSide(side string) string {
	switch side {
	case models.SideClient, models.SideServer, models.SideBoth:
		return side
	default:
		return models.SideBoth
	}
}

// fetchMrpackForSnapshot pulls an external .mrpack for the snapshot through the
// hardened services.SafeFetch (SSRF-safe, size-capped, timeout-bounded). The
// host is pre-restricted to cdn.modrinth.com or this Core's own pack mirror host;
// every other host is refused.
func (h *ServerHandler) fetchMrpackForSnapshot(rawURL string) ([]byte, error) {
	if !h.isSnapshotFetchHostAllowed(rawURL) {
		return nil, fmt.Errorf("host not allowlisted for snapshot fetch: %q", rawURL)
	}
	return services.SafeFetch(context.Background(), rawURL, mrpackSnapshotMaxBytes, 30*time.Second)
}

// isSnapshotFetchHostAllowed permits cdn.modrinth.com and this Core's own pack
// mirror host (a rendered pack .mrpack lives there).
func (h *ServerHandler) isSnapshotFetchHostAllowed(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "cdn.modrinth.com" {
		return true
	}
	if base, err := modpackMirrorBase(h.state.Store.GetSetting); err == nil {
		if mu, err := url.Parse(base); err == nil && strings.EqualFold(mu.Hostname(), host) {
			return true
		}
	}
	return false
}
