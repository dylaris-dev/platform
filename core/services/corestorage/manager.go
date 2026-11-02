// Package corestorage is Core file storage: the shared config and where it is
// persisted, provider construction (host path, s3, a saved storage connection,
// a tenant's own bucket for templates), the cross-Core reachability round, and
// the retention and purge jobs over what is stored there. It holds no HTTP
// handlers; the routes and their responses live in handlers/.
package corestorage

import (
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/services"
	"dylaris-core/services/storagereach"
	"dylaris-core/storage"
	"dylaris-core/store"
)

// Manager is the one Core file storage of this process. main.go builds it ONCE
// and every handler and leader worker shares that pointer: reachRoundMu is the
// single-process half of the config-round lock, and a second Manager would be a
// second lock (spec R7). Tests build their own literal.
type Manager struct {
	Store store.Store
	Redis *redis.Client
	// Events carries a reach round's live progress to the panel. Nil-safe.
	Events *services.SystemEventsPublisher
	// Registry is the node registry the backup purge hands to the node-local
	// backup provider.
	Registry *nodegrpc.Registry
	// ClusterSecret derives the rendered-pack storage keys (MrpackStorageKey).
	ClusterSecret string

	// Gate is the watchdog for the configured host-path core storage.
	// SyncGate points it at the current path (or stops it when the backend is
	// s3), and every per-request provider build consults it. Every Gate method
	// is nil-safe, so a Manager built without one - which is only ever a test -
	// simply never gates.
	Gate *storage.Gate

	// S3 is the connection state and retry loop for the configured s3 core
	// storage, the S3-side counterpart to Gate. It is long-lived because
	// providers are rebuilt per request, so state kept on a provider would
	// reset before anything could read it. Every method is nil-safe, so a
	// Manager built without one - only ever a test - simply never pauses.
	S3 *storage.S3Resilience

	// Reach proves this Core can actually reach the SHARED core file storage,
	// and gates this Core's storage routes when it cannot. Nil in tooling and
	// tests, which the gate treats as "not enabled" rather than "deny".
	Reach *storagereach.Service

	// reachRoundDeadline overrides the config-round cap. Zero means the
	// default; it exists so tests do not wait 15s per case.
	reachRoundDeadline time.Duration

	// reachRoundPoll overrides the round's poll cadence, which is also how
	// often the coordinator retries its OWN storage listing. Zero means the
	// production default (storagereach's 1s). It exists because that cadence
	// decides how many chances the coordinator gets to observe a peer's beacon
	// before the deadline: at 1s a short test deadline buys one or two listings
	// and the positive-control test then depends on a peer goroutine being
	// scheduled inside a single second, which held locally and flaked on CI.
	// Production keeps 1s - a config save is interactive, and polling shared
	// storage faster than that buys nothing.
	reachRoundPoll time.Duration

	// reachRoundMu is the single-process half of the config-round lock that
	// CheckSharedReachable takes before calling RunRound: the Redis SETNX lock
	// alongside it is the cluster-wide half. Zero value is a ready to use,
	// unlocked mutex, so no constructor is needed.
	reachRoundMu sync.Mutex

	// modpackStorage caches "does modpack storage resolve to a provider", which
	// GET /api/system/features reports to every user at boot. See
	// modpack_state.go for why that call must not run per request.
	modpackStorage modpackStorageState
}

// SetReachRoundDeadline and SetReachRoundPoll shorten the shared-storage
// verification round for tests (see the fields above). Production never calls
// them.
func (s *Manager) SetReachRoundDeadline(d time.Duration) { s.reachRoundDeadline = d }

func (s *Manager) SetReachRoundPoll(d time.Duration) { s.reachRoundPoll = d }
