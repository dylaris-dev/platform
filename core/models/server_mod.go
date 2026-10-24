package models

import "time"

// ServerMod is one installed mod/plugin on a server. We track Modrinth
// project + version IDs so the panel can compare against the latest version
// for "Update available" detection without re-hitting Modrinth on every
// page load (the cache layer on /api/modrinth/* covers re-checks).
type ServerMod struct {
	ID                  int    `json:"id"`
	ServerID            int    `json:"serverId"`
	SubServerName       string `json:"subServerName"`
	ModrinthProjectID   string `json:"modrinthProjectId"`
	ModrinthProjectSlug string `json:"modrinthProjectSlug"`
	ModrinthVersionID   string `json:"modrinthVersionId"`
	Title               string `json:"title"`
	FileName            string `json:"fileName"`
	// TargetDir is the directory the jar was installed into ("mods"/"plugins").
	// Empty on rows written before the column existed; the uninstall path then
	// falls back to deriving it from the loader, which is how they were placed.
	TargetDir   string    `json:"targetDir"`
	SHA512      string    `json:"sha512"`
	InstalledAt time.Time `json:"installedAt"`
	InstalledBy *string   `json:"installedBy,omitempty"`
	// Status is what the NODE reported, not what Core hoped for: "installing"
	// until it answers, then "installed" or "failed". Rows written before this
	// existed read as "installed", which is what they were taken to be.
	Status string `json:"status"`
	// StatusMessage carries the node's reason for a failure. Empty otherwise.
	StatusMessage string `json:"statusMessage,omitempty"`
	// InstallID correlates one attempt with its report. A report naming an
	// attempt this row no longer holds is a late answer about a superseded
	// install and is dropped.
	InstallID string `json:"-"`
	// UpdatesPaused keeps the mod out of "Update all". It is a choice about
	// the bulk run only; updating this one mod by hand still works.
	UpdatesPaused bool `json:"updatesPaused"`
	// Disabled: the jar was moved to <TargetDir>/.disabled/ and is not loaded.
	Disabled bool `json:"disabled"`
}

// ServerModHistoryEntry is a version this mod USED to be, kept so an update can
// be undone. Written when an install replaces a different version, newest
// first, three per project.
type ServerModHistoryEntry struct {
	ID                int       `json:"id"`
	ModrinthProjectID string    `json:"modrinthProjectId"`
	ModrinthVersionID string    `json:"modrinthVersionId"`
	Title             string    `json:"title"`
	FileName          string    `json:"fileName"`
	TargetDir         string    `json:"targetDir"`
	SHA512            string    `json:"sha512"`
	InstalledAt       time.Time `json:"installedAt"`
	ReplacedAt        time.Time `json:"replacedAt"`
}

// The three states a server_mods row can be in. An install is queued, so
// "installed" is something the NODE reports, never something Core assumes at
// dispatch.
const (
	ServerModInstalling = "installing"
	ServerModInstalled  = "installed"
	ServerModFailed     = "failed"
)
