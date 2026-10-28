package services

// Access to a pack bound to a Modrinth project comes from that project's
// Modrinth team (owner decision 2026-10-28; panel collaborators are gone).
// Each user's own connected token answers "what may I do there"; the answer is
// cached per user and project, because Modrinth allows 300 requests per minute
// per IP and every panel user shares Core's.
//
//   - fresh (< 5 min): used as is
//   - older: asked again; if Modrinth cannot be reached, the last answer holds
//     for up to an hour, then the user is refused until it answers
//   - token rejected (401), lacking a read scope, disconnected, or the project
//     answering 404 (a draft hides itself from such a token): state
//     token_invalid - READ access to what the user had, for a day after the
//     last real "member" answer, then nothing until they renew the token
//
// The day is the bound on a removed member: a draft answers 404 to everyone
// outside its team, so "removed" and "token lapsed" look the same from here.
//
// Every caller - pack handlers, server installs, background alerts - resolves
// through MembershipFor, so none of them trusts a cached answer longer than
// these rules allow.

import (
	"context"
	"errors"
	"log"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

const (
	MembershipFresh   = 5 * time.Minute
	MembershipStale   = time.Hour
	TokenInvalidGrace = 24 * time.Hour
	MembershipMember  = "member"
	MembershipNone    = "none"
	MembershipLapsed  = "token_invalid"
)

// PATLoader loads and decrypts a user's Modrinth PAT.
type PATLoader interface {
	LoadPAT(userID string) (pat string, username string, err error)
}

var packPATs PATLoader

// SetPackPATLoader wires the PAT loader for callers outside the pack handlers.
func SetPackPATLoader(p PATLoader) { packPATs = p }

// LapsedReadable reports whether a token_invalid membership still grants read.
func LapsedReadable(m *models.ModrinthMembership) bool {
	return m != nil && m.State == MembershipLapsed && m.ConfirmedAt != nil && time.Since(*m.ConfirmedAt) < TokenInvalidGrace
}

// MembershipFor returns the user's current membership in the project,
// refreshing it from Modrinth when the cached answer is older than 5 minutes.
// nil means "no access and nothing known".
func MembershipFor(ctx context.Context, st store.Store, pats PATLoader, userID, projectID string) *models.ModrinthMembership {
	cached, err := st.GetModrinthMembership(userID, projectID)
	if err != nil {
		log.Printf("packs: membership %s/%s not read: %v", userID, projectID, err)
		return nil
	}
	if cached != nil && time.Since(cached.CheckedAt) < MembershipFresh {
		return cached
	}
	fresh, err := AskModrinthMembership(ctx, pats, userID, projectID, cached)
	if err != nil {
		// Modrinth did not answer: keep the last answer for a while, never forever.
		if cached != nil && time.Since(cached.CheckedAt) < MembershipStale {
			return cached
		}
		log.Printf("packs: membership %s/%s unknown: %v", userID, projectID, err)
		return nil
	}
	if err := st.UpsertModrinthMembership(fresh); err != nil {
		log.Printf("packs: membership %s/%s not stored: %v", userID, projectID, err)
	}
	return fresh
}

// AskModrinthMembership asks Modrinth with the user's own token. An error
// means "Modrinth did not answer"; a rejected or missing token is an answer
// (token_invalid), keeping the last known permissions and confirmation.
func AskModrinthMembership(ctx context.Context, pats PATLoader, userID, projectID string, cached *models.ModrinthMembership) (*models.ModrinthMembership, error) {
	lapsed := func(listed bool) (*models.ModrinthMembership, error) {
		m := &models.ModrinthMembership{UserID: userID, ProjectID: projectID, State: MembershipLapsed}
		if cached != nil {
			m.Permissions, m.ConfirmedAt = cached.Permissions, cached.ConfirmedAt
		}
		if !listed && (cached == nil || cached.State == MembershipNone) {
			m.State, m.Permissions = MembershipNone, 0 // nothing to keep visible
		}
		return m, nil
	}
	if pats == nil {
		return lapsed(false)
	}
	pat, _, err := pats.LoadPAT(userID)
	if err != nil {
		return lapsed(false)
	}
	mc := NewModrinthClient(pat)
	modrinthUser, _, err := mc.CurrentUser(ctx)
	if ModrinthTokenRejected(err) {
		return lapsed(false)
	}
	if err != nil {
		return nil, err
	}
	perms, member, err := mc.ProjectPermissionsFor(ctx, projectID, modrinthUser)
	if errors.Is(err, ErrModrinthScopeMissing) {
		// Modrinth listed the user in the team and only hid their rights: a
		// member for sure, shown read only even with nothing cached.
		m, _ := lapsed(true)
		now := time.Now()
		m.ConfirmedAt = &now
		return m, nil
	}
	// 404 is what a draft answers a token without PROJECT_READ (and what a
	// deleted project answers everyone): keep what the user had, read only.
	if ModrinthTokenRejected(err) || ModrinthNotFound(err) {
		return lapsed(false)
	}
	if err != nil {
		return nil, err
	}
	m := &models.ModrinthMembership{UserID: userID, ProjectID: projectID, State: MembershipNone}
	if member {
		now := time.Now()
		m.State, m.Permissions, m.ConfirmedAt = MembershipMember, perms, &now
	}
	return m, nil
}

// PackReachableBy answers "may this user use this pack" for callers outside
// the pack handlers (background alerts, server installs). A pack bound to a
// Modrinth project follows its team; an unbound one is its creator's.
// verified=true demands a live membership; false also accepts a lapsed token
// within its grace (read access).
func PackReachableBy(ctx context.Context, st store.Store, p *models.Pack, userID string, verified bool) (bool, error) {
	if p == nil || userID == "" {
		return false, nil
	}
	if p.ModrinthProjectID == "" {
		return p.OwnerID == userID, nil
	}
	m := MembershipFor(ctx, st, packPATs, userID, p.ModrinthProjectID)
	if m == nil {
		return false, nil
	}
	if m.State == MembershipMember {
		return true, nil
	}
	return !verified && LapsedReadable(m), nil
}
