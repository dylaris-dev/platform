package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
)

// ErrGatewayOwnDomainExtras refuses voice chat and Bedrock on a customer's own
// domain on the managed gateway: not offered yet.
var ErrGatewayOwnDomainExtras = errors.New("voice chat and Bedrock are not available on an own domain")

// VoiceDomainFor is the remote-mode twin of VoiceRouteFor: the lexically first
// of domains exactly one label under an ONLINE region's wildcard, and that
// region. It goes through VoiceRegionFor, so both modes share one rule; an own
// domain is under no wildcard and never qualifies.
func VoiceDomainFor(domains []string, regions []remotegw.Region) (domain, region string, ok bool) {
	edges := make([]GatewayEdgeInfo, 0, len(regions))
	for _, r := range regions {
		if r.Online {
			edges = append(edges, GatewayEdgeInfo{Region: r.Region, Wildcard: r.Wildcard, Status: "online"})
		}
	}
	sorted := append([]string(nil), domains...)
	sort.Strings(sorted)
	for _, d := range sorted {
		if region, ok := VoiceRegionFor(d, edges); ok {
			return d, region, true
		}
	}
	return "", "", false
}

// ServerOwnDomains maps each of the server's addresses (minus pending deletes)
// to whether it sits on the customer's own domain.
func (g *RemoteGateway) ServerOwnDomains(serverUUID string) (map[string]bool, error) {
	routes, err := g.serverRoutes(serverUUID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(routes))
	for _, r := range routes {
		out[r.Domain] = r.CustomDomain
	}
	return out, nil
}

// serverRoute is the server's row for domain, or an error when the server
// holds no such row (the handler checked; this is a race with a delete).
func (g *RemoteGateway) serverRoute(serverUUID, domain string) (*store.RemoteRoute, error) {
	row, err := g.st.GetRemoteRoute(domain)
	if err != nil {
		return nil, err
	}
	if row == nil || row.ServerUUID != serverUUID {
		return nil, fmt.Errorf("%s is not an address of this server", domain)
	}
	return row, nil
}

// extrasPreflight is what every synchronous ON checks before the provider is
// asked: no own domain, no change waiting for the address (a queued OFF that
// runs after this ON would leave the provider off while the row says on), and
// the row touched, so the sweep can tell this flip in flight from drift.
func (g *RemoteGateway) extrasPreflight(row *store.RemoteRoute) error {
	if row.CustomDomain {
		return ErrGatewayOwnDomainExtras
	}
	if pending, err := g.st.HasOpenOutbox(row.Domain, ""); err != nil {
		return err
	} else if pending {
		return ErrGatewayChangePending
	}
	return g.st.TouchRemoteRoute(row.Domain)
}

// ServerVoice is the server's voice address from the local columns.
func (g *RemoteGateway) ServerVoice(serverUUID string) (domain string, port int, mod string, on bool, err error) {
	routes, err := g.serverRoutes(serverUUID)
	if err != nil {
		return "", 0, "", false, err
	}
	for _, r := range routes {
		if r.VoicePort != nil {
			return r.Domain, *r.VoicePort, r.VoiceMod, true, nil
		}
	}
	return "", 0, "", false, nil
}

// EnableServerVoice turns the provider's voice relay on for domain and records
// its port locally once the provider said yes. A failed local write leaves the
// provider on and the row off: voice reads as off here, a retry keeps the
// provider's port, and the sweep switches the provider off.
func (g *RemoteGateway) EnableServerVoice(ctx context.Context, serverUUID, domain, mod string) (int, error) {
	row, err := g.serverRoute(serverUUID, domain)
	if err != nil {
		return 0, err
	}
	if err := g.extrasPreflight(row); err != nil {
		return 0, err
	}
	// Not the caller's context: a closed tab must not cut the call off between
	// the provider's commit and the local write.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remoteCallTimeout)
	defer cancel()
	defer g.routes.drop()
	v, err := g.api().SetVoice(ctx, domain, true, VoiceInternalPort)
	if err != nil {
		return 0, fmt.Errorf("voice chat for %s: %w", domain, err)
	}
	if !v.Enabled || v.Port <= 0 {
		return 0, fmt.Errorf("%w: voice chat for %s answered without a port", remotegw.ErrBadAnswer, domain)
	}
	port := v.Port
	found, err := g.st.SetRemoteRouteVoice(domain, &port, mod)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("voice chat for %s: the route row is gone", domain)
	}
	return port, nil
}

// DisableServerVoice clears every voice column of the server, then queues the
// provider's OFF. Never refused; a failed enqueue is reported, not returned,
// since the column is what turns voice off here and the sweep queues it again.
func (g *RemoteGateway) DisableServerVoice(serverUUID string) (changed bool, err error) {
	routes, err := g.st.ListRemoteRoutesByServer(serverUUID)
	if err != nil {
		return false, err
	}
	for _, r := range routes {
		if r.VoicePort == nil {
			continue
		}
		if _, err := g.st.SetRemoteRouteVoice(r.Domain, nil, ""); err != nil {
			return changed, err
		}
		changed = true
		if err := g.st.EnqueueOutbox(OutboxOpVoice, r.Domain, OutboxVoice{Enabled: false}); err != nil {
			logErrf("gateway-remote", "voice chat off for %s not queued for the provider: %v", r.Domain, err)
		}
	}
	if changed {
		g.routes.drop()
	}
	return changed, nil
}

// ServerEditions maps each of the server's addresses (minus pending deletes)
// to its local edition: "" Java only, "both" or "bedrock".
func (g *RemoteGateway) ServerEditions(serverUUID string) (map[string]string, error) {
	routes, err := g.serverRoutes(serverUUID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(routes))
	for _, r := range routes {
		out[r.Domain] = r.Edition
	}
	return out, nil
}

func editionOf(java, bedrock bool) string {
	switch {
	case bedrock && java:
		return "both"
	case bedrock:
		return "bedrock"
	}
	return ""
}

// AddBedrock gives domain Bedrock on the provider and stores the provider's
// Floodgate key for the server. The address is reserved locally FIRST, so the
// one-Bedrock-address index refuses a second address (ErrRemoteBedrockTaken)
// before the provider is asked. Any failure after the reservation puts the
// column back; the provider may then be left with Bedrock on, which a retry
// reuses and the sweep switches off. The caller publishes the key.
func (g *RemoteGateway) AddBedrock(ctx context.Context, serverID int, serverUUID, domain string, java bool) (keyChanged bool, err error) {
	row, err := g.serverRoute(serverUUID, domain)
	if err != nil {
		return false, err
	}
	if err := g.extrasPreflight(row); err != nil {
		return false, err
	}
	prev := row.Edition
	if _, err := g.st.SetRemoteRouteEdition(domain, editionOf(java, true)); err != nil {
		return false, err
	}
	undo := func() {
		if _, err := g.st.SetRemoteRouteEdition(domain, prev); err != nil {
			logErrf("gateway-remote", "Bedrock reservation of %s not taken back after a failed enable: %v", domain, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remoteCallTimeout)
	defer cancel()
	defer g.routes.drop()
	api := g.api()
	if _, err := api.SetBedrock(ctx, domain, java, true); err != nil && !bedrockStoredAnyway(ctx, api, domain, java, err) {
		undo()
		return false, fmt.Errorf("Bedrock for %s: %w", domain, err)
	}
	key, err := api.FloodgateKey(ctx, domain)
	if err != nil {
		undo()
		return false, fmt.Errorf("Floodgate key for %s: %w", domain, err)
	}
	changed, err := g.st.SetBedrockKey(serverID, key)
	if err != nil {
		undo()
		return false, err
	}
	return changed, nil
}

// bedrockStoredAnyway: the provider answers 502 when it stored the editions but
// could not tell its gateway yet (it does within a minute), and an answer that
// never arrived may hide a commit too. For those its route list is the truth;
// a definite refusal is not looked up. A list that cannot be read, or one that
// says Java only (its shape when its own table read fails), counts as not
// stored: the column goes back and a retry, which the provider takes again,
// finishes it.
func bedrockStoredAnyway(ctx context.Context, api *remotegw.Client, domain string, java bool, err error) bool {
	if !remotegw.Retryable(err) {
		return false
	}
	routes, lerr := api.Routes(ctx)
	if lerr != nil {
		return false
	}
	for _, r := range routes {
		if r.Domain == domain {
			return r.Bedrock != nil && r.Bedrock.Bedrock && r.Bedrock.Java == java
		}
	}
	return false
}

// SetEditionWithoutAdding records a removal of Bedrock or a Java change, then
// queues it for the provider. Never refused; an unchanged edition queues
// nothing.
func (g *RemoteGateway) SetEditionWithoutAdding(serverUUID, domain string, java, bedrock bool) error {
	row, err := g.serverRoute(serverUUID, domain)
	if err != nil {
		return err
	}
	edition := editionOf(java, bedrock)
	if row.Edition == edition {
		return nil
	}
	if _, err := g.st.SetRemoteRouteEdition(domain, edition); err != nil {
		return err
	}
	g.routes.drop()
	if err := g.st.EnqueueOutbox(OutboxOpBedrock, domain, OutboxBedrock{Java: java, Bedrock: bedrock}); err != nil {
		logErrf("gateway-remote", "editions of %s not queued for the provider: %v", domain, err)
	}
	return nil
}

// routeExtrasPerSweep bounds the voice and Bedrock re-asserts one sweep sends.
// ponytail: a flat 10 per replica and sweep, in orderForExtras' order (at most
// half on unchecked Bedrock keys, the rest in domain order from a cursor that
// moves on after a pass that spent it all). A provider whose list reads every
// address as off costs 10 writes per 5 minute sweep, not one per address,
// against the key's 30 writes a minute; more mismatched addresses heal 10 per
// sweep. Raise it, or size it by the number of rows, if that ever is too slow.
const routeExtrasPerSweep = 10

// reconcileRouteExtras keeps row's voice and Bedrock in step with p, the
// provider's listing of the same address. The caller checked that the row is
// present there, has no open outbox item and was not written moments ago.
//
// The listing is never acted on alone: the provider lists voice off and Java
// only when its own table read fails. A local ON it contradicts is re-asserted
// (an idempotent PUT, within budget) and only that answer decides: the same
// port or key changes nothing, another one or a final refusal switches the
// feature off here and queues it off there. No answer about the address (5xx,
// 429, a rejected key, a refusal about the moment) changes nothing. A provider
// ON the record lacks is queued off. A nil half (an older provider) is
// skipped. The row is read again before any decision and before any write,
// and left alone once it changed: a switch flipped meanwhile wins.
func reconcileRouteExtras(ctx context.Context, st store.Store, c *remotegw.Client, publishKey func(ctx context.Context, uuid string, key []byte) (changed bool, err error), row store.RemoteRoute, p remotegw.Route, srv models.Server, budget *int) error {
	if p.Voice == nil && p.Bedrock == nil {
		return nil
	}
	fresh, err := extrasRowAsRead(st, row)
	if err != nil || fresh == nil {
		return err
	}
	// One change per address and sweep: the next one starts from the row
	// that change wrote.
	if p.Voice != nil {
		if acted, err := reconcileExtrasVoice(ctx, st, c, *fresh, *p.Voice, srv, budget); acted || err != nil {
			return err
		}
	}
	if p.Bedrock != nil {
		return reconcileExtrasBedrock(ctx, st, c, publishKey, *fresh, *p.Bedrock, srv, budget)
	}
	return nil
}

// extrasRowAsRead is row read again, or nil when it is gone, differs from
// row, or was written moments ago (a switch being flipped right now).
func extrasRowAsRead(st store.Store, row store.RemoteRoute) (*store.RemoteRoute, error) {
	again, err := st.GetRemoteRoute(row.Domain)
	if err != nil || again == nil {
		return nil, err
	}
	sameVoice := (again.VoicePort == nil) == (row.VoicePort == nil) && (again.VoicePort == nil || *again.VoicePort == *row.VoicePort)
	if !again.UpdatedAt.Equal(row.UpdatedAt) || !sameVoice || again.VoiceMod != row.VoiceMod || again.Edition != row.Edition {
		return nil, nil
	}
	if writtenMomentsAgo(*again) {
		return nil, nil
	}
	return again, nil
}

// reconcileExtrasVoice: acted is true once the address was changed or queued.
func reconcileExtrasVoice(ctx context.Context, st store.Store, c *remotegw.Client, row store.RemoteRoute, pv remotegw.RouteVoice, srv models.Server, budget *int) (acted bool, err error) {
	if row.VoicePort == nil {
		if !pv.Enabled {
			return false, nil
		}
		return true, st.EnqueueOutbox(OutboxOpVoice, row.Domain, OutboxVoice{Enabled: false})
	}
	// A read error changes nothing: a database blip must not switch voice off
	// on every server here.
	if allowed, _, err := ResolveVoiceAllowed(st, &srv); err == nil && !allowed {
		return true, switchVoiceOff(st, row, "voice chat is no longer allowed for this server here")
	}
	if pv.Enabled && pv.Port == *row.VoicePort {
		// Agreeing is healthy: a strike from an earlier fault is over.
		forgetAnswer("voice", row.Domain)
		return false, nil
	}
	if *budget <= 0 {
		return false, nil
	}
	*budget--
	cctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	v, err := c.SetVoice(cctx, row.Domain, true, VoiceInternalPort)
	cancel()
	switch {
	case err == nil && v.Enabled && v.Port == *row.VoicePort:
		forgetAnswer("voice", row.Domain)
		return false, nil
	case err == nil && v.Enabled && v.Port > 0:
		if !answeredTwice("voice", row, "port "+strconv.Itoa(v.Port)) {
			return true, nil
		}
		return true, switchVoiceOff(st, row, fmt.Sprintf("the gateway provider has given it another voice port (%d, not %d)", v.Port, *row.VoicePort))
	case err == nil:
		forgetAnswer("voice", row.Domain)
		return false, fmt.Errorf("voice chat of %s: %w: on without a port", row.Domain, remotegw.ErrBadAnswer)
	case voiceRefusalIsFinal(err):
		if !answeredTwice("voice", row, providerErrText(err)) {
			return true, nil
		}
		return true, switchVoiceOff(st, row, providerErrText(err))
	}
	forgetAnswer("voice", row.Domain)
	holdExtrasBudget(err, budget)
	return false, fmt.Errorf("voice chat of %s not confirmed: %w", row.Domain, err)
}

func reconcileExtrasBedrock(ctx context.Context, st store.Store, c *remotegw.Client, publishKey func(ctx context.Context, uuid string, key []byte) (changed bool, err error), row store.RemoteRoute, pb remotegw.RouteBedrock, srv models.Server, budget *int) error {
	if row.Edition == "" {
		if pb.Bedrock || !pb.Java {
			return st.EnqueueOutbox(OutboxOpBedrock, row.Domain, OutboxBedrock{Java: true, Bedrock: false})
		}
		return nil
	}
	java := row.Edition == "both"
	stored, hasKey, err := st.GetBedrockKey(srv.ID)
	if err != nil {
		return err
	}
	if pb.Bedrock && pb.Java == java {
		forgetAnswer("bedrock", row.Domain)
		// Both sides agree, yet the key here can be missing or an earlier
		// address's: a Core that died between the provider's yes and storing
		// the key leaves exactly that, and Floodgate then refuses every
		// Bedrock player. Each row's key is compared once per process (and
		// again after the row or the key changes), so a restart, which is
		// what follows such a death, checks every one.
		if hasKey && bedrockKeyIsChecked(row, stored) {
			return nil
		}
		if *budget <= 0 {
			return nil
		}
		*budget--
		cctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
		key, err := c.FloodgateKey(cctx, row.Domain)
		cancel()
		switch {
		case err != nil:
			holdExtrasBudget(err, budget)
			return fmt.Errorf("Floodgate key of %s not checked: %w", row.Domain, err)
		case hasKey && bytes.Equal(key, stored):
			bedrockKeyChecked.Store(row.Domain, bedrockKeyFingerprint(row, stored))
			return nil
		}
		return repairBedrockKey(ctx, st, publishKey, row, srv, key)
	}
	if *budget <= 0 {
		return nil
	}
	*budget--
	cctx, cancel := context.WithTimeout(ctx, remoteCallTimeout)
	defer cancel()
	var key []byte
	if _, err = c.SetBedrock(cctx, row.Domain, java, true); err == nil {
		key, err = c.FloodgateKey(cctx, row.Domain)
	}
	if !bedrockRefusalIsFinal(err) {
		forgetAnswer("bedrock", row.Domain)
	}
	switch {
	case err == nil && hasKey && bytes.Equal(key, stored):
		bedrockKeyChecked.Store(row.Domain, bedrockKeyFingerprint(row, stored))
		if publishKey == nil {
			return nil
		}
		_, err := publishKey(ctx, srv.UUID, stored)
		return err
	case err == nil:
		// The provider has Bedrock on with its key (none stored here, or the
		// address was created again there): adopting it keeps the players
		// able to join after one restart (review ruling, replaces "off").
		return repairBedrockKey(ctx, st, publishKey, row, srv, key)
	case bedrockRefusalIsFinal(err):
		if !answeredTwice("bedrock", row, providerErrText(err)) {
			return nil
		}
		return switchBedrockOff(st, row, providerErrText(err))
	}
	holdExtrasBudget(err, budget)
	return fmt.Errorf("Bedrock of %s not confirmed: %w", row.Domain, err)
}

// bedrockKeyChecked maps a domain to the fingerprint of the row and stored key
// last found equal to the provider's (bedrockKeyFingerprint). In memory on
// purpose: an empty map after a restart or a leader change is what makes the
// sweep compare every key once.
var bedrockKeyChecked sync.Map

// bedrockKeyFingerprint ties a check to the row's last write and the key; a
// hash, never the key, so nothing secret sits in the map.
func bedrockKeyFingerprint(row store.RemoteRoute, key []byte) string {
	sum := sha256.Sum256(key)
	return strconv.FormatInt(row.UpdatedAt.UnixNano(), 10) + ":" + hex.EncodeToString(sum[:])
}

func bedrockKeyIsChecked(row store.RemoteRoute, key []byte) bool {
	v, ok := bedrockKeyChecked.Load(row.Domain)
	return ok && v == bedrockKeyFingerprint(row, key)
}

// repairBedrockKey stores and publishes the provider's key for a row whose own
// key is missing or stale, unless the row changed meanwhile. Floodgate reads
// the key at server start, so when the node's copy was missing or different
// the owner has to restart the server; the log line on the Errors screen says
// so. A node copy that already was this key needs no restart and no line on
// the Errors screen. The key is never logged.
func repairBedrockKey(ctx context.Context, st store.Store, publishKey func(ctx context.Context, uuid string, key []byte) (changed bool, err error), row store.RemoteRoute, srv models.Server, key []byte) error {
	if again, err := extrasRowAsRead(st, row); err != nil || again == nil {
		return err
	}
	if _, err := st.SetBedrockKey(srv.ID, key); err != nil {
		return err
	}
	bedrockKeyChecked.Store(row.Domain, bedrockKeyFingerprint(row, key))
	changed, err := true, error(nil)
	if publishKey != nil {
		changed, err = publishKey(ctx, srv.UUID, key)
	}
	if !changed && err == nil {
		log.Printf("[gateway-outbox] the Floodgate key for %s was missing here and was taken from the gateway provider; the server already has it", row.Domain)
		return nil
	}
	logErrf("gateway-outbox", "the Floodgate key for %s was missing or stale here and was taken from the gateway provider; restart server %s so Bedrock players can join", row.Domain, srv.UUID)
	return err
}

// voiceRefusalIsFinal: the provider runs every voice check again on a re-assert,
// so a refusal about the moment (link or relay down, a link being replaced, a
// full pool, an edge list it could not read) says nothing about the address.
// Neither does 404: the sweep only asks about an address the provider listed
// moments ago, so "not found" contradicts that list (and an older provider
// answers a failed table read that way). Only the account rule's "no" is final.
func voiceRefusalIsFinal(err error) bool {
	return remotegw.IsCode(err, "voice_not_allowed")
}

// bedrockRefusalIsFinal: the account rule's "no" (bedrock_disabled). The
// provider gives its other add-path refusals for the moment (no Bedrock entry
// in the region, a link being replaced), and 404 contradicts the list as for
// voice.
func bedrockRefusalIsFinal(err error) bool {
	return remotegw.IsCode(err, "bedrock_disabled")
}

// holdExtrasBudget ends this sweep's re-asserts on an answer every other one
// would get too: a rejected key, a rate limit, an account refusal.
func holdExtrasBudget(err error, budget *int) {
	var ae *remotegw.APIError
	if providerRejectsAll(err) || (errors.As(err, &ae) && (ae.Status == http.StatusTooManyRequests || ae.Status == http.StatusForbidden)) {
		*budget = 0
	}
}

// switchVoiceOff clears the row's voice and queues the provider's off, unless
// the row changed since it was read. The cleared column is what the panel
// shows; the log line tells the owner what to do. A failed enqueue is healed
// by the next sweep, which finds the provider on and the row off.
func switchVoiceOff(st store.Store, row store.RemoteRoute, why string) error {
	if again, err := extrasRowAsRead(st, row); err != nil || again == nil {
		return err
	}
	if _, err := st.SetRemoteRouteVoice(row.Domain, nil, ""); err != nil {
		return err
	}
	logErrf("gateway-outbox", "voice chat was switched off for %s: %s; switch it on again in the panel", row.Domain, why)
	return st.EnqueueOutbox(OutboxOpVoice, row.Domain, OutboxVoice{Enabled: false})
}

// switchBedrockOff is switchVoiceOff for Bedrock: Java only here and there.
func switchBedrockOff(st store.Store, row store.RemoteRoute, why string) error {
	if again, err := extrasRowAsRead(st, row); err != nil || again == nil {
		return err
	}
	if _, err := st.SetRemoteRouteEdition(row.Domain, ""); err != nil {
		return err
	}
	logErrf("gateway-outbox", "Bedrock was switched off for %s: %s; switch it on again in the panel, then restart the server", row.Domain, why)
	return st.EnqueueOutbox(OutboxOpBedrock, row.Domain, OutboxBedrock{Java: true, Bedrock: false})
}

// extrasNow is the sweep's clock for the two-answer rule; tests move it.
var extrasNow = time.Now

// extrasVerdict is a final answer the provider gave for one feature of one
// address, about the row as it was then (stamp).
type extrasVerdict struct {
	answer string
	stamp  time.Time
	first  time.Time
}

// extrasVerdictsMax bounds the pending verdicts of one process.
// ponytail: past it the map is cleared, which only restarts every count.
const extrasVerdictsMax = 10000

// extrasVerdictMaxAge: a first answer older than this (three sweeps) no
// longer counts, so one fault long ago cannot be the first half of a pair.
const extrasVerdictMaxAge = 3 * outboxSweepEvery

var (
	extrasMu       sync.Mutex
	extrasVerdicts = map[string]extrasVerdict{}
	// extrasWalkCursor: the pass after one that spent the whole budget starts
	// after the last address that spent some, so no ten addresses that keep
	// getting no answer hold the budget for the rest.
	extrasWalkCursor string
)

// answeredTwice records the provider's final answer for feature of row and
// reports whether the same answer came for the unchanged row at least one full
// sweep interval earlier. Only then is the feature switched off: one answer
// may be the provider's fault, two a sweep apart are its state. In memory, so
// a restart or a leader change only restarts the count.
func answeredTwice(feature string, row store.RemoteRoute, answer string) bool {
	k := feature + " " + row.Domain
	now := extrasNow()
	extrasMu.Lock()
	defer extrasMu.Unlock()
	if v, ok := extrasVerdicts[k]; ok && v.answer == answer && v.stamp.Equal(row.UpdatedAt) && now.Sub(v.first) <= extrasVerdictMaxAge {
		if now.Sub(v.first) >= outboxSweepEvery {
			delete(extrasVerdicts, k)
			return true
		}
		return false
	}
	if len(extrasVerdicts) >= extrasVerdictsMax {
		clear(extrasVerdicts)
	}
	extrasVerdicts[k] = extrasVerdict{answer: answer, stamp: row.UpdatedAt, first: now}
	return false
}

// forgetAnswer drops a pending verdict: the provider answered otherwise.
func forgetAnswer(feature, domain string) {
	extrasMu.Lock()
	delete(extrasVerdicts, feature+" "+domain)
	extrasMu.Unlock()
}

// resetExtrasSweep forgets everything the sweep keeps in memory (tests).
func resetExtrasSweep() {
	bedrockKeyChecked.Clear()
	extrasMu.Lock()
	clear(extrasVerdicts)
	extrasWalkCursor = ""
	extrasMu.Unlock()
}

// writtenMomentsAgo: the row may be a switch being flipped right now. The age
// is the database's own (NOW() - updated_at), so no replica clock is compared
// with the database's; a store that cannot tell falls back to updated_at.
func writtenMomentsAgo(r store.RemoteRoute) bool {
	if r.AgeKnown {
		return r.Age < 2*remoteCallTimeout
	}
	return !r.UpdatedAt.IsZero() && time.Since(r.UpdatedAt) < 2*remoteCallTimeout
}

// orderForExtras is the order of one pass over the rows: first the Bedrock
// rows whose key this process has not compared yet (no stored key, then the
// newest rows: what a death in the middle of an enable leaves; at most half
// the budget), then all other rows in domain order starting after the cursor.
func orderForExtras(st store.Store, rows []store.RemoteRoute, servers map[string]models.Server) []store.RemoteRoute {
	sorted := append([]store.RemoteRoute(nil), rows...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].Domain < sorted[b].Domain })
	extrasMu.Lock()
	cursor := extrasWalkCursor
	extrasMu.Unlock()
	start := sort.Search(len(sorted), func(i int) bool { return sorted[i].Domain > cursor })
	walk := append(append([]store.RemoteRoute(nil), sorted[start:]...), sorted[:start]...)
	var missing, unchecked, rest []store.RemoteRoute
	for _, r := range walk {
		srv, ok := servers[r.ServerUUID]
		if _, checked := bedrockKeyChecked.Load(r.Domain); r.Edition == "" || checked || !ok {
			continue
		}
		if _, has, err := st.GetBedrockKey(srv.ID); err == nil && !has {
			missing = append(missing, r)
		} else {
			unchecked = append(unchecked, r)
		}
	}
	sort.SliceStable(unchecked, func(a, b int) bool { return unchecked[a].UpdatedAt.After(unchecked[b].UpdatedAt) })
	// Half the budget at most: a row whose check keeps failing is never
	// marked checked and would stay in front of every pass, so the rest of
	// the addresses must keep their own share and their rotation.
	first := append(missing, unchecked...)
	if len(first) > routeExtrasPerSweep/2 {
		first = first[:routeExtrasPerSweep/2]
	}
	taken := make(map[string]bool, len(first))
	for _, r := range first {
		taken[r.Domain] = true
	}
	for _, r := range walk {
		if !taken[r.Domain] {
			rest = append(rest, r)
		}
	}
	return append(first, rest...)
}

// advanceExtrasCursor ends a pass: after one that used the whole budget the
// next starts after lastSpent, otherwise at the beginning.
func advanceExtrasCursor(budgetLeft int, lastSpent string) {
	extrasMu.Lock()
	defer extrasMu.Unlock()
	if budgetLeft <= 0 {
		extrasWalkCursor = lastSpent
	} else {
		extrasWalkCursor = ""
	}
}
