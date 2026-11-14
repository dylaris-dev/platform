package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"dylaris-core/models"
	"dylaris-core/pkg/leader"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
	"dylaris-pkg/validate"
)

const (
	outboxTick       = 5 * time.Second
	outboxBatch      = 20
	outboxBackoffMin = 10 * time.Second
	outboxBackoffMax = 10 * time.Minute
	// A refusal (403, 409, ...) fails the same way until somebody changes
	// something, so it is retried slowly and stays visible as open.
	outboxRefusalRetry = 10 * time.Minute
	// A rejected key fails every call the same way: stop, try again in a minute.
	outboxAuthRetry  = 60 * time.Second
	outboxSweepEvery = 5 * time.Minute
	outboxKeepDone   = 7 * 24 * time.Hour
	// Half the store's 2-minute claim lease: a batch still running after that
	// hands the rest back before another claimer could take the same rows.
	outboxTickBudget = time.Minute
)

// StartGatewayOutbox drains the gateway outbox on the leader while the remote
// provider is on. With remote off (or the mode unknown) it does nothing and
// leaves the rows: a mode flip never sends or deletes anything. rg supplies the
// provider's own-domain claims to the sweep (cached) and has that cache dropped
// when a claim release lands; nil skips own-domain rows in the sweep.
func StartGatewayOutbox(ctx context.Context, st store.Store, api func() *remotegw.Client, el leader.Election, mode func() (remote, known bool), rg *RemoteGateway) {
	l := &outboxLoop{st: st, api: api, el: el, mode: mode, now: time.Now, rg: rg}
	go func() {
		t := time.NewTicker(outboxTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				l.tick(ctx)
			}
		}
	}()
}

type outboxLoop struct {
	st   store.Store
	api  func() *remotegw.Client
	el   leader.Election
	mode func() (remote, known bool)
	now  func() time.Time
	rg   *RemoteGateway
	// Touched only by the loop goroutine.
	nextSweep time.Time
	// pauseUntil: the provider rejected the key or rate-limited us; nothing is
	// sent before then, whatever else is due.
	pauseUntil time.Time
}

func (l *outboxLoop) tick(ctx context.Context) {
	if l.el != nil && !l.el.IsLeader() {
		l.nextSweep = time.Time{}
		return
	}
	if remote, known := l.mode(); !remote || !known {
		l.nextSweep = time.Time{}
		return
	}
	now := l.now()
	if now.Before(l.pauseUntil) {
		return
	}
	c := l.api()
	if _, _, pause := drainOutboxOnce(ctx, l.st, c, l.el, l.now, l.rg); !pause.IsZero() {
		l.pauseUntil = pause
		return
	}
	// The sweep runs on the first active tick, so switching back to remote
	// reconciles at once.
	if now.Before(l.nextSweep) {
		return
	}
	l.nextSweep = now.Add(outboxSweepEvery)
	if _, err := l.st.PurgeDoneOutbox(now.Add(-outboxKeepDone)); err != nil {
		log.Printf("[gateway-outbox] purge: %v", err)
	}
	// Before the routes: a removed kit's routes died with it on the provider,
	// and the route repair below would try to put them back on it.
	if n, err := reconcileRemoteKits(ctx, l.st, c, now); err != nil {
		log.Printf("[gateway-outbox] link kits: %v", err)
	} else if n > 0 {
		log.Printf("[gateway-outbox] forgot %d link kit(s) the provider has removed", n)
	}
	adopted, repaired, foreign, err := reconcileRemoteRoutes(ctx, l.st, c, l.rg)
	if err != nil {
		log.Printf("[gateway-outbox] reconcile: %v", err)
	} else if adopted+repaired > 0 {
		log.Printf("[gateway-outbox] reconcile: adopted %d, repaired %d, left %d foreign", adopted, repaired, foreign)
	}
	if n, err := releaseOrphanProviderClaims(ctx, l.st, l.rg); err != nil {
		log.Printf("[gateway-outbox] orphaned provider claims: %s", providerErrText(err))
	} else if n > 0 {
		log.Printf("[gateway-outbox] queued the release of %d provider claim(s) no user here holds", n)
	}
}

// orphanReleasesPerSweep bounds the provider claim releases one sweep queues.
const orphanReleasesPerSweep = 20

// releaseOrphanProviderClaims queues the release of provider claims that no
// user here holds a live local claim on: what a release finished as in use, a
// release that could not be queued, or a route delete without one leaves. Each
// takes a slot of the provider's bound for the whole platform account.
// ReleaseProviderClaim keeps its own guards (a route row, a queued release).
// Nothing is released when the claim list or a local count cannot be read.
func releaseOrphanProviderClaims(ctx context.Context, st store.Store, rg *RemoteGateway) (queued int, err error) {
	if rg == nil {
		return 0, nil
	}
	claims, err := providerClaims(ctx, rg)
	if err != nil {
		return 0, err
	}
	for domain := range claims {
		if queued >= orphanReleasesPerSweep {
			break
		}
		n, err := st.CountLiveCustomDomainClaims(domain, "")
		if err != nil {
			return queued, err
		}
		if n > 0 {
			continue
		}
		open, err := st.HasOpenOutbox(domain, OutboxOpDeleteCustomDomain)
		if err != nil {
			return queued, err
		}
		if open {
			continue
		}
		if err := rg.ReleaseProviderClaim(domain, ""); err != nil {
			return queued, err
		}
		if now, err := st.HasOpenOutbox(domain, OutboxOpDeleteCustomDomain); err == nil && now {
			queued++
		}
	}
	return queued, nil
}

// reconcileRemoteRoutes repairs drift between remote_routes and the provider.
// A provider route to mc-<uuid> of a local server with no row is adopted (the
// Core died between the provider's create and the insert). A row the provider
// lacks is put again. A row whose server is gone gets its delete queued. Any
// other provider route is someone's own and only counted. A route that sits on
// another node's kit than its server's (a move while the gateway was local, or
// a refused move) gets the move queued, and one whose real-IP switch differs
// from its server's gets the switch queued. A domain with an open outbox item
// is left to the drain.
//
// A route on an own domain (not under a provider hoster domain) also needs its
// server owner's VERIFIED local claim on that exact domain; without it the
// delete is queued instead of a repair or a move, and a provider route of ours
// on such a domain is deleted rather than adopted. A repair the provider
// refuses for the domain itself (domain_unverified, domain_blocked) queues the
// delete too. The provider's claim is checked BEFORE any repair or move, from
// rg's cached claim list: its route gate re-proves a lapsed claim live, so a
// repair after its lapse would bring the route back whenever the record shows
// again. A claim that is not verified there gets the delete; a list that could
// not be read (or no rg) leaves own-domain rows alone for this pass. An
// ownerless server (the operator's own) needs no local claim; the provider's
// claim still applies.
func reconcileRemoteRoutes(ctx context.Context, st store.Store, c *remotegw.Client, rg *RemoteGateway) (adopted, repaired, foreign int, err error) {
	// The rows first: with the provider list read first, a route another
	// leader's drain deleted (provider route, then row) in between was adopted
	// back from the stale list. The repair below re-reads its row for the
	// mirror-image race.
	local, err := st.ListRemoteRoutes()
	if err != nil {
		return 0, 0, 0, err
	}
	lctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	provider, err := c.Routes(lctx)
	cancel()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("list provider routes: %w", err)
	}
	servers, err := st.ListAllServers()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("list servers: %w", err)
	}
	kits, err := st.ListRemoteKits()
	if err != nil {
		return 0, 0, 0, err
	}
	serverByUUID := make(map[string]models.Server, len(servers))
	serverByHost := make(map[string]string, len(servers))
	for _, s := range servers {
		serverByUUID[s.UUID] = s
		if h := validate.ServerHostname(s.UUID); h != "" {
			serverByHost[h] = s.UUID
		}
	}
	nodeByLink := make(map[string]uint, len(kits))
	kitByNode := make(map[uint]store.RemoteKit, len(kits))
	for _, k := range kits {
		nodeByLink[k.LinkID] = k.NodeID
		kitByNode[k.NodeID] = k
	}
	onProvider := make(map[string]remotegw.Route, len(provider))
	for _, p := range provider {
		onProvider[p.Domain] = p
	}

	// The provider's own-domain claims, read once per pass and only when an
	// own-domain row needs them.
	var claims map[string]remotegw.CustomDomain
	var claimsErr error
	claimsRead, claimsFresh := false, false
	hasRow := make(map[string]bool, len(local))
	for _, r := range local {
		hasRow[r.Domain] = true
		gone := false
		srv, listed := serverByUUID[r.ServerUUID]
		owner := srv.OwnerID
		if !listed {
			// The list skips rows it cannot scan, so absence there is not proof.
			s, err := st.GetServerByUUID(r.ServerUUID)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				gone = true
			case err != nil:
				log.Printf("[gateway-outbox] route %s skipped: server %s lookup: %v", r.Domain, r.ServerUUID, err)
				continue
			default:
				owner = s.OwnerID
			}
		}
		if gone {
			// The server's own delete did not get queued; queue it now.
			queued, err := st.HasOpenOutbox(r.Domain, OutboxOpDelete)
			if err != nil {
				return adopted, repaired, foreign, err
			}
			if !queued {
				if err := st.EnqueueOutbox(OutboxOpDelete, r.Domain, struct{}{}); err != nil {
					return adopted, repaired, foreign, err
				}
			}
			continue
		}
		p, present := onProvider[r.Domain]
		pending, err := st.HasOpenOutbox(r.Domain, "")
		if err != nil {
			return adopted, repaired, foreign, err
		}
		if pending {
			continue
		}
		// Claimed or written moments ago: its create may still be on its way
		// to the provider, and a put from here (repair or drift move) could
		// race the create's own undo and leave a route nothing here knows.
		if !r.UpdatedAt.IsZero() && time.Since(r.UpdatedAt) < 2*remoteCallTimeout {
			continue
		}
		if r.CustomDomain {
			held, err := ownDomainClaimHeld(st, owner, r.Domain)
			if err != nil {
				log.Printf("[gateway-outbox] route %s skipped: local claim lookup: %v", r.Domain, err)
				continue
			}
			if !held {
				if err := queueOwnDomainDelete(st, r.Domain, r.ServerUUID, "the server's owner holds no verified claim on this domain here"); err != nil {
					return adopted, repaired, foreign, err
				}
				continue
			}
			if !claimsRead {
				claims, claimsErr = providerClaims(ctx, rg)
				claimsRead = true
				if claimsErr != nil {
					log.Printf("[gateway-outbox] own-domain routes skipped this pass: provider claims: %s", providerErrText(claimsErr))
				}
			}
			if claimsErr != nil {
				continue
			}
			pc, ok := claims[r.Domain]
			if (!ok || pc.State != store.ClaimVerified) && !claimsFresh {
				// The list is cached per replica, and a verify or create on
				// another replica drops only that one's copy: a "pending" read
				// here may be a minute old and the route just proven. Deletes are
				// decided on a list read now, once per pass.
				rg.domains.drop()
				claims, claimsErr = providerClaims(ctx, rg)
				claimsFresh = true
				if claimsErr != nil {
					log.Printf("[gateway-outbox] own-domain routes skipped this pass: provider claims: %s", providerErrText(claimsErr))
					continue
				}
				pc, ok = claims[r.Domain]
			}
			if !ok && present {
				// The provider refuses to release a claim a live route uses, so
				// a route without one is a list that does not belong to these
				// routes (another provider account's key, a partial answer).
				logErrf("gateway-outbox", "address %s left alone this pass: the gateway provider lists its route but no claim on the domain", r.Domain)
				continue
			}
			if !ok || pc.State != store.ClaimVerified {
				if err := queueOwnDomainDelete(st, r.Domain, r.ServerUUID, "the gateway provider's claim on the domain is not verified"); err != nil {
					return adopted, repaired, foreign, err
				}
				continue
			}
		}
		if present {
			// A server that could not be listed is not compared: there is
			// nothing to compare it with.
			if listed {
				if err := queueRouteDrift(st, r, p, srv, kitByNode); err != nil {
					return adopted, repaired, foreign, err
				}
			}
			continue
		}
		// Deleted by another leader's drain since the read above: putting it
		// back would undo that delete.
		if again, err := st.GetRemoteRoute(r.Domain); err != nil || again == nil {
			continue
		}
		kit, err := st.GetRemoteKit(r.NodeID)
		if err != nil {
			return adopted, repaired, foreign, err
		}
		host := validate.ServerHostname(r.ServerUUID)
		if kit == nil || host == "" {
			log.Printf("[gateway-outbox] route %s not repaired: node %d has no link kit or the server id is no gateway target", r.Domain, r.NodeID)
			continue
		}
		// The provider takes nothing onto a link being replaced and would
		// refuse this on every sweep.
		if kit.RetireAt != nil {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
		err = c.PutRoute(pctx, kit.LinkID, r.Domain, host, r.Port)
		cancel()
		if err != nil {
			if remotegw.Retryable(err) || providerRejectsAll(err) {
				return adopted, repaired, foreign, fmt.Errorf("repair %s: %w", r.Domain, err)
			}
			if r.CustomDomain && providerRefusesDomain(err) {
				if qerr := queueOwnDomainDelete(st, r.Domain, r.ServerUUID, "the gateway provider no longer accepts the domain ("+providerErrText(err)+")"); qerr != nil {
					return adopted, repaired, foreign, qerr
				}
				continue
			}
			msg := routeErrText(err, r.CustomDomain)
			if providerRefusesDomain(err) {
				// A hoster-domain row stays; the refusal would repeat on every
				// sweep, so it is reported once per text.
				if prev, seen := reconcileRepairLogged.Swap(r.Domain, msg); seen && prev == msg {
					continue
				}
			}
			logErrf("gateway-outbox", "route %s not repaired: %s", r.Domain, msg)
			continue
		}
		// The provider's put does not carry the real-IP switch.
		if r.ProxyProtocol {
			if err := st.EnqueueOutbox(OutboxOpProxy, r.Domain, OutboxProxy{Enabled: true}); err != nil {
				logErrf("gateway-outbox", "real-IP switch for repaired route %s not queued: %v", r.Domain, err)
			}
		}
		repaired++
	}

	// The hoster domains are read once per pass, and only when a route is up
	// for adoption; a failed read ends the pass like a failed route list.
	var hosters []remotegw.HosterDomain
	hostersRead := false
	for _, p := range provider {
		if hasRow[p.Domain] {
			continue
		}
		uuid, ours := serverByHost[p.TargetHost]
		node, kitOK := nodeByLink[p.LinkID]
		if !ours || !kitOK {
			foreign++
			continue
		}
		pending, err := st.HasOpenOutbox(p.Domain, "")
		if err != nil {
			return adopted, repaired, foreign, err
		}
		if pending {
			continue
		}
		if !hostersRead {
			rctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
			regions, err := c.Regions(rctx)
			cancel()
			if err != nil {
				return adopted, repaired, foreign, fmt.Errorf("list provider hoster domains: %w", err)
			}
			hosters, hostersRead = regions.HosterDomains, true
		}
		custom := OwnDomain(p.Domain, hosters)
		if custom {
			held, err := ownDomainClaimHeld(st, serverByUUID[uuid].OwnerID, p.Domain)
			if err != nil {
				log.Printf("[gateway-outbox] route %s not adopted: local claim lookup: %v", p.Domain, err)
				continue
			}
			if !held {
				if err := queueOwnDomainDelete(st, p.Domain, uuid, "it is on an own domain the server's owner holds no verified claim on here"); err != nil {
					return adopted, repaired, foreign, err
				}
				continue
			}
		}
		// A claim, never an overwrite: a row written since the read above
		// belongs to a create that won the address, and a provider list older
		// than that create must not hand it back to anyone.
		row := store.RemoteRoute{Domain: p.Domain, ServerUUID: uuid, NodeID: node, Port: p.TargetPort, ProxyProtocol: p.ProxyProtocol, CustomDomain: custom}
		claimed, _, err := st.ClaimRemoteRoute(row)
		if err != nil {
			return adopted, repaired, foreign, err
		}
		if !claimed {
			continue
		}
		adopted++
		if err := queueRouteDrift(st, row, p, serverByUUID[uuid], kitByNode); err != nil {
			return adopted, repaired, foreign, err
		}
	}
	return adopted, repaired, foreign, nil
}

// ownDomainClaimHeld reports whether ownerID holds a verified local claim on
// exactly domain. An ownerless server is the operator's own and needs none.
func ownDomainClaimHeld(st store.Store, ownerID, domain string) (bool, error) {
	if ownerID == "" {
		return true, nil
	}
	c, err := st.GetCustomDomainClaim(ownerID, domain)
	if errors.Is(err, store.ErrNoClaim) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return c.State == store.ClaimVerified, nil
}

// providerClaims is rg's cached own-domain claim list; no rg is a failure, so
// own-domain rows are left alone rather than judged against nothing.
func providerClaims(ctx context.Context, rg *RemoteGateway) (map[string]remotegw.CustomDomain, error) {
	if rg == nil {
		return nil, errors.New("no gateway provider claims source")
	}
	cctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	defer cancel()
	return rg.ProviderCustomDomains(cctx)
}

// providerRefusesDomain: the provider's route gate refused the domain itself.
func providerRefusesDomain(err error) bool {
	return remotegw.IsCode(err, "domain_unverified") || remotegw.IsCode(err, "domain_blocked")
}

// providerErrText is an error without the provider's message when it is a
// refusal: the one for an unproven domain spells out this platform's TXT value
// for it. Status and code still match ListRefusedOutbox's pattern.
func providerErrText(err error) string {
	var ae *remotegw.APIError
	if errors.As(err, &ae) {
		return fmt.Sprintf("gateway provider answered %d %s", ae.Status, ae.Code)
	}
	return err.Error()
}

// routeErrText is the text logged and stored for a failed change to a route:
// without the provider's message on an own domain.
func routeErrText(err error, ownDomain bool) string {
	if ownDomain {
		return providerErrText(err)
	}
	return err.Error()
}

// queueOwnDomainDelete queues the delete of a route on an own domain that has
// lost its proof, and reports it once, naming server and domain. The caller
// has checked no item is open for the domain.
func queueOwnDomainDelete(st store.Store, domain, serverUUID, why string) error {
	if err := st.EnqueueOutbox(OutboxOpDelete, domain, struct{}{}); err != nil {
		return err
	}
	logErrf("gateway-outbox", "address %s of server %s removed: %s", domain, serverUUID, why)
	return nil
}

// remoteKitLostAge protects a kit another replica minted a moment ago: the
// provider list read here may be older than its mint.
const remoteKitLostAge = 10 * time.Minute

// reconcileRemoteKits makes the local kit rows follow the provider's kit list,
// read fresh once per sweep and only when there are rows:
//
//   - A row without retire_at whose kit the provider lists as retiring gets the
//     provider's deadline (a retire whose answer or local stamp was lost).
//   - A row whose kit the provider no longer lists is forgotten once its
//     retire deadline has passed, or once it is older than remoteKitLostAge:
//     the provider removed it, and its routes with it. The node's route rows
//     go too, except one with an open outbox item, which the drain settles (a
//     queued move re-homes it). EnsureKit then mints a fresh kit for the node.
//
// Nothing is forgotten unless the list call succeeded.
func reconcileRemoteKits(ctx context.Context, st store.Store, c *remotegw.Client, now time.Time) (forgotten int, err error) {
	local, err := st.ListRemoteKits()
	if err != nil || len(local) == 0 {
		return 0, err
	}
	kctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	provider, err := c.Kits(kctx)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("list provider kits: %w", err)
	}
	listed := make(map[string]remotegw.Kit, len(provider))
	for _, k := range provider {
		listed[k.LinkID] = k
	}
	var routes []store.RemoteRoute
	routesRead := false
	for _, k := range local {
		if p, ok := listed[k.LinkID]; ok {
			if k.RetireAt == nil && p.RetireAt != nil {
				if err := st.SetRemoteKitRetireAt(k.NodeID, k.LinkID, *p.RetireAt); err != nil {
					return forgotten, err
				}
				log.Printf("[gateway-outbox] node %d's link %s is being replaced on the provider until %s; recorded here", k.NodeID, k.LinkID, p.RetireAt.UTC().Format(time.RFC3339))
			}
			continue
		}
		retired := k.RetireAt != nil && !now.Before(*k.RetireAt)
		// A zero CreatedAt is a row that could not say its age: not old.
		lost := !retired && !k.CreatedAt.IsZero() && now.Sub(k.CreatedAt) >= remoteKitLostAge
		if !retired && !lost {
			continue
		}
		if !routesRead {
			if routes, err = st.ListRemoteRoutes(); err != nil {
				return forgotten, err
			}
			routesRead = true
		}
		for _, r := range routes {
			if r.NodeID != k.NodeID {
				continue
			}
			open, err := st.HasOpenOutbox(r.Domain, "")
			if err != nil {
				return forgotten, err
			}
			if open {
				continue
			}
			if err := st.DeleteRemoteRoute(r.Domain); err != nil {
				return forgotten, err
			}
			logErrf("gateway-outbox", "address %s of server %s removed: it was still on node %d's link %s, which the gateway provider has removed with its addresses; create it again", r.Domain, r.ServerUUID, k.NodeID, k.LinkID)
		}
		if err := st.DeleteRemoteKit(k.NodeID); err != nil {
			return forgotten, err
		}
		if lost {
			logErrf("gateway-outbox", "the gateway provider no longer has node %d's link %s; deploy a new one", k.NodeID, k.LinkID)
		}
		forgotten++
	}
	return forgotten, nil
}

// reconcileNoKitLogged keeps the "node has no kit" drift report to once per
// node and process; it would otherwise repeat every sweep.
var reconcileNoKitLogged sync.Map

// reconcileRepairLogged keeps a refused repair's report to once per domain,
// process and error text.
var reconcileRepairLogged sync.Map

// queueRouteDrift queues what makes the provider route p follow the row's
// server srv: the move MigrateServerRoutes would have queued when the route
// sits on another node's kit than the server's or targets another server, and
// the server's real-IP switch when the route carries the other one. The ROW is
// the truth; the provider route is never copied into it. An empty LinkID is the
// provider not knowing the kit, not a mismatch. The caller has checked no item
// is open for the domain.
func queueRouteDrift(st store.Store, row store.RemoteRoute, p remotegw.Route, srv models.Server, kitByNode map[uint]store.RemoteKit) error {
	node := uint(srv.NodeID)
	kit, hasKit := kitByNode[node]
	host := validate.ServerHostname(srv.UUID)
	if node != row.NodeID || (hasKit && p.LinkID != "" && p.LinkID != kit.LinkID) || p.TargetHost != host {
		switch {
		case !hasKit:
			// Minting needs the node's name and the gateway's kit cache; the
			// next route create or move on that node mints it, and the sweep
			// after that queues the move.
			if _, seen := reconcileNoKitLogged.LoadOrStore(node, true); !seen {
				log.Printf("[gateway-outbox] route %s sits on node %d's link, its server is on node %d, which has no link kit yet; not moved", row.Domain, row.NodeID, node)
			}
		case kit.RetireAt != nil:
			// A link being replaced takes no route; the provider would refuse
			// the move on every sweep.
		case host != "":
			if err := st.EnqueueOutbox(OutboxOpMove, row.Domain, OutboxMove{LinkID: kit.LinkID, NodeID: node, TargetHost: host, Port: row.Port}); err != nil {
				return err
			}
		}
	}
	if srv.ProxyProtocol != p.ProxyProtocol {
		if err := st.EnqueueOutbox(OutboxOpProxy, row.Domain, OutboxProxy{Enabled: srv.ProxyProtocol}); err != nil {
			return err
		}
	}
	return nil
}

// drainOutboxOnce claims up to one batch and replays it. Items of one domain
// run in id order: the claim skips an item while an older one for its domain
// is open, and the drain checks again. pauseUntil is set when the provider
// refuses everything for now (rejected key, rate limit); the caller then sends
// nothing before it. el may be nil (always leader).
func drainOutboxOnce(ctx context.Context, st store.Store, c *remotegw.Client, el leader.Election, clock func() time.Time, rg *RemoteGateway) (done, failed int, pauseUntil time.Time) {
	now := clock()
	items, err := st.ClaimDueOutbox(now, outboxBatch)
	if err != nil {
		logErrf("gateway-outbox", "claim: %v", err)
		return 0, 0, time.Time{}
	}
	for _, it := range items {
		if !pauseUntil.IsZero() {
			deferOutbox(st, it, pauseUntil)
			continue
		}
		if ctx.Err() != nil {
			return // the lease brings the rest back
		}
		// Stay inside the lease and stop the moment leadership is lost; the
		// untried rest is due again at once, no attempt counted.
		if clock().Sub(now) > outboxTickBudget || (el != nil && !el.IsLeader()) {
			deferOutbox(st, it, now)
			continue
		}
		oldest, err := st.OldestOpenOutboxID(it.Domain)
		if err != nil || oldest != it.ID {
			deferOutbox(st, it, now)
			continue
		}
		if it.Op == OutboxOpMove || it.Op == OutboxOpProxy {
			// A newer delete makes the change pointless; finishing it lets the
			// delete through instead of waiting behind a refusal.
			if deleting, err := st.HasOpenOutbox(it.Domain, OutboxOpDelete); err != nil {
				deferOutbox(st, it, now)
				continue
			} else if deleting {
				finishOutbox(st, it)
				done++
				continue
			}
		}
		if it.Op == OutboxOpDeleteCustomDomain && rg != nil {
			// Under the platform's own-domain write ceiling, as the handlers'
			// claim writes are: not tried, so no attempt is counted.
			if ok, at := rg.takeClaimWrite(false); !ok {
				deferOutbox(st, it, at)
				continue
			}
		}
		err = applyOutbox(ctx, st, c, it)
		if err == nil {
			finishOutbox(st, it)
			if it.Op == OutboxOpDeleteCustomDomain && rg != nil {
				rg.domains.drop()
			}
			done++
			continue
		}
		if ctx.Err() != nil {
			return
		}
		ownDomain := it.Op == OutboxOpDeleteCustomDomain
		if it.Op == OutboxOpMove || it.Op == OutboxOpProxy || it.Op == OutboxOpDelete {
			if row, rerr := st.GetRemoteRoute(it.Domain); rerr == nil && row != nil && row.CustomDomain {
				ownDomain = true
				// The provider no longer takes the domain (its claim lapsed):
				// every retry would be the same refusal, or a live re-proof
				// that undoes the lapse. The route goes instead.
				if it.Op != OutboxOpDelete && providerRefusesDomain(err) {
					finishOutbox(st, it)
					done++
					if deleting, derr := st.HasOpenOutbox(it.Domain, OutboxOpDelete); derr == nil && !deleting {
						if qerr := queueOwnDomainDelete(st, it.Domain, row.ServerUUID, "the gateway provider no longer accepts the domain ("+providerErrText(err)+")"); qerr != nil {
							logErrf("gateway-outbox", "delete of %s not queued after the provider refused the domain: %v", it.Domain, qerr)
						}
					}
					continue
				}
			}
		}
		failed++
		msg := routeErrText(err, ownDomain)
		var ae *remotegw.APIError
		isAPI := errors.As(err, &ae)
		// Waits count from the answer, not from the batch start: late in a
		// long batch, a Retry-After measured from the start has already run out.
		at := clock()
		switch {
		case providerRejectsAll(err):
			pauseUntil = at.Add(outboxAuthRetry)
			failOutbox(st, it, msg, pauseUntil)
			logOutboxOnce(it, msg)
		case remotegw.Retryable(err):
			wait := outboxBackoff(it.Attempts)
			if isAPI && ae.RetryAfter > wait {
				wait = ae.RetryAfter
			}
			failOutbox(st, it, msg, at.Add(wait))
			if isAPI && ae.Status == http.StatusTooManyRequests {
				// The rest would hit the same window; they did not try, so
				// they come back when it ends rather than after a backoff.
				pause := ae.RetryAfter
				if pause <= 0 {
					pause = outboxBackoffMin
				}
				pauseUntil = at.Add(pause)
			}
		default:
			failOutbox(st, it, msg, at.Add(outboxRefusalRetry))
			logOutboxOnce(it, msg)
		}
	}
	return done, failed, pauseUntil
}

func finishOutbox(st store.Store, it store.OutboxItem) {
	if err := st.FinishOutbox(it.ID); err != nil {
		// Replayed after the lease; every op is idempotent.
		log.Printf("[gateway-outbox] finish %d: %v", it.ID, err)
	}
}

// providerRejectsAll: no key, a rejected key or an unusable URL fails every
// call the same way.
func providerRejectsAll(err error) bool {
	var ae *remotegw.APIError
	return errors.Is(err, remotegw.ErrNoKey) || errors.Is(err, remotegw.ErrBaseURL) ||
		(errors.As(err, &ae) && ae.Status == http.StatusUnauthorized)
}

func outboxBackoff(attempts int) time.Duration {
	d := outboxBackoffMin
	for i := 0; i < attempts && d < outboxBackoffMax; i++ {
		d *= 2
	}
	return min(d, outboxBackoffMax)
}

// logOutboxOnce reports a stuck item when its error first appears, not on
// every retry.
func logOutboxOnce(it store.OutboxItem, msg string) {
	if it.LastError != msg {
		logErrf("gateway-outbox", "%s %s not applied: %s", it.Op, it.Domain, msg)
	}
}

func failOutbox(st store.Store, it store.OutboxItem, msg string, next time.Time) {
	if err := st.FailOutbox(it.ID, msg, next); err != nil {
		log.Printf("[gateway-outbox] fail %d: %v", it.ID, err)
	}
}

func deferOutbox(st store.Store, it store.OutboxItem, next time.Time) {
	if err := st.DeferOutbox(it.ID, next); err != nil {
		log.Printf("[gateway-outbox] defer %d: %v", it.ID, err)
	}
}

// applyOutbox sends one item and then makes the local row follow. A failed row
// change returns an error, so the item is replayed; the provider calls are
// idempotent.
func applyOutbox(ctx context.Context, st store.Store, c *remotegw.Client, it store.OutboxItem) error {
	cctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	defer cancel()
	switch it.Op {
	case OutboxOpDelete:
		if err := c.DeleteRoute(cctx, it.Domain); err != nil {
			return err
		}
		return st.DeleteRemoteRoute(it.Domain)
	case OutboxOpMove:
		var p OutboxMove
		if err := json.Unmarshal(it.Payload, &p); err != nil {
			return fmt.Errorf("bad move payload: %w", err)
		}
		if err := c.PutRoute(cctx, p.LinkID, it.Domain, p.TargetHost, p.Port); err != nil {
			return err
		}
		return updateRemoteRoute(st, it.Domain, func(r *store.RemoteRoute) { r.NodeID = p.NodeID })
	case OutboxOpProxy:
		var p OutboxProxy
		if err := json.Unmarshal(it.Payload, &p); err != nil {
			return fmt.Errorf("bad proxy payload: %w", err)
		}
		// not_found: the provider has no such route, so there is nothing to
		// switch; reconcile puts the route back and queues the switch again.
		// A code-less 404 is not that answer and stays a refusal.
		if err := c.SetProxyProtocol(cctx, it.Domain, p.Enabled); err != nil && !remotegw.IsCode(err, "not_found") {
			return err
		}
		return updateRemoteRoute(st, it.Domain, func(r *store.RemoteRoute) { r.ProxyProtocol = p.Enabled })
	case OutboxOpRevokeKit:
		// Already gone counts as done.
		if err := c.RevokeKit(cctx, it.Domain); err != nil && !remotegw.IsCode(err, "link_not_found") && !remotegw.IsCode(err, "not_found") {
			return err
		}
		return nil
	case OutboxOpDeleteCustomDomain:
		// No such claim is done, and so is domain_in_use: a route on the
		// domain still uses the claim, so keeping it is right. Left open, the
		// item would block every later change to the domain.
		err := c.DeleteCustomDomain(cctx, it.Domain)
		if remotegw.IsCode(err, "domain_in_use") {
			log.Printf("[gateway-outbox] provider claim on %s kept: a route on it is still in use", it.Domain)
			return nil
		}
		return err
	default:
		return fmt.Errorf("unknown outbox op %q", it.Op)
	}
}

// updateRemoteRoute changes the local row; a row that is gone needs nothing.
func updateRemoteRoute(st store.Store, domain string, change func(*store.RemoteRoute)) error {
	r, err := st.GetRemoteRoute(domain)
	if err != nil || r == nil {
		return err
	}
	change(r)
	return st.UpsertRemoteRoute(*r)
}
