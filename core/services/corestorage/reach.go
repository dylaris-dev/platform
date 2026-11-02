package corestorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-core/services"
	"dylaris-core/services/storagereach"
	"dylaris-core/storage"
)

// defaultReachRoundDeadline is the owner-chosen cap for a config round:
// snappy beats worst-case, and a genuinely shared but slow mount that misses
// it is not a permanent failure - Retry re-runs the round.
const defaultReachRoundDeadline = 15 * time.Second

// reachRoundLockKey and reachRoundLockTTL guard against two config rounds
// running at once, cluster-wide.
//
// RunRound's own discovery-key cleanup (round.go) deletes the single global
// "current round" key unconditionally, without checking that the key still
// points at ITS round. Two rounds in flight together - two admins saving near
// the same moment, or one admin's retry landing on a different Core replica
// behind the load balancer while the first attempt is still running - would
// let one coordinator delete the other's still-open round out from under it,
// turning a real, slow-but-honest peer into a false no-response. This lock
// stops a second round from starting anywhere in the fleet while one runs.
//
// Matches the SetNX-with-TTL cluster lock convention already used by
// RoutingMigrationService.Run, StorageMigrationService.Start and friends.
const (
	reachRoundLockKey = "dylaris:storagereach:save-lock"
	// reachRoundLockTTL outlives the round's own hard cap (15s) with margin, so
	// a Core that dies mid-round - before its deferred release ever runs -
	// does not wedge every future save behind a lock nobody will clear.
	reachRoundLockTTL = 20 * time.Second
)

// reachRoundBusyMessage is returned for either lock layer refusing an
// attempt - the local TryLock and the Redis SETNX alike - so a same-replica
// caller and a cross-replica caller see an identical refusal. From the
// outside "another round is running" is one fact, not two.
const reachRoundBusyMessage = "Another storage configuration change is currently being verified on this deployment. Wait for it to finish and try again."

// reachRoundLockReleaseScript frees the config-round lock only if it still
// holds the value THIS caller set with SetNX. A plain GET-then-DEL from Go
// would not be atomic across the two round trips, reopening the exact race
// the per-attempt token exists to close: a stale holder whose TTL just
// expired could still delete a lock a NEWER holder acquired in between the
// GET and the DEL. Same compare-and-delete idiom as pkg/leader/leader.go's
// lease release.
const reachRoundLockReleaseScript = `
if redis.call('get', KEYS[1]) == ARGV[1] then
	return redis.call('del', KEYS[1])
else
	return 0
end
`

// releaseReachRoundLock is the compare-and-delete half of the token above.
// Unexported, but called directly by this file's tests to simulate a stale
// holder's release racing a newer holder's lock without waiting out a real
// round.
func releaseReachRoundLock(ctx context.Context, rdb *redis.Client, token string) error {
	return rdb.Eval(ctx, reachRoundLockReleaseScript, []string{reachRoundLockKey}, token).Err()
}

// newReachRoundLockToken returns an unguessable per-attempt lock value - the
// same construction round.go's newRoundID uses for round ids.
func newReachRoundLockToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("core storage: config-round lock token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// SharedStorageRefusal is returned when a config must not be persisted because
// the fleet could not prove shared access. It carries the whole RoundResult,
// not just a message: the panel names the failing Core and its taxonomy
// reason, which is the difference between "it broke" and "fix host-2's mount".
type SharedStorageRefusal struct {
	Status  int
	Message string
	Result  *storagereach.RoundResult
}

func (e *SharedStorageRefusal) Error() string { return e.Message }

// ToReachConfig narrows the persisted settings shape to what the
// verifier needs. The s3 secret travels because each participant builds a real
// client from it; without it every peer would report unreachable.
//
// Exported (rather than left package-private) because main.go's boot wiring
// builds storagereach.ServiceDeps.ConfigFor from it, and that package cannot
// import handlers.
func ToReachConfig(cfg Config) storagereach.Config {
	return storagereach.Config{
		Backend:     cfg.Backend,
		Path:        cfg.Path,
		S3Endpoint:  cfg.S3Endpoint,
		S3Bucket:    cfg.S3Bucket,
		S3Region:    cfg.S3Region,
		S3AccessKey: cfg.S3AccessKey,
		S3SecretKey: cfg.S3SecretKey,
		S3PathStyle: cfg.S3PathStyle,
		S3Prefix:    cfg.S3Prefix,
	}
}

// NewReachProvider builds a provider rooted at the backend ROOT rather than a
// sub-prefix, so probe files land in .dylaris-reachability/ beside
// ticket-attachments/ and ticket-backups/ instead of inside one of them.
//
// Exported for the same reason as ToReachConfig: main.go wires this
// in directly as storagereach.ServiceDeps.NewProvider.
//
// nil gate, nil s3 resilience - the same RAW build every other candidate-config
// call site does (ProbeS3Connection, TestConnection, the migration target
// builder), for the reason NewProviderForConfig's own doc comment gives:
// a candidate probe must fail fast, and the live instances describe the LIVE
// backend rather than the one being tested. Passing them here broke three
// things at once:
//   - A mistyped s3 endpoint flipped the LIVE, working backend into
//     reconnecting, and the probe's next call then sat in the resilience
//     layer's ten-MINUTE retry budget while holding the config-round lock.
//   - A Core whose egress to the endpoint is firewalled - the flagship failure
//     this verifier exists to name - blocked in that same budget and never
//     wrote its report, so it was reported as no-response ("check its logs")
//     instead of unreachable ("check that it can reach the S3 endpoint").
//   - For a host path, a CANDIDATE path's failures marked the LIVE path
//     unhealthy, and a wedged live path refused to build a provider for a
//     DIFFERENT, healthy candidate - so an admin could not fix a broken path
//     config by pointing at a good one.
//
// Failing fast without the wrapper still holds: every S3Provider method threads
// the caller's ctx into aws-sdk-go-v2 (storage/s3provider.go), which aborts on
// it, and the round now hands the probe a context bounded by the round deadline.
func (s *Manager) NewReachProvider(cfg storagereach.Config) (storage.StorageProvider, error) {
	return NewProviderForConfig(Config{
		Backend:     cfg.Backend,
		Path:        cfg.Path,
		S3Endpoint:  cfg.S3Endpoint,
		S3Bucket:    cfg.S3Bucket,
		S3Region:    cfg.S3Region,
		S3AccessKey: cfg.S3AccessKey,
		S3SecretKey: cfg.S3SecretKey,
		S3PathStyle: cfg.S3PathStyle,
		S3Prefix:    cfg.S3Prefix,
	}, "", nil, nil)
}

// CheckSharedReachable replaces the count-only host-path guard with an
// actual proof. It runs a verifier round across every online Core and refuses
// the save unless all of them wrote, read each other, and cross-wrote.
//
// It covers s3 as well as host paths: the old guard ignored s3 entirely, but a
// Core holding credentials that do not work is exactly as broken as a Core on
// an unshared mount, and just as silent.
//
// A verification that could not run is a refusal, not a pass. This is a rare,
// deliberate admin action, so "could not verify, try again" is cheap; letting
// it through would silently split file storage across instances, which is the
// failure this exists to prevent.
func (s *Manager) CheckSharedReachable(ctx context.Context, cfg Config) error {
	online, err := services.OnlineCoreIDs(ctx, s.Redis)
	if err != nil {
		log.Printf("core storage: could not list online Cores: %v", err)
		return &SharedStorageRefusal{
			Status:  http.StatusServiceUnavailable,
			Message: "Could not verify which Core instances are online, so the storage settings cannot be saved right now. Check that Redis is reachable and try again.",
		}
	}
	// 0 and 1 are both "not more than one": a count of 0 means this Core's own
	// heartbeat has not landed yet, not that no Core is running. One Core
	// cannot fail a sharing check, so there is nothing to prove.
	if len(online) <= 1 {
		return nil
	}
	if s.Reach == nil {
		return &SharedStorageRefusal{
			Status:  http.StatusServiceUnavailable,
			Message: "The storage reachability verifier is not running on this Core, so a multi-Core storage change cannot be verified.",
		}
	}

	// Local mutex (single-process safety) plus a Redis SETNX lock
	// (cluster-wide safety) - the same two-layer pattern
	// RoutingMigrationService.Run uses, but with TryLock instead of Lock.
	//
	// RunRound can block forever: for a path backend it writes through a gated
	// provider, and Gate.Run is documented (storage/limiter.go) to run fn
	// INLINE on the caller's goroutine with NO deadline - a deadline cannot
	// tell a legitimately slow multi-GB upload from a wedged mount, so it does
	// not try. On a wedged mount a blocking Lock() would then park every later
	// SaveConfig/SwitchConfig on this Core behind a mutex nothing will ever
	// release: a leaked goroutine and HTTP connection per attempt, with no TTL
	// to self-heal, while the Redis lock's own TTL expires underneath it so
	// even the cross-process guarantee is gone. That is worse than the overlap
	// this lock exists to prevent, since a wedged mount is exactly the failure
	// this whole feature exists to detect. TryLock turns "busy" into the same
	// fast refusal the Redis branch below returns, instead of an unbounded
	// park, so a same-replica caller and a cross-replica caller behave
	// identically.
	if !s.reachRoundMu.TryLock() {
		return &SharedStorageRefusal{Status: http.StatusConflict, Message: reachRoundBusyMessage}
	}
	defer s.reachRoundMu.Unlock()

	// The lock value is an unguessable per-attempt token, not a shared
	// constant, so the deferred release below can confirm it still owns the
	// lock before deleting it. A shared value would let a holder that overran
	// the TTL delete a LATER holder's lock - the same unconditional-delete bug
	// class this lock exists to defend against in RunRound's own cleanup.
	token, tokenErr := newReachRoundLockToken()
	if tokenErr != nil {
		log.Printf("core storage: could not generate a config-round lock token: %v", tokenErr)
		return &SharedStorageRefusal{
			Status:  http.StatusServiceUnavailable,
			Message: "Could not verify shared storage access right now. Check that Redis is reachable and try again.",
		}
	}

	acquired, lockErr := s.Redis.SetNX(ctx, reachRoundLockKey, token, reachRoundLockTTL).Result()
	if lockErr != nil {
		log.Printf("core storage: could not acquire the config-round lock: %v", lockErr)
		return &SharedStorageRefusal{
			Status:  http.StatusServiceUnavailable,
			Message: "Could not verify shared storage access right now. Check that Redis is reachable and try again.",
		}
	}
	if !acquired {
		return &SharedStorageRefusal{Status: http.StatusConflict, Message: reachRoundBusyMessage}
	}
	defer func() {
		if err := releaseReachRoundLock(context.WithoutCancel(ctx), s.Redis, token); err != nil {
			log.Printf("core storage: could not release the config-round lock: %v", err)
		}
	}()

	deadline := s.reachRoundDeadline
	if deadline <= 0 {
		deadline = defaultReachRoundDeadline
	}

	reachCfg := ToReachConfig(cfg)
	result, err := s.Reach.Coordinator().RunRound(ctx, reachCfg, online, storagereach.RoundOptions{
		Deadline: deadline,
		// Zero leaves storagereach's production cadence in place; only tests set
		// it, to stop a short deadline from reducing the coordinator to a couple
		// of listings. See Manager.reachRoundPoll.
		PollEvery: s.reachRoundPoll,
		// Each partial result becomes the panel's live "X/N cores confirmed"
		// counter. Without this the admin stares at a spinner for 15s.
		OnProgress: func(r storagereach.RoundResult) {
			s.Events.Publish(ctx, storagereach.EventStorageReachChanged, map[string]interface{}{
				"round":     r.RoundID,
				"confirmed": r.Confirmed,
				"total":     r.Total,
				"done":      r.Done,
				"ok":        r.OK,
				"results":   redactReachResults(r.Results),
			})
		},
	})
	if err != nil {
		return &SharedStorageRefusal{
			Status:  http.StatusServiceUnavailable,
			Message: fmt.Sprintf("Could not verify shared storage access across %d Cores: %v", len(online), err),
			Result:  &result,
		}
	}
	if !result.OK {
		return &SharedStorageRefusal{
			Status: http.StatusConflict,
			Message: fmt.Sprintf("Only %d of %d Cores could prove they can read and write this storage. The settings were not saved.",
				result.Confirmed, result.Total),
			Result: &result,
		}
	}
	return nil
}

// redactReachResults strips Detail - the raw backend error text (mount paths,
// S3 endpoints, bucket names, permission text) - before a round's progress
// goes out over the system-events SSE stream.
//
// /api/system/events is authenticated but carries no capability check, so
// every logged-in tenant with a panel session open receives this event: up to
// 15 of them per round, one per poll tick a status actually changes on. The
// fault-list endpoint (StorageReachStatus, settings.read-gated) is the one
// place Detail is meant to reach an operator; this is the same leak class the
// 503 route gate was already fixed against (see RequireCoreStorageReachable's
// own doc comment in feature_gate.go).
func redactReachResults(results []storagereach.CoreResult) []storagereach.CoreResult {
	out := make([]storagereach.CoreResult, len(results))
	for i, r := range results {
		out[i] = r
		out[i].Detail = ""
	}
	return out
}
