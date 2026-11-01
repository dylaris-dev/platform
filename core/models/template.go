package models

import "time"

// Template is a user-owned starting point for a server or proxy.
// StorageConnectionID 0 means Core's own storage.
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
