package services

import (
	"context"
	nodegrpc "dylaris-core/grpc"
	"dylaris-core/mailer"
	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/services/redisacl"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Settings keys + built-in defaults for the BYON non-payment lifecycle. All are
// payment-provider-agnostic: status is set by the admin endpoint today (or a
// webhook later) and this worker progresses it.
const (
	BillingGracePeriodKey   = "billing.grace_period"
	BillingR2RetentionKey   = "billing.r2_retention"
	BillingNodeRetentionKey = "billing.node_retention"
	BillingPaymentURLKey    = "billing.payment_url"

	// BillingR2IncludedKey is the backup storage one purchased UNIT brings, in
	// GB. Multiplied by what the tenant holds, the same way every other
	// allowance on this platform is - a customer with three BYON units gets
	// three times the addresses and three times the traffic, and backup storage
	// had no reason to be the one thing that did not scale with the purchase.
	BillingR2IncludedKey = "billing.r2_included_gb"
	// BillingR2BookableKey is how much MORE a tenant may take on top, once they
	// have agreed to be charged for it. Zero means the included amount is the
	// end of it, whatever they agree to.
	BillingR2BookableKey = "billing.r2_bookable_gb"

	DefaultR2IncludedGB = 50
	DefaultR2BookableGB = 500

	DefaultGracePeriod = "3d"
	// DefaultR2Retention is how long a SUSPENDED tenant's backups stay on our
	// storage before they are deleted. One week, deliberately short: the window
	// exists so somebody who fixes their payment finds their data intact, not so
	// we store it indefinitely for free. Their servers and backups stay readable
	// throughout - suspension stops servers, it does not gate the backup routes.
	//
	// Backups on a storage the TENANT connected are never touched by this at any
	// deadline; see deleteTenantBackups.
	DefaultR2Retention   = "1w"
	DefaultNodeRetention = "2w"
)

// R2IncludedGB is the backup storage a tenant's entitlement brings, before
// anything they have agreed to be charged for. Zero when they hold nothing.
//
// "Entitlement", not "purchase": a live administrator grant is worth one unit of
// its kind, so a comped tenant gets the same allowance a single BYON purchase
// includes rather than none. See entitledUnits.
func R2IncludedGB(st settingReader, b *store.UserBilling) int64 {
	return settingInt(st, BillingR2IncludedKey, DefaultR2IncludedGB) * entitledUnits(b, time.Now())
}

// R2BookableGB is how much a tenant MAY take beyond the included amount once
// metered backup billing is on. Also per unit: the cap on what they can spend
// scales with what they hold, like the allowance it sits on top of.
func R2BookableGB(st settingReader, b *store.UserBilling) int64 {
	return R2BookablePerUnit(st) * entitledUnits(b, time.Now())
}

// R2BookablePerUnit is the stored setting on its own, before any tenant's units
// are applied. The operator screen edits this number, and the notification that
// goes out when it changes is written against it.
func R2BookablePerUnit(st settingReader) int64 {
	return settingInt(st, BillingR2BookableKey, DefaultR2BookableGB)
}

// entitledR2QuotaGB is the hard stop for a tenant who holds something, bought or
// granted: their included allowance, plus the bookable extra ONLY if they agreed
// to pay for it.
//
// nil when they hold NOTHING, so an account with no entitlement at all falls
// through to the flat platform setting rather than being handed a quota of zero
// - which would stop backups on every self-hosted install the moment this
// shipped. That fallthrough is why a grant has to count as a unit: a comped
// tenant took this path and landed on the platform setting, which is unset by
// default and means no cap.
func entitledR2QuotaGB(st settingReader, b *store.UserBilling) *int64 {
	if entitledUnits(b, time.Now()) == 0 {
		return nil
	}
	total := R2IncludedGB(st, b)
	if b != nil && b.BackupBillingEnabled {
		total += R2BookableGB(st, b)
	}
	return &total
}

// entitledUnits counts the countable products a tenant holds RIGHT NOW, whether
// they bought them or an administrator granted them. nil and 0 both mean none:
// the store clears the override rather than writing 0, so a stored zero is not a
// quantity it ever meant.
//
// It counts a live grant because this number is what the per-unit allowances are
// multiplied by, and a grant that conveys access but no allowance produces the
// wrong answer at both ends. It used to count purchases only, so a granted
// tenant had ZERO units: their included backup storage worked out to nothing,
// purchasedR2QuotaGB returned "no answer", and the resolution fell through to
// the flat platform setting - which is unset by default and means NO CAP. The
// tenant an administrator comped was the one tenant with unlimited backup
// storage.
//
// Built on grantedCap so the "a purchase wins over a grant" rule stays in one
// place: a grant is worth one machine of its kind, and only while it is live, so
// the allowance lapses on its own with nothing to clean up. An administrator who
// wants a different number sets the per-user override (b.R2QuotaGB), which
// r2QuotaGB already reads first.
func entitledUnits(b *store.UserBilling, now time.Time) int64 {
	if b == nil {
		return 0
	}
	var n int64
	if c := grantedCap(b.MaxNodes, b.ManualByonExpiresAt, now); c != nil && *c > 0 {
		n += *c
	}
	if c := grantedCap(b.MaxLinks, b.ManualRouteExpiresAt, now); c != nil && *c > 0 {
		n += *c
	}
	return n
}

// settingReader is the whole store surface the backup allowances need. Narrow
// rather than store.Store so BackupAllowanceGB can hand its own narrow
// interface straight through - every real caller still passes the full store.
type settingReader interface {
	GetSetting(key string) (string, error)
}

func settingInt(st settingReader, key string, fallback int64) int64 {
	if v, _ := st.GetSetting(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

// backupQuotaStore is the allowance surface plus the one usage figure.
type backupQuotaStore interface {
	backupAllowanceStore
	BackupBytesByOwner(ownerID string) (int64, error)
}

// BackupAllowanceExceeded reports whether a server owner may store one more
// byte of backup on OUR storage. It is a CREATE gate, so it answers at-or-over:
// sitting exactly on the allowance means the next backup does not fit.
//
// ownerID is the SERVER's owner, never the caller - see BackupAllowanceGB.
//
// quotaBytes is 0 when there is no cap, and both callers only read the numbers
// on the exceeded path, so that overload never reaches a message.
//
// `dest` is the storage the run has already resolved, and it takes the
// parameter rather than leaving the check to its callers on purpose: a run
// headed for a storage the TENANT connected must not meet our ceiling at all.
// Those bytes are already excluded from BackupBytesByOwner (bst.owner_id IS
// NULL) and deleteTenantBackups already refuses to touch them at any retention
// deadline, so the gate was the single place that did not take the distinction
// - a customer with their own bucket, once at our ceiling, could never back up
// again although they were no longer using our storage. Folding it in here
// leaves no version of this question a caller can ask without it.
func BackupAllowanceExceeded(st backupQuotaStore, ownerID string, storeEnabled bool, dest *models.BackupStorage) (exceeded bool, usedBytes, quotaBytes int64) {
	if dest != nil && dest.OwnerID != nil {
		return false, 0, 0
	}
	quota := BackupAllowanceGB(st, ownerID, storeEnabled)
	if quota == nil {
		return false, 0, 0
	}
	quotaBytes = *quota * 1024 * 1024 * 1024
	used, err := st.BackupBytesByOwner(ownerID)
	if err != nil {
		return false, 0, quotaBytes
	}
	return used >= quotaBytes, used, quotaBytes
}

// BillingLifecycleService runs the non-payment lifecycle: past_due (grace, all
// running) -> suspended (services stopped, read access kept) -> retention cleanup
// (next chunk removes node connection + R2 backups after their windows). It NEVER
// deletes a user or their DB rows. Leader-gated so only one Core acts.
type BillingLifecycleService struct {
	store       store.Store
	queue       *QueueService
	registry    *nodegrpc.Registry // for backupstorage.Deps (node-local deletes)
	frontendURL string
	leader      leader.Election
	interval    time.Duration
	// storeEnabled mirrors config.StoreEnabled and reaches the over-limit sweep,
	// which has to tell "no billing plane exists" from "this tenant holds no
	// entitlement". Zero value false means self-host, where the sweep grants
	// everything - the safe direction if this is ever left unwired.
	storeEnabled bool

	// Route-only link teardown/restore deps, wired after the ACL provisioner is
	// built (SetLinkACL). Nil in solo/hoster mode, where there are no link kits.
	gateway       GatewayProvider
	redis         *redis.Client
	provisioner   *redisacl.Provisioner
	clusterSecret string

	// warpPeers drops a tenant's warp tunnels at the hard cutoff. Wired after the
	// warp service exists (SetWarpPeers); nil where there is no overlay, in which
	// case there is no tunnel to drop either.
	warpPeers warpPeerDisconnector

	// suspendGrace defers the hard cutoff until suspended_at + suspendGrace has
	// elapsed (see enforceSuspensions). Threaded from cfg.SuspendGrace at
	// construction, like frontendURL.
	suspendGrace time.Duration
}

// warpPeerDisconnector is the narrow slice of WarpService the lifecycle needs:
// remove every WireGuard peer enrolled under one key, at every leader of its
// region, and delete the rows.
type warpPeerDisconnector interface {
	DisconnectKeyPeers(ctx context.Context, keyID int) int
}

func NewBillingLifecycleService(s store.Store, q *QueueService, registry *nodegrpc.Registry, frontendURL string, suspendGrace time.Duration, storeEnabled bool) *BillingLifecycleService {
	return &BillingLifecycleService{store: s, queue: q, registry: registry, frontendURL: frontendURL, interval: time.Hour, suspendGrace: suspendGrace, storeEnabled: storeEnabled}
}

func (s *BillingLifecycleService) SetLeader(l leader.Election) { s.leader = l }

// SetWarpPeers wires the tunnel teardown used at the hard cutoff. Called once at
// startup after the warp service exists. Without it, enforcement still stops
// servers and drops link credentials - it just leaves the tunnel up.
func (s *BillingLifecycleService) SetWarpPeers(w warpPeerDisconnector) { s.warpPeers = w }

// SetLinkACL wires the route-only link teardown/restore hooks. Called once at
// startup after the ACL provisioner and gateway exist. When any dependency is nil
// (solo/hoster mode) Suspend/Reactivate skip the link steps.
func (s *BillingLifecycleService) SetLinkACL(gw GatewayProvider, rdb *redis.Client, prov *redisacl.Provisioner, clusterSecret string) {
	s.gateway = gw
	s.redis = rdb
	s.provisioner = prov
	s.clusterSecret = clusterSecret
}

func (s *BillingLifecycleService) Start(ctx context.Context) {
	log.Println("Billing lifecycle service started")
	ticker := time.NewTicker(s.interval)
	go func() {
		defer ticker.Stop()
		// One pass immediately, then on the ticker. Without it a restart bought
		// every tenant up to a full interval of unenforced time - a grace window
		// that elapsed while Core was down was not acted on until an hour after it
		// came back. Every action here is already time-gated, so an early pass can
		// only do what was due anyway. Matches the ticket retention service.
		for {
			if s.leader == nil || s.leader.IsLeader() {
				s.runOnce(ctx)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// runOnce progresses past_due tenants whose grace window has elapsed into
// suspended. Retention cleanup of suspended tenants is a separate pass added in
// the next chunk.
func (s *BillingLifecycleService) runOnce(ctx context.Context) {
	pastDue, err := s.store.ListUserBillingByStatus("past_due")
	if err != nil {
		log.Printf("billing lifecycle: list past_due: %v", err)
		return
	}
	now := time.Now()
	for _, b := range pastDue {
		if b.GraceUntil == nil || now.Before(*b.GraceUntil) {
			continue
		}
		if err := s.Suspend(ctx, b.UserID); err != nil {
			log.Printf("billing lifecycle: suspend %s: %v", b.UserID, err)
		}
	}

	// Deferred hard cutoff: enforce suspensions whose grace has elapsed. Separate
	// from the past_due->suspended promotion above so a tenant suspended this pass
	// still gets the full grace before anything is cut.
	s.enforceSuspensions(ctx)

	// Over-limit is a SEPARATE clock from non-payment: a tenant who downgraded is
	// paying perfectly well and still holding more than they bought. Runs after
	// the payment path so an already-suspended tenant is skipped rather than
	// warned about services that are not running.
	s.enforceEntitlementLimits(ctx)

	s.cleanupExpiredR2(ctx)
	// NOTE: node-connection retention teardown (drop the warp tunnel + revoke the
	// tenant's warp peers/keys after node_retention) is handled in the Warp
	// multi-hub track, which adds the tenant-scoped warp queries + remove_peer
	// command needed to disconnect a LIVE tunnel. Deleting enroll tokens alone
	// would not drop an active tunnel, so it is intentionally not done here.
}

// enforceSuspensions applies the hard cutoff to tenants whose suspension has
// persisted past the grace window (suspended_at + suspendGrace <= now): it stops
// their running servers and drops their route-only link ACLs. Suspend() only
// records the suspended state; the cutoff lands here so a transient billing fault
// cannot instantly kick a paying customer. Idempotent - stopping an already
// stopped server and DELUSER/DEL on absent keys are no-ops - so re-running every
// hour is harmless and needs no "enforced" flag. Leader-gated via runOnce.
func (s *BillingLifecycleService) enforceSuspensions(ctx context.Context) {
	suspended, err := s.store.ListUserBillingByStatus("suspended")
	if err != nil {
		log.Printf("billing lifecycle: list suspended for enforcement: %v", err)
		return
	}
	now := time.Now()
	for _, b := range suspended {
		if b.SuspendedAt == nil || now.Before(b.SuspendedAt.Add(s.suspendGrace)) {
			continue
		}
		// One line per enforced tenant per pass. Suspended tenants are few, so the
		// hourly repeat is acceptable; no state is added just to silence it.
		log.Printf("billing lifecycle: enforcing suspension cutoff for %s (suspended_at=%s, grace=%s)",
			b.UserID, b.SuspendedAt.UTC().Format(time.RFC3339), s.suspendGrace)
		s.stopTenantServers(ctx, b.UserID)
		s.suspendTenantLinks(ctx, b.UserID)
		s.suspendTenantWarpPeers(ctx, b.UserID)
	}
}

// suspendTenantWarpPeers drops the tenant's warp tunnels once the grace has
// elapsed. Until this ran, a hard-suspended tenant kept a live overlay tunnel
// indefinitely: suspendTenantLinks takes away what the tunnel CARRIES, and
// stopTenantServers stops the servers, but neither touches the tunnel itself,
// and the warp client happily keeps a working peer forever.
//
// The peer is removed at every leader of its region and its row deleted, so the
// tunnel drops within a handshake interval rather than at the client's next
// re-enroll. The enroll gate in handlers/warp.go is what keeps it down - without
// it the client would re-enroll within minutes and this pass would fight it
// every hour.
//
// Idempotent: a key with no peers removes nothing, so the hourly repeat is a
// no-op once the tenant is cut off. Reactivation needs no counterpart - the
// client re-enrolls on its own once the gate opens (its handshake goes stale
// within ~3 minutes of losing the peer, and it re-enrolls on the 10 minute
// timer regardless).
func (s *BillingLifecycleService) suspendTenantWarpPeers(ctx context.Context, userID string) {
	if s.warpPeers == nil {
		return
	}
	keys, err := s.store.ListWarpAPIKeysByOwner(userID)
	if err != nil {
		log.Printf("billing lifecycle: list warp keys for %s: %v", userID, err)
		return
	}
	removed := 0
	for _, k := range keys {
		removed += s.warpPeers.DisconnectKeyPeers(ctx, k.ID)
	}
	if removed > 0 {
		log.Printf("billing lifecycle: dropped %d warp peer(s) for %s at the suspension cutoff", removed, userID)
	}
}

// cleanupExpiredR2 deletes the R2 backups of suspended tenants whose r2_retention
// window has elapsed (measured from suspended_at). The user account + server
// metadata are never touched — only the backup objects + their rows.
func (s *BillingLifecycleService) cleanupExpiredR2(ctx context.Context) {
	suspended, err := s.store.ListUserBillingByStatus("suspended")
	if err != nil {
		log.Printf("billing lifecycle: list suspended: %v", err)
		return
	}
	now := time.Now()
	for _, b := range suspended {
		if b.SuspendedAt == nil {
			continue
		}
		spec := s.effectiveSpec(b.R2Retention, BillingR2RetentionKey, DefaultR2Retention)
		deadline, ok := AddRetention(*b.SuspendedAt, spec)
		if !ok || now.Before(deadline) {
			continue
		}
		s.deleteTenantBackups(ctx, b.UserID)
	}
}

// deleteTenantBackups is the part of retention that actually deletes data, so
// the two things it must never do are skip an object and forget an object.
//
// It used to do both. A run whose job has no storage_id had its object deletion
// skipped entirely and its row deleted anyway - and storage_id is NULL for every
// job created with the panel's FIRST dropdown option, "Default storage", so that
// was the common case, not an edge one. The archive then survives the retention
// window with nothing left pointing at it: the tenant's data is still there, the
// operator is still paying for it, and the record that would let either be found
// is gone. Same NULL-means-default confusion that made those backups
// unrestorable (fixed in 1c08ded); this site was missed.
//
// Second, a FAILED delete also dropped the row, which turns a transient fault
// into the same permanent orphan. The row is now kept whenever the object was not
// confirmed gone, and the hourly pass retries. That cannot spin forever on an
// already-deleted object: S3 maps NoSuchKey to nil and local ignores
// os.IsNotExist, so a missing object reports success and the row goes on the
// first pass. A node-local archive on an unreachable node does keep retrying,
// which is the correct answer - the data is still out there.
func (s *BillingLifecycleService) deleteTenantBackups(ctx context.Context, userID string) {
	refs, err := s.store.ListBackupRunsByOwner(userID)
	if err != nil {
		log.Printf("billing lifecycle: list backups for %s: %v", userID, err)
		return
	}
	deps := backupstorage.Deps{Registry: s.registry, NodeStore: s.store}
	for _, ref := range refs {
		// The empty owner is deliberate: retention only ever removes archives on
		// OUR storage, so the chain must not walk into the tenant's own default.
		// A ref with no storage id at all predates the column, and a run that
		// predates it went to the platform default.
		storage, err := ResolveJobStorage(s.store, ref.StorageID, "")
		if err != nil {
			logErrf("billing-lifecycle", "run %d (%s): cannot resolve storage: %v — keeping the row so the object is not orphaned",
				ref.RunID, ref.StorageKey, err)
			continue
		}
		// A bucket the tenant connected themselves is not ours to empty. We pay
		// nothing for it, so deleting frees us nothing, and it holds data they
		// are paying somebody else to keep. The row stays too: the archive is
		// still there and still theirs to restore.
		if storage.OwnerID != nil {
			continue
		}
		provider, err := backupstorage.Open(ctx, storage, deps)
		if err != nil {
			logErrf("billing-lifecycle", "run %d (%s): cannot open storage %q: %v — keeping the row",
				ref.RunID, ref.StorageKey, storage.Name, err)
			continue
		}
		if err := provider.Delete(ctx, ref.StorageKey); err != nil {
			log.Printf("billing lifecycle: delete backup object %s: %v — keeping the row so the next pass retries",
				ref.StorageKey, err)
			continue
		}
		if err := s.store.DeleteBackupRun(ref.RunID); err != nil {
			log.Printf("billing lifecycle: delete backup run %d: %v", ref.RunID, err)
		}
	}
}

// effectiveSpec resolves a retention spec: per-user override wins, else the
// platform setting (if valid), else the built-in default.
func (s *BillingLifecycleService) effectiveSpec(override, settingKey, def string) string {
	if ValidRetentionSpec(override) {
		return override
	}
	if v, _ := s.store.GetSetting(settingKey); ValidRetentionSpec(v) {
		return v
	}
	return def
}

// EnterPastDue marks a tenant past_due, sets the grace deadline from the
// effective grace period, and sends the dunning email. Everything keeps running
// during grace. Re-calling resets the grace window.
func (s *BillingLifecycleService) EnterPastDue(userID string) error {
	b, err := s.store.GetUserBilling(userID)
	if err != nil {
		return err
	}
	grace := s.effectiveSpec(b.GracePeriod, BillingGracePeriodKey, DefaultGracePeriod)
	until, ok := AddRetention(time.Now(), grace)
	if !ok {
		until, _ = AddRetention(time.Now(), DefaultGracePeriod)
	}
	if err := s.store.SetUserBillingStatus(userID, "past_due", &until, nil); err != nil {
		return err
	}
	s.sendDunningEmail(userID, until)
	return nil
}

// Reactivate clears the lifecycle back to active (e.g. after payment). Stopped
// servers are NOT auto-started; the owner starts them. The tenant's route-only
// links come back on their own once their ACL + tunnel key are restored - but
// ONLY links that were GRACED-suspended (ACL/tunnel key dropped by
// suspendTenantLinks, never revoked). A link that was DURABLY REVOKED by an
// admin's force-suspend (SuspendNow) stays revoked: reactivateTenantLinks only
// re-provisions non-revoked kits (linkKitsForUser reads
// ListWarpAPIKeysByOwner, which already excludes revoked_at IS NOT NULL rows),
// so a force-suspended tenant's links must be deliberately re-minted by an
// admin after reactivation. This is intentional: reactivation must not
// silently restore a nuked tenant's link infrastructure.
func (s *BillingLifecycleService) Reactivate(userID string) error {
	if err := s.store.SetUserBillingStatus(userID, "active", nil, nil); err != nil {
		return err
	}
	// Paying again does not hand the links back to a tenant who is ALSO past
	// their over-limit grace. The two enforcements have separate clocks, and the
	// ACL reconciler would tear these down again on its next 60s tick anyway -
	// so restoring them here would mean issuing a working Redis credential for
	// one minute and calling it a reactivation.
	if b, err := s.store.GetUserBilling(userID); err == nil && b != nil &&
		b.OverLimitSince != nil && !time.Now().Before(b.OverLimitSince.Add(OverLimitGrace)) {
		log.Printf("billing lifecycle: %s is active again but still over its limits, links stay down", userID)
		return nil
	}
	s.reactivateTenantLinks(userID)
	return nil
}

// Suspend marks the tenant suspended and notifies them. Read access (file
// browser, backups) stays; the start path is gated elsewhere. No data is
// deleted. The hard cutoff (stop servers, drop link ACLs) is NOT synchronous
// here - see enforceSuspensions. Used by the AUTOMATIC non-payment lifecycle
// and the store webhook (handlers/store.go): a buggy webhook must not
// instant-kill a paying tenant. See SuspendNow for the ADMIN-manual immediate
// variant.
func (s *BillingLifecycleService) Suspend(ctx context.Context, userID string) error {
	now := time.Now()
	if err := s.store.SetUserBillingStatus(userID, "suspended", nil, &now); err != nil {
		return err
	}
	// The hard cutoff (stop servers + drop route-only link ACLs) is deferred to the
	// enforcement pass in runOnce, which fires once suspended_at + suspendGrace has
	// elapsed. Recording state + notifying only here means a transient billing/DB
	// fault (or a buggy manual suspend) cannot instantly kick a paying customer, and
	// enforcement is single-sourced in the leader-gated ticker. ctx is retained for
	// the unchanged caller signature.
	s.sendSuspendedEmail(userID)
	return nil
}

// SuspendNow immediately, synchronously suspends a tenant for the ADMIN-manual
// path: unlike the graced Suspend (deferred cutoff via enforceSuspensions,
// used by the automatic non-payment lifecycle and the store webhook), this is
// a one-click hard kill for fraud/abuse. It records the suspended state +
// sends the same email as Suspend, stops the tenant's running servers
// synchronously, and DURABLY revokes every one of the tenant's route-only
// link kits via RevokeLinkKitTeardown (revoked_at first, then ACL, then
// tunnel key, then routes - the same ordering RevokeLinkKit uses) so they are
// immediately and permanently down. No grace-window resurrection, and no
// change to the three grace predicates (reconciler SQL, enforceSuspensions,
// handlers.linkHardSuspended all stay as-is): a durably revoked link is
// already excluded by all three. See Reactivate's doc comment for what
// happens to these links on reactivation.
func (s *BillingLifecycleService) SuspendNow(ctx context.Context, userID string) error {
	now := time.Now()
	if err := s.store.SetUserBillingStatus(userID, "suspended", nil, &now); err != nil {
		return err
	}
	s.sendSuspendedEmail(userID)
	s.stopTenantServers(ctx, userID)
	if s.provisioner == nil || s.gateway == nil || s.redis == nil {
		return nil // solo/hoster mode: no link kits to revoke
	}
	revokeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, linkID := range s.linkKitsForUser(userID) {
		if _, rerr := RevokeLinkKitTeardown(revokeCtx, s.store, s.gateway, s.redis, s.provisioner, linkID, userID); rerr != nil {
			log.Printf("billing lifecycle: force-suspend %s: revoke link %s: %v", userID, linkID, rerr)
		}
	}
	return nil
}

// stopTenantServers is the cutoff both suspension paths rely on: the deferred
// one (enforceSuspensions) and the admin-immediate one (SuspendNow).
//
// It must flip desired_state to "stopped" BEFORE queueing the stop, not just
// queue it. The node runs a reconciler that restarts any container that is not
// running while its desired_state is "online", treating it as a crash. Queueing
// a bare stop therefore did not suspend anything: the container went down and
// came straight back, observed live as
//
//	13:35:52 Server <uuid> stopped
//	13:35:57 reconciler: restarting crashed container mc_<uuid> (attempt 1/5)
//
// so a non-paying tenant kept their servers with a five-second blip, and the
// panel still read "online" because nothing wrote the row either.
//
// This is the same rule the rest of the codebase already follows and states -
// PowerAction sets desired_state=stopped then sends "stop", the migration
// orchestrator's stopServer says so in its doc comment, and DeleteSubServer
// spells the race out in full. This path was the one place that only sent the
// command.
//
// The status write is "stopping", matching PowerAction: the node's own status
// publish moves it to "stopped" when the container is actually down. No
// "suspended" server status is introduced here - that value has no producer
// anywhere in the codebase and adding one is a UI-wide change, not a fix.
func (s *BillingLifecycleService) stopTenantServers(ctx context.Context, userID string) {
	servers, err := s.store.ListServersByOwner(userID)
	if err != nil {
		log.Printf("billing lifecycle: list servers for %s: %v", userID, err)
		return
	}
	for _, srv := range servers {
		if srv.Status != "online" {
			continue
		}
		node, err := s.store.GetNodeByID(srv.NodeID)
		if err != nil {
			continue
		}
		// Before the command: the reconciler reads desired_state, and losing the
		// race means the stop is undone rather than merely delayed.
		if err := s.store.UpdateServerDesiredState(srv.ID, "stopped"); err != nil {
			log.Printf("billing lifecycle: desired_state for %s: %v — NOT sending the stop, the node reconciler would restart it", srv.UUID, err)
			continue
		}
		if err := s.store.UpdateServerStatus(srv.ID, "stopping"); err != nil {
			log.Printf("billing lifecycle: status for %s: %v", srv.UUID, err)
		}
		if err := s.queue.SendCommand(ctx, node.Token, "stop", map[string]interface{}{"uuid": srv.UUID}, nil); err != nil {
			log.Printf("billing lifecycle: stop %s: %v", srv.UUID, err)
		}
	}
}

// --- emails (best-effort; SMTP misconfig never blocks the lifecycle) ---

// PaymentURL is the configured payment link the panel banner + emails point at.
func (s *BillingLifecycleService) PaymentURL() string { return s.paymentURL() }

func (s *BillingLifecycleService) paymentURL() string {
	v, _ := s.store.GetSetting(BillingPaymentURLKey)
	if v != "" {
		return v
	}
	return strings.TrimRight(s.frontendURL, "/") + "/account/billing"
}

func (s *BillingLifecycleService) sendDunningEmail(userID string, graceUntil time.Time) {
	u, err := s.store.GetUserByID(userID)
	if err != nil || u == nil || u.Email == "" {
		return
	}
	transport, err := mailer.Load(s.store, "auth")
	if err != nil {
		return
	}
	body := fmt.Sprintf(`Hi %s,

We could not process the payment for your Dylaris services.

Everything keeps running for now, but your services will be SUSPENDED on %s if payment is not completed.

Pay here to keep your services active:

%s

Your data is safe either way — nothing is deleted when you miss a payment.

— Dylaris
`, u.Username, graceUntil.UTC().Format("2006-01-02 15:04 UTC"), s.paymentURL())
	if err := transport.Send(mailer.Message{To: u.Email, Subject: "Payment required — your Dylaris services", Body: body}); err != nil {
		logErrf("billing-lifecycle", "dunning mail to %s failed: %v", u.Email, err)
	}
}

func (s *BillingLifecycleService) sendSuspendedEmail(userID string) {
	u, err := s.store.GetUserByID(userID)
	if err != nil || u == nil || u.Email == "" {
		return
	}
	transport, err := mailer.Load(s.store, "auth")
	if err != nil {
		return
	}
	body := fmt.Sprintf(`Hi %s,

Your Dylaris account has been suspended for non-payment. Your servers and services will stop after a grace period if the issue is not resolved. Your data and backups are kept and remain viewable.

Pay here to reactivate:

%s

— Dylaris
`, u.Username, s.paymentURL())
	if err := transport.Send(mailer.Message{To: u.Email, Subject: "Your Dylaris services are suspended", Body: body}); err != nil {
		logErrf("billing-lifecycle", "suspended mail to %s failed: %v", u.Email, err)
	}
}

// linkKitsForUser returns the tenant's non-revoked route-only link identities.
func (s *BillingLifecycleService) linkKitsForUser(userID string) []string {
	keys, err := s.store.ListWarpAPIKeysByOwner(userID)
	if err != nil {
		log.Printf("billing lifecycle: list link kits for %s: %v", userID, err)
		return nil
	}
	var ids []string
	for _, k := range keys {
		if strings.HasPrefix(k.NodeID, "link-") {
			ids = append(ids, k.NodeID)
		}
	}
	return ids
}

// suspendTenantLinks drops each of the tenant's route-only links (ACL user +
// tunnel key), killing their live tunnels. It does NOT revoke the warp key or
// delete routes, so Reactivate can bring the same links back.
func (s *BillingLifecycleService) suspendTenantLinks(ctx context.Context, userID string) {
	if s.provisioner == nil || s.gateway == nil || s.redis == nil {
		return
	}
	// The hourly ticker calls Suspend with context.Background(); a hung Redis call
	// would stall that goroutine indefinitely. Bound it, as reactivate already does.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, linkID := range s.linkKitsForUser(userID) {
		tunnelToken := s.gateway.LinkToken(linkID)
		s.provisioner.RemoveRouteOnlyLinkACL(ctx, linkID)
		if err := s.redis.Del(ctx, "link:"+tunnelToken).Err(); err != nil {
			log.Printf("billing lifecycle: suspend link %s: delete tunnel key: %v", linkID, err)
		}
	}
}

// reactivateTenantLinks restores each link's scoped Redis ACL (identical derived
// password, so the link's pool re-authenticates on its next reconnect) and re-adds
// its tunnel key, so the edge accepts the tunnel again within seconds. No restart
// needed: the link's ConnectionManager retries on its own.
func (s *BillingLifecycleService) reactivateTenantLinks(userID string) {
	if s.provisioner == nil || s.gateway == nil || s.redis == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, linkID := range s.linkKitsForUser(userID) {
		tunnelToken := s.gateway.LinkToken(linkID)
		if _, _, err := s.provisioner.EnsureRouteOnlyLinkACL(ctx, s.clusterSecret, linkID, tunnelToken); err != nil {
			log.Printf("billing lifecycle: reactivate link %s: ensure ACL: %v", linkID, err)
			continue
		}
		if err := s.redis.Set(ctx, "link:"+tunnelToken, "valid", 24*time.Hour).Err(); err != nil {
			log.Printf("billing lifecycle: reactivate link %s: set tunnel key: %v", linkID, err)
		}
	}
}
