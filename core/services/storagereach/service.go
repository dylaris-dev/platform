package storagereach

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"dylaris-core/services"

	"github.com/redis/go-redis/v9"
)

// EventStorageReachChanged is the SSE event the panel refreshes on. Event
// types in this codebase are inline string literals at the publish site; this
// one is a constant because two packages emit it (the service and the config
// round handler) and they must agree.
const EventStorageReachChanged = "storagereach.changed"

const (
	// defaultSelfCheckEvery is the owner-chosen periodic interval.
	defaultSelfCheckEvery = 120 * time.Second
	// defaultPollEvery is how often a Core looks for an open round. A config
	// round has a 15s cap, so polling at the self-check interval would miss
	// every round; this is the loop's real tick.
	defaultPollEvery = time.Second
	// defaultSelfCheckBudget is how long the loop waits for one self-check
	// before it stops waiting and rules on it. It is the same 15s a config
	// round gets: both are "prove storage access once", and a backend that has
	// not answered in 15s is not about to.
	defaultSelfCheckBudget = 15 * time.Second
	// defaultRoundParticipationBudget is how long the loop waits for one
	// round-participation attempt before it stops waiting on it. It matches
	// RunRound's own default 15s Deadline: a participant that has not answered
	// by then is legitimately no-response to the coordinator, not something
	// this loop needs to rule on itself.
	defaultRoundParticipationBudget = 15 * time.Second
	// defaultBeaconMaxAge is 2.5 self-check intervals: long enough that one
	// missed refresh does not evict a healthy Core, short enough that a dead
	// one stops vouching for the share within five minutes.
	defaultBeaconMaxAge = 300 * time.Second
	// notSharedGraceRounds is how many CONSECUTIVE not-shared verdicts gate
	// the routes. See shouldGate for why only this status gets grace.
	//
	// On a cold fleet (nobody has a fresh beacon-write claim yet) a Core's
	// very first pass has no peer claims to expect, so expectedParticipants
	// narrows the set down to itself alone and that first pass reads ok
	// before the streak ever starts, which is why a genuine fake-shared
	// volume takes three passes to gate on a cold start, not two.
	notSharedGraceRounds = 2
)

// ServiceDeps are injected so this package stays a leaf: it never imports
// handlers (which owns config resolution) or services (which owns
// OnlineCoreIDs), and every dependency is trivially fakeable in tests.
type ServiceDeps struct {
	Redis  *redis.Client
	CoreID string
	// NewProvider builds a provider for a config.
	NewProvider ProviderFactory
	// ConfigFor returns the PERSISTED core storage config. The bool is false
	// when storage is not configured at all.
	ConfigFor func() (Config, bool)
	// OnlineCores returns the heartbeating Core ids.
	OnlineCores func(context.Context) ([]string, error)
	// Publish, when set, emits an SSE event. Optional.
	Publish func(eventType string, payload map[string]interface{})
}

// Service owns this Core's boot check, its periodic self-check, and its
// participation in other Cores' config rounds.
type Service struct {
	deps   ServiceDeps
	status *LocalStatus
	coord  *Coordinator

	// These six are fields rather than constants so tests can age beacons out
	// and drive the loop in milliseconds instead of minutes.
	beaconMaxAge    time.Duration
	claimTTL        time.Duration
	selfCheckEvery  time.Duration
	selfCheckBudget time.Duration
	pollEvery       time.Duration
	roundBudget     time.Duration

	// notSharedStreak counts consecutive not-shared verdicts. Only ever
	// touched from apply, which the single service loop serialises: a check
	// goroutine produces a verdict and never applies one.
	notSharedStreak int

	mu         sync.Mutex
	lastStatus Status

	startOnce sync.Once
	doneCh    chan struct{}
}

func NewService(deps ServiceDeps) *Service {
	return &Service{
		deps:            deps,
		status:          NewLocalStatus(),
		coord:           NewCoordinator(deps.Redis, deps.CoreID, deps.NewProvider),
		beaconMaxAge:    defaultBeaconMaxAge,
		claimTTL:        defaultClaimTTL,
		selfCheckEvery:  defaultSelfCheckEvery,
		selfCheckBudget: defaultSelfCheckBudget,
		pollEvery:       defaultPollEvery,
		roundBudget:     defaultRoundParticipationBudget,
		doneCh:          make(chan struct{}),
	}
}

// Status is what the route gate reads on every storage request.
func (s *Service) Status() *LocalStatus { return s.status }

// Coordinator runs config-time rounds.
func (s *Service) Coordinator() *Coordinator { return s.coord }

// Done closes when the service loop has exited.
func (s *Service) Done() <-chan struct{} { return s.doneCh }

// Start runs the boot self-check, then loops. Calling it twice is a no-op so a
// second call cannot leave an unstoppable goroutine behind.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		go s.run(ctx)
	})
}

// checkOutcome is what one self-check goroutine hands back to the loop. It
// carries a verdict and nothing else: gating, fault recording and publishing
// all stay on the loop goroutine, which is what keeps notSharedStreak
// single-writer without a mutex.
type checkOutcome struct {
	res CoreResult
	// apply is false when the check produced nothing to commit; see observe.
	apply bool
}

// roundOutcome is what one round-participation goroutine hands back to the
// loop. RunParticipant already commits everything it produces - its own
// report key in Redis - so there is nothing here to apply, only to log and to
// let the loop stop waiting on it.
type roundOutcome struct {
	err error
}

// run is the service loop. Every state mutation happens here: the storage work
// runs in a child goroutine that only produces a verdict.
//
// The child goroutine is the whole point. A wedged mount is exactly the
// failure this package exists to catch, and it cannot be waited on safely:
// LocalProvider's MkdirAll, CreateTemp, Sync and Rename are plain blocking
// syscalls, so a cancelled context cannot interrupt one that is already stuck
// (storage/provider.go says so on the interface itself). A synchronous
// self-check would therefore freeze this loop, stop round participation, never
// observe ctx.Done, and leave the Core reporting healthy forever - because
// LocalStatus starts healthy by design and nothing would ever update it.
func (s *Service) run(ctx context.Context) {
	defer close(s.doneCh)

	// Capacity 1 so a check that overran its budget can still complete its send
	// and exit after this loop moved on. Only one check is ever in flight (see
	// inFlight), so one slot is exactly enough and the send never blocks.
	outcomes := make(chan checkOutcome, 1)

	// Both are read and written ONLY on this goroutine. inFlight stays true
	// until the check actually reports back, budget or no budget: the goroutine
	// is still out there, and starting a second one against a wedged mount
	// would only add another stuck goroutine every interval.
	var (
		inFlight     bool
		budgetExpiry time.Time
	)
	startCheck := func() {
		inFlight = true
		budgetExpiry = time.Now().Add(s.selfCheckBudget)
		go func() {
			res, ok := s.observe(ctx)
			outcomes <- checkOutcome{res: res, apply: ok}
		}()
	}

	// Same hazard and the same watchdog shape as the self-check above:
	// RunParticipant writes through the same provider, so it can wedge on the
	// same hung syscall. See this function's doc comment for why the child
	// goroutine cannot be waited on safely and is left to leak instead.
	roundOutcomes := make(chan roundOutcome, 1)
	var (
		roundInFlight     bool
		roundBudgetExpiry time.Time
		roundIDInFlight   string
		// lastRoundID is the round this Core already answered. The current-
		// round key stays up until the coordinator returns, so without it a
		// finished participant re-probes every tick and can write probe files
		// after the coordinator's cleanup, which nothing then removes.
		lastRoundID string
	)
	startRound := func(roundID string) {
		roundInFlight = true
		roundBudgetExpiry = time.Now().Add(s.roundBudget)
		roundIDInFlight = roundID
		lastRoundID = roundID
		go func() {
			err := RunParticipant(ctx, s.deps.Redis, s.deps.CoreID, roundID, s.deps.NewProvider)
			roundOutcomes <- roundOutcome{err: err}
		}()
	}

	// The boot check starts before the first tick: a Core that cannot see the
	// shared storage must be gated the moment it joins the load balancer, not
	// two minutes later.
	startCheck()
	nextSelfCheck := time.Now().Add(s.selfCheckEvery)

	ticker := time.NewTicker(s.pollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// A check blocked in a hung syscall cannot be reclaimed by any Go
			// mechanism, so it outlives this loop by design. Leaking one
			// goroutine on a wedged mount is the acceptable half of the trade;
			// stalling the service forever is not.
			return
		case out := <-outcomes:
			inFlight = false
			budgetExpiry = time.Time{}
			// A late result is still an honest observation of the backend, so
			// it is applied rather than dropped: it is what un-gates a Core
			// whose mount recovered between two periodic ticks.
			if out.apply {
				s.apply(ctx, out.res)
			}
			continue
		case out := <-roundOutcomes:
			roundInFlight = false
			roundBudgetExpiry = time.Time{}
			if out.err != nil {
				services.ReportOperatorError("storagereach", "participating in round %s: %v", roundIDInFlight, out.err)
				// A Redis error left no report; the next tick may try again.
				lastRoundID = ""
			}
			continue
		case <-ticker.C:
		}

		// A check that blew its budget IS the fault: the backend has not
		// answered, which is what unreachable means. Re-armed to the next
		// self-check cadence rather than cleared: inFlight stays true for as
		// long as the mount stays wedged, so clearing budgetExpiry would rule
		// on this exactly once for the lifetime of the process. The fault's
		// Redis record would then sit un-refreshed until its own TTL expired,
		// after which a Core that is still alive, still gated, and still
		// failing every storage request would look perfectly healthy on the
		// fleet page. Re-arming keeps the same synthetic verdict flowing at
		// the self-check cadence without starting a second goroutine against
		// the wedged mount - inFlight is untouched.
		if inFlight && !budgetExpiry.IsZero() && time.Now().After(budgetExpiry) {
			budgetExpiry = time.Now().Add(s.selfCheckEvery)
			s.apply(ctx, CoreResult{
				CoreID: s.deps.CoreID,
				Status: StatusUnreachable,
				Detail: fmt.Sprintf("storage self-check did not finish within %s", s.selfCheckBudget),
			})
		}

		// A round attempt that blew its budget is NOT a fault to record: the
		// coordinator already treats a participant with no report as
		// no-response, which is the correct, fail-closed verdict for a Core
		// that cannot reach storage - answering anyway would be dishonest.
		// Re-armed for the same reason as the self-check ruling above: without
		// it, a permanently stuck attempt would log exactly once for the life
		// of the process and then go silent even though roundInFlight is still
		// blocking every later attempt.
		if roundInFlight && !roundBudgetExpiry.IsZero() && time.Now().After(roundBudgetExpiry) {
			roundBudgetExpiry = time.Now().Add(s.roundBudget)
			services.ReportOperatorError("storagereach", "round %s participation did not finish within %s; the coordinator will see this Core as no-response", roundIDInFlight, s.roundBudget)
		}

		// Take part in any open config round. This is the fast path: a round
		// lives for 15s, so it cannot wait for the 120s self-check. Skipped
		// while one is still in flight, same as startCheck below, so a wedged
		// mount does not accumulate one more stuck goroutine every tick.
		if !roundInFlight {
			if roundID, err := PendingRoundID(ctx, s.deps.Redis); err == nil && roundID != "" && roundID != lastRoundID {
				startRound(roundID)
			}
		}

		if time.Now().After(nextSelfCheck) {
			nextSelfCheck = time.Now().Add(s.selfCheckEvery)
			if !inFlight {
				startCheck()
			}
		}
	}
}

// SelfCheck proves THIS Core's access to the persisted shared storage and
// applies the result: it updates the local gate, records or clears the Redis
// fault, and publishes on a change.
//
// It refreshes the fleet beacon rather than running a round, because a
// self-check has no coordinator: each Core ticks on its own schedule, so only
// a stable per-Core path lets them see each other at all. And it reads the
// PERSISTED config, unlike a config round, which tests a candidate that has
// deliberately not been saved yet.
//
// It is synchronous, so it blocks for exactly as long as the backend does. The
// service loop never calls it for that reason; see run.
//
// Must not be called concurrently with a running Start: both paths end up in
// apply, and apply's state (notSharedStreak, lastStatus) is mutated on the
// assumption of a single caller at a time, not guarded by a mutex.
func (s *Service) SelfCheck(ctx context.Context) CoreResult {
	res, ok := s.observe(ctx)
	if !ok {
		return res
	}
	return s.apply(ctx, res)
}

// observe does one self-check's storage I/O and returns THIS Core's verdict.
// It mutates no service state, which is what makes it safe to run off the loop
// goroutine. The bool is false when there is nothing to commit.
func (s *Service) observe(ctx context.Context) (CoreResult, bool) {
	me := s.deps.CoreID

	cfg, configured := s.deps.ConfigFor()
	if !configured {
		// Not a fault: RequireCoreStorageConfigured already refuses the write
		// routes, and a red fleet banner on every fresh install would train
		// operators to ignore it.
		return CoreResult{CoreID: me, Status: StatusOK}, true
	}

	participants, err := s.deps.OnlineCores(ctx)
	if err != nil {
		// Redis being unreachable is separately visible on the health page.
		// Closing every storage route because peers could not be COUNTED
		// would turn a Redis blip into a storage outage.
		services.ReportOperatorError("storagereach", "could not list online Cores, skipping this self-check: %v", err)
		return CoreResult{CoreID: me, Status: StatusOK}, false
	}
	if !contains(participants, me) {
		// This Core's own heartbeat has not landed yet. Checking without
		// itself in the set would prove nothing about itself.
		//
		// Copied first: the slice belongs to the OnlineCores implementation,
		// and appending in place would write into a backing array it is free to
		// reuse on the next call.
		withMe := make([]string, len(participants), len(participants)+1)
		copy(withMe, participants)
		participants = append(withMe, me)
	}

	fingerprint := Fingerprint(cfg)

	// Read the peers' beacon-write claims BEFORE refreshing this Core's own
	// beacon, never after. A claim already in Redis was published after the
	// beacon it vouches for was written, so a listing taken afterwards cannot
	// miss that file; reading claims afterwards would let a peer that wrote
	// mid-pass be expected before this Core ever had a chance to see it.
	claimed, claimErr := FreshClaims(ctx, s.deps.Redis, participants, time.Now(), s.claimTTL)
	if claimErr != nil {
		// Same shape as the OnlineCores failure above, and the same answer.
		// Without the claim set there is no honest way to tell "my storage is
		// fake-shared" from "a peer stopped writing", and ruling anyway would
		// either gate on a peer's fault or clear a real one of this Core's.
		// Committing nothing leaves an already-gated Core gated, with its
		// fault standing.
		services.ReportOperatorError("storagereach", "could not read peer beacon claims, skipping this self-check: %v", claimErr)
		return CoreResult{CoreID: me, Status: StatusOK}, false
	}

	prov, provErr := s.deps.NewProvider(cfg)
	if provErr != nil {
		// No provider means no beacon write, so this Core has to stop being
		// expected by its peers now rather than when its claim expires.
		s.claimBeaconWrite(ctx, false)
		return CoreResult{CoreID: me, Status: StatusUnreachable, Detail: provErr.Error()}, true
	}

	// Participants, not the claim-filtered set: what this Core LOOKS at is
	// every online peer, so a mismatched fingerprint and a denied cross-write
	// are still observed. The claim filter belongs on what it is entitled to
	// EXPECT, which is the aggregation below.
	rep := RefreshBeacon(ctx, prov, BeaconOptions{
		CoreID: me, Fingerprint: fingerprint,
		Participants: participants, MaxAge: s.beaconMaxAge,
	})
	s.claimBeaconWrite(ctx, rep.Wrote)

	// Only THIS Core's verdict is meaningful here - the report is its own.
	// The fleet view is assembled from every Core's own fault record, not
	// from one Core's opinion of its peers.
	agg := Aggregate(expectedParticipants(participants, me, claimed, rep.MismatchedPeers),
		map[string]Report{me: rep}, fingerprint, true)
	for _, r := range agg.Results {
		if r.CoreID == me {
			return r, true
		}
	}
	// Not reachable in practice - me is in participants by the block above, and
	// Aggregate emits one result per participant - but Go needs a terminal
	// return, so it is fail-closed rather than a silent ok. A Core with no
	// verdict about itself has proven nothing about the storage, and claiming
	// health on no evidence is the single outcome this package exists to
	// prevent. This goes through apply like any other failure: it gates and
	// records a fault the operator can see.
	return CoreResult{
		CoreID: me,
		Status: StatusNoResponse,
		Detail: "self-check produced no verdict for this Core",
	}, true
}

// claimBeaconWrite publishes or withdraws this Core's beacon-write claim, which
// is what tells its PEERS whether to expect its beacon in storage at all.
//
// It is tied strictly to the beacon WRITE, never to the verdict. On a
// fake-shared volume every Core writes its own file perfectly and every one
// reports not-shared; withdrawing the claim on a failing verdict would make all
// of them stop expecting each other and un-gate the entire fleet on the next
// pass, which is the exact failure this package exists to catch.
//
// A Redis error is logged, not fatal: it is a claim about this Core for other
// Cores to read, not evidence about this Core's own storage, so it must not
// become this Core's verdict.
func (s *Service) claimBeaconWrite(ctx context.Context, wrote bool) {
	if wrote {
		if err := PublishClaim(ctx, s.deps.Redis, s.deps.CoreID, time.Now(), s.claimTTL); err != nil {
			services.ReportOperatorError("storagereach", "%v", err)
		}
		return
	}
	// Withdrawn the moment the write fails, so healthy peers stop expecting
	// this Core's beacon at once instead of at the claim's TTL - which is long
	// enough for them to gate themselves on a fault that is only about here.
	if err := ClearClaim(ctx, s.deps.Redis, s.deps.CoreID); err != nil {
		services.ReportOperatorError("storagereach", "%v", err)
	}
}

// shouldGate decides whether a verdict closes this Core's storage routes.
//
// not-shared is the one ambiguous status: on a single pass, "I cannot see
// core-b's beacon" is indistinguishable from "core-b booted ten seconds ago
// and has not written one yet". Gating on that would make every scale-up
// briefly close its peers' storage routes. Every other failing status is a
// directly observed local fault and gates at once.
//
// The fault is RECORDED on the first pass either way, so the panel shows it
// immediately even while the routes stay open.
func (s *Service) shouldGate(status Status) bool {
	if status == StatusOK {
		s.notSharedStreak = 0
		return false
	}
	if status != StatusNotShared {
		s.notSharedStreak = 0
		return true
	}
	s.notSharedStreak++
	return s.notSharedStreak >= notSharedGraceRounds
}

// apply commits a verdict: gate or ungate locally, record or clear the fault,
// and publish only when the status actually changed.
func (s *Service) apply(ctx context.Context, res CoreResult) CoreResult {
	// The fault below is recorded on the FIRST failing pass regardless, so the
	// panel shows it immediately. Only the local route gate waits out the
	// grace window, and only for not-shared (see shouldGate).
	if s.shouldGate(res.Status) {
		s.status.Set(res.Status, res.Detail)
	} else {
		s.status.Set(StatusOK, "")
	}

	if res.Status == StatusOK {
		if err := ClearFault(ctx, s.deps.Redis, res.CoreID); err != nil {
			services.ReportOperatorError("storagereach", "%v", err)
		}
	} else {
		hostname, _ := os.Hostname()
		now := time.Now().Unix()
		if err := RecordFault(ctx, s.deps.Redis, Fault{
			CoreID:       res.CoreID,
			Hostname:     hostname,
			Status:       res.Status,
			Detail:       res.Detail,
			MissingPeers: res.MissingPeers,
			DeniedPeers:  res.DeniedPeers,
			Since:        now,
			At:           now,
		}); err != nil {
			services.ReportOperatorError("storagereach", "%v", err)
		}
		// Plain log, and deliberately not reported to the error stream: the
		// verdict was just written with RecordFault above and the panel reads
		// it from there. What IS reported are the failures of this verifier's
		// own machinery - a round that could not run, a claim that could not be
		// published, a fault that could not be recorded - because those are the
		// ones no screen can show, and the last of them is what makes every
		// other fault invisible.
		log.Printf("storagereach: %s cannot use the shared storage: %s %s", res.CoreID, res.Status, res.Detail)
	}

	s.mu.Lock()
	changed := s.lastStatus != res.Status
	s.lastStatus = res.Status
	s.mu.Unlock()

	// Only on a change: publishing every 120s tick would make a healthy fleet
	// chatter at the panel forever.
	if changed && s.deps.Publish != nil {
		s.deps.Publish(EventStorageReachChanged, map[string]interface{}{
			"coreId": res.CoreID,
			"status": string(res.Status),
		})
	}
	return res
}
