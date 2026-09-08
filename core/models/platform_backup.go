package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// A platform backup is a SELECTION, not a fixed bundle.
//
// The operator decides per run what goes in. That is not a convenience: a
// bundle containing every server's world is the sum of all of them, written in
// full on every run against one destination with one retention policy, and
// server backups already have per-job schedules, per-owner quotas and per-owner
// destinations of their own. Making everything mandatory would mean either
// ignoring all of that or implementing it twice.

// PlatformBackupServerMode is HOW servers are chosen. Servers are always taken
// whole - never per sub-server - because a platform bundle is about restoring
// an installation, and half a server is not a thing anyone restores.
type PlatformBackupServerMode string

const (
	// PlatformBackupServersNone takes no servers at all, which is the sensible
	// default: the database is small and quick, the worlds are neither.
	PlatformBackupServersNone PlatformBackupServerMode = "none"
	// PlatformBackupServersList takes exactly the servers named.
	PlatformBackupServersList PlatformBackupServerMode = "list"
	// PlatformBackupServersOwner takes every server belonging to one user,
	// including ones created after the selection was saved.
	PlatformBackupServersOwner PlatformBackupServerMode = "owner"
	// PlatformBackupServersAll takes every server on the platform.
	PlatformBackupServersAll PlatformBackupServerMode = "all"
	// PlatformBackupServersBYON takes every server that sits on hardware a
	// customer owns. Offered only on a store-connected platform: a self-hoster
	// has no BYON tenants to tell apart, so the option would be a filter over
	// an empty distinction.
	PlatformBackupServersBYON PlatformBackupServerMode = "byon"
)

// PlatformBackupServers is the server half of a selection.
type PlatformBackupServers struct {
	Mode PlatformBackupServerMode `json:"mode"`
	// OwnerID is required by, and only read for, mode "owner".
	OwnerID *string `json:"ownerId,omitempty"`
	// ServerIDs is required by, and only read for, mode "list".
	//
	// A saved selection outlives the things it names - a server is deleted long
	// after somebody ticked it - so an id in here that no longer exists is an
	// ordinary outcome of a healthy run. It is skipped and recorded, never an
	// error, and it never fails the run around it.
	ServerIDs []int `json:"serverIds,omitempty"`
}

// PlatformBackupSelection is everything one run covers.
type PlatformBackupSelection struct {
	// Database is the platform's own Postgres: users, servers, jobs, settings,
	// and the encrypted credentials that make the rest of it usable.
	Database bool `json:"database"`
	// MetricsDB is the statistics database, which is a separate deployment and
	// is frequently not present at all.
	MetricsDB bool `json:"metricsDb"`
	// Library is core storage: the server jars, loaders and uploads.
	Library bool `json:"library"`
	// Modpacks is core storage: published packs and their builds.
	Modpacks bool                  `json:"modpacks"`
	Servers  PlatformBackupServers `json:"servers"`
}

// ErrEmptyPlatformBackupSelection is what an operator gets for a run that would
// archive nothing. It is caught here rather than producing a valid, empty
// bundle, because an empty bundle looks exactly like a successful backup.
var ErrEmptyPlatformBackupSelection = errors.New("a platform backup must include at least one component")

// ServerMode is the selection's server mode with absence read as "none".
//
// An ABSENT mode and a WRONG one are different things and must not be treated
// alike. A payload that omits the servers object at all, or a row written
// before the field existed, selects no servers - the forgiving reading, and the
// safe direction. A mode that is present but unrecognised is a mistake or a
// newer version's word, and is refused rather than silently read as none.
func (s PlatformBackupSelection) ServerMode() PlatformBackupServerMode {
	if s.Servers.Mode == "" {
		return PlatformBackupServersNone
	}
	return s.Servers.Mode
}

// Validate reports whether this selection describes a run that can be executed.
func (s PlatformBackupSelection) Validate() error {
	switch s.ServerMode() {
	case PlatformBackupServersNone, PlatformBackupServersAll, PlatformBackupServersBYON:
	case PlatformBackupServersOwner:
		if s.Servers.OwnerID == nil || *s.Servers.OwnerID == "" {
			return errors.New("selecting servers by owner requires an owner")
		}
	case PlatformBackupServersList:
		if len(s.Servers.ServerIDs) == 0 {
			return errors.New("selecting servers individually requires at least one server")
		}
	default:
		return fmt.Errorf("unknown server selection mode %q", s.Servers.Mode)
	}

	if !s.Database && !s.MetricsDB && !s.Library && !s.Modpacks &&
		s.ServerMode() == PlatformBackupServersNone {
		return ErrEmptyPlatformBackupSelection
	}
	return nil
}

// BackupTargetServer is the little a selection needs to know about a server.
// Deliberately not models.Server: choosing what to archive needs identity and
// ownership, and nothing about ports, images or run state.
type BackupTargetServer struct {
	ID      int    `json:"id"`
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	OwnerID string `json:"ownerId"`
	NodeID  int    `json:"nodeId"`
	// BYON is true when the NODE belongs to a customer. Ownership of the server
	// says who uses it; ownership of the node says whose hardware it runs on,
	// and those are different questions.
	BYON bool `json:"byon"`
}

// PlatformBackupJob is a configured platform backup.
type PlatformBackupJob struct {
	ID             int                     `json:"id"`
	Name           string                  `json:"name"`
	Schedule       string                  `json:"schedule"`
	Selection      PlatformBackupSelection `json:"selection"`
	StorageID      *int                    `json:"storageId,omitempty"`
	RetentionCount int                     `json:"retentionCount"`
	Enabled        bool                    `json:"enabled"`
	LastRunAt      *time.Time              `json:"lastRunAt,omitempty"`
	NextRunAt      *time.Time              `json:"nextRunAt,omitempty"`
	CreatedAt      time.Time               `json:"createdAt"`
}

// PlatformBackupComponentStatus is what became of one part of a run.
type PlatformBackupComponentStatus string

const (
	PlatformBackupIncluded PlatformBackupComponentStatus = "included"
	// PlatformBackupSkipped is a normal outcome, not a failure: a selected
	// server that has since been deleted, or a metrics database that is not
	// configured on this installation.
	PlatformBackupSkipped PlatformBackupComponentStatus = "skipped"
	PlatformBackupFailed  PlatformBackupComponentStatus = "failed"
)

// PlatformBackupComponent is one line of what a run actually did, which is not
// the same thing as what it was asked to do.
type PlatformBackupComponent struct {
	Kind      string                        `json:"kind"` // database | metrics | library | modpacks | server
	Ref       string                        `json:"ref,omitempty"`
	Status    PlatformBackupComponentStatus `json:"status"`
	SizeBytes int64                         `json:"sizeBytes,omitempty"`
	Message   string                        `json:"message,omitempty"`
}

// PlatformBackupRun records one execution.
type PlatformBackupRun struct {
	ID           int        `json:"id"`
	JobID        int        `json:"jobId"`
	StartedAt    time.Time  `json:"startedAt"`
	CompletedAt  *time.Time `json:"completedAt,omitempty"`
	Status       string     `json:"status"` // running | success | failed
	SizeBytes    int64      `json:"sizeBytes"`
	StorageKey   string     `json:"storageKey"`
	StorageID    *int       `json:"storageId,omitempty"`
	ErrorMessage string     `json:"errorMessage,omitempty"`
	// Components is what went in, per part, including the parts that did not.
	// A run with skipped servers is still a success, and the operator has to be
	// able to see WHICH ones without reading a log.
	Components []PlatformBackupComponent `json:"components"`
}

// EncodePlatformBackupComponents renders the component list for its column. A
// nil list becomes an empty array rather than "null", because a reader that
// unmarshals it must get a list either way.
func EncodePlatformBackupComponents(cs []PlatformBackupComponent) string {
	if cs == nil {
		cs = []PlatformBackupComponent{}
	}
	b, err := json.Marshal(cs)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// DecodePlatformBackupComponents reads the column back. Anything unreadable is
// an empty list: a run whose record cannot be parsed still happened, and
// failing the read would hide the run itself.
func DecodePlatformBackupComponents(raw string) []PlatformBackupComponent {
	if raw == "" {
		return nil
	}
	var cs []PlatformBackupComponent
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		return nil
	}
	return cs
}
