package models

import "time"

// Template is a user-owned starting point for a server or proxy.
// StorageConnectionID 0 means Core's own storage. BackupStorageID, when set,
// names the owner's own s3 target instead and wins over StorageConnectionID.
type Template struct {
	ID                  int       `json:"id"`
	OwnerID             string    `json:"ownerId"`
	OwnerName           string    `json:"ownerName"` // read-only, joined from users
	Name                string    `json:"name"`
	Kind                string    `json:"kind"`
	Visibility          string    `json:"visibility"`
	Software            string    `json:"software"`
	McVersion           string    `json:"mcVersion"`
	SoftwareVersion     string    `json:"softwareVersion"`
	StorageConnectionID int       `json:"storageConnectionId"`
	BackupStorageID     *int      `json:"backupStorageId,omitempty"`
	SizeBytes           int64     `json:"sizeBytes"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

// TemplateShare grants one user use or edit access, either to one template
// (TemplateID set) or, as an owner-wide share, to all of an owner's templates.
type TemplateShare struct {
	TemplateID int       `json:"templateId"`
	UserID     string    `json:"userId"`
	Username   string    `json:"username"`
	Level      string    `json:"level"`
	CreatedAt  time.Time `json:"createdAt"`
}

// TemplateModrinthRef is a jar a template keeps as a Modrinth reference rather
// than a stored copy. Path is relative to the template (e.g. plugins/x.jar).
type TemplateModrinthRef struct {
	ID         int    `json:"id"`
	TemplateID int    `json:"templateId"`
	Path       string `json:"path"`
	ProjectID  string `json:"projectId"`
	VersionID  string `json:"versionId"`
	FileName   string `json:"fileName"` // Modrinth's name, display only: never a filesystem path (Path is)
	URL        string `json:"url"`
	Sha1       string `json:"sha1"`
	Sha512     string `json:"sha512"`
	SizeBytes  int64  `json:"sizeBytes"`
}
