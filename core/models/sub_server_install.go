package models

import "time"

// SubServerInstall is how one sub-server was installed: which installer, which
// versions, and - for a modpack or a pack - exactly which one.
//
// It exists because the setup request has always carried the Modrinth and pack
// references and nothing ever stored them. They were handed to the node and
// dropped, so the panel could not tell an operator which modpack a sub-server
// runs, and "edit this sub-server" could not put the modpack back in the form.
// Changing a JVM flag meant re-picking the pack from memory and hoping.
//
// One row per (ServerID, SubServerName). The servers row carries the same three
// version columns, but only ever for whichever sub-server is active - which is
// the wrong answer the moment there are two.
type SubServerInstall struct {
	ServerID      int    `json:"-"`
	SubServerName string `json:"subServerName"`

	// InstallerType is the installer as the panel names it: paper, vanilla,
	// fabric, forge, neoforge, upload, upload-zip, modpack, pack. Rows written
	// before the Library was removed may still say library.
	InstallerType string `json:"installerType"`
	McVersion     string `json:"mcVersion"`
	BuildVersion  string `json:"buildVersion"`
	Loader        string `json:"loader,omitempty"`

	// The Modrinth modpack this sub-server boots from, when it is one.
	ModrinthProjectID   string `json:"modrinthProjectId,omitempty"`
	ModrinthVersionID   string `json:"modrinthVersionId,omitempty"`
	ModrinthProjectSlug string `json:"modrinthProjectSlug,omitempty"`

	// The in-house pack + build, when the install came from the pack builder.
	// Zero means "not a pack install"; the two are written and read together.
	PackID      int `json:"packId,omitempty"`
	PackBuildID int `json:"packBuildId,omitempty"`

	InstalledAt time.Time `json:"installedAt"`
}

// IsModpack reports whether this install came from a Modrinth modpack.
func (i SubServerInstall) IsModpack() bool { return i.ModrinthProjectID != "" }

// IsPack reports whether this install came from the in-house pack builder.
func (i SubServerInstall) IsPack() bool { return i.PackID > 0 }
