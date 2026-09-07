package models

import (
	"encoding/json"
	"time"
)

// BackupStorage is a target for backup archives: either the platform's own,
// configured by an admin, or one a tenant connected for themselves.
type BackupStorage struct {
	ID       int             `json:"id"`
	Name     string          `json:"name"`
	Provider string          `json:"provider"` // "local"|"shared" | "s3" | "node-local" | "core-storage"
	Config   json.RawMessage `json:"config"`
	// OwnerID is nil for the platform's own storages, which is what every row
	// was before tenants could bring their own. A tenant's storage is theirs:
	// only they may target it, we do not pay for what it holds, and it is
	// therefore outside their backup quota.
	OwnerID *string `json:"ownerId,omitempty"`
	// IsDefault is the default WITHIN its scope: the platform default when
	// OwnerID is nil, that tenant's own default otherwise. A tenant's default
	// wins over the platform's for their jobs.
	IsDefault bool      `json:"isDefault"`
	CreatedAt time.Time `json:"createdAt"`

	// SecretSet reports, on a read response, that an s3 secret is stored
	// WITHOUT returning it. It is transient: set by the store on read from the
	// encrypted secret_enc column (or a legacy plaintext secret still in
	// config), never persisted. The secret itself lives encrypted in secret_enc
	// and is stripped from Config on the list path, so a settings.read holder
	// can no longer harvest every backup credential over the API.
	SecretSet bool `json:"secretSet,omitempty"`
}

// BackupJob describes a recurring or manual backup configuration.
// Schedule is a free-form string; "manual" disables auto-runs.
// Otherwise expected formats: "every Nh", "every Nd" (parsed by scheduler).
type BackupJob struct {
	ID              int        `json:"id"`
	ServerID        int        `json:"serverId"`
	SubServer       *string    `json:"subServer,omitempty"` // NULL = full container
	Name            string     `json:"name"`
	Schedule        string     `json:"schedule"`
	IncludePatterns []string   `json:"includePatterns"`
	ExcludePatterns []string   `json:"excludePatterns"`
	RetentionCount  int        `json:"retentionCount"`
	StorageID       *int       `json:"storageId,omitempty"`
	Enabled         bool       `json:"enabled"`
	LastRunAt       *time.Time `json:"lastRunAt,omitempty"`
	NextRunAt       *time.Time `json:"nextRunAt,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
}

// BackupRun records a single execution (in-progress or completed).
type BackupRun struct {
	ID           int        `json:"id"`
	JobID        int        `json:"jobId"`
	StartedAt    time.Time  `json:"startedAt"`
	CompletedAt  *time.Time `json:"completedAt,omitempty"`
	Status       string     `json:"status"` // running | success | failed
	SizeBytes    int64      `json:"sizeBytes"`
	StorageKey   string     `json:"storageKey"`
	ErrorMessage string     `json:"errorMessage"`
	// StorageID is where the archive actually went, recorded when the run
	// started. nil on runs from before it was recorded, and on runs whose
	// storage has since been deleted; both are read as the platform's own,
	// which is the safe direction for a quota.
	StorageID *int `json:"storageId,omitempty"`
	// InstallSnapshot is the sub-server install records this archive was taken
	// from, as JSON. Empty for runs from before it was captured, and for runs
	// that failed - a restore then leaves the records alone, which is the
	// honest answer when nobody wrote down what was there.
	InstallSnapshot string `json:"-"`
	// Manifest is what this archive contains, described: the install records
	// AND the installed-mod rows, as JSON. The same bytes are inside the
	// archive, so a downloaded archive describes itself on a foreign platform
	// while a same-instance restore reads this copy and fetches nothing.
	//
	// Empty for every run written before manifests existed, and for failed
	// runs. A restore then falls back to InstallSnapshot and leaves the mod
	// rows alone, which is the honest answer when nobody wrote down what was
	// there.
	Manifest string `json:"-"`
}

// BackupRestore records a restore attempt against an archived BackupRun.
type BackupRestore struct {
	ID           int        `json:"id"`
	RunID        int        `json:"runId"`
	ServerID     int        `json:"serverId"`
	RequestedBy  *string    `json:"requestedBy,omitempty"`
	RequestedAt  time.Time  `json:"requestedAt"`
	CompletedAt  *time.Time `json:"completedAt,omitempty"`
	Status       string     `json:"status"` // queued | running | success | failed
	ErrorMessage string     `json:"errorMessage"`

	// Stalled is set at response time (not a DB column) when this restore is
	// still queued and the node that would run it is not there. The status stays
	// what it is: the job sits on the node's durable stream and really does
	// resume, so calling it failed would be wrong in the common case of a node
	// rebooting. Same treatment, and the same reasoning, as Server.InstallStalled.
	Stalled     bool   `json:"stalled,omitempty"`
	StallReason string `json:"stallReason,omitempty"`
}
