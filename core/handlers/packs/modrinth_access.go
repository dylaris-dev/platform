package packs

// The rules for access through a Modrinth team live in
// services/pack_access.go, shared with server installs and alerts; this file
// maps them onto the pack handlers' needs.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/services"
)

const (
	stateMember       = services.MembershipMember
	stateNone         = services.MembershipNone
	stateTokenInvalid = services.MembershipLapsed
)

// needBits is the Modrinth permission a need requires (any bit of it).
func needBits(need packNeed) int64 {
	switch need {
	case packEdit:
		return services.ModrinthPermUploadVersion
	case packDelete:
		return services.ModrinthPermDeleteVersion
	case packEditPage:
		return services.ModrinthPermEditDetails | services.ModrinthPermEditBody
	case packDeletePack:
		return services.ModrinthPermDeleteProject
	}
	return 0
}

// allows reports whether a membership grants need.
func allows(m *models.ModrinthMembership, need packNeed) bool {
	if m == nil {
		return false
	}
	switch m.State {
	case stateMember:
		bits := needBits(need)
		return bits == 0 || m.Permissions&bits != 0
	case stateTokenInvalid:
		return need == packRead && services.LapsedReadable(m) // greyed out until renewed
	}
	return false
}

func (h *PacksHandler) membershipFor(ctx context.Context, userID, projectID string) *models.ModrinthMembership {
	return services.MembershipFor(ctx, h.state.Store, h.patLoader(), userID, projectID)
}

func (h *PacksHandler) askModrinthMembership(ctx context.Context, userID, projectID string, cached *models.ModrinthMembership) (*models.ModrinthMembership, error) {
	return services.AskModrinthMembership(ctx, h.patLoader(), userID, projectID, cached)
}

// patLoader keeps a nil loader a nil interface (a typed nil would look wired).
func (h *PacksHandler) patLoader() services.PATLoader {
	if h.pat == nil {
		return nil
	}
	return h.pat
}

// grantMembership records a membership Core just proved (the caller created
// or imported the project with their own token).
func (h *PacksHandler) grantMembership(userID, projectID string, perms int64) {
	now := time.Now()
	if err := h.state.Store.UpsertModrinthMembership(&models.ModrinthMembership{UserID: userID, ProjectID: projectID, State: stateMember, Permissions: perms, ConfirmedAt: &now}); err != nil {
		log.Printf("packs: membership %s/%s not stored: %v", userID, projectID, err)
	}
}

// discoverTeamPacks finds packs the user can reach through a Modrinth team
// they joined since the last look: their projects (own team and
// organizations) that are bound to a pack here and have no cached answer yet.
// At most every services.MembershipFresh per user and Core replica.
func (h *PacksHandler) discoverTeamPacks(ctx context.Context, userID string) {
	h.teamScanMu.Lock()
	if h.teamScans == nil {
		h.teamScans = map[string]time.Time{}
	}
	if last, ok := h.teamScans[userID]; ok && time.Since(last) < services.MembershipFresh {
		h.teamScanMu.Unlock()
		return
	}
	h.teamScans[userID] = time.Now()
	h.teamScanMu.Unlock()
	if h.pat == nil {
		return
	}
	pat, _, err := h.pat.LoadPAT(userID)
	if err != nil {
		return
	}
	mc := services.NewModrinthClient(pat)
	projects, err := memberModpacks(ctx, mc)
	if err != nil {
		return
	}
	for _, p := range projects {
		pack, err := h.state.Store.GetPackByModrinthProject(p.ID)
		if err != nil || pack == nil {
			continue
		}
		if cached, _ := h.state.Store.GetModrinthMembership(userID, p.ID); cached == nil {
			h.membershipFor(ctx, userID, p.ID)
		}
	}
}

// ClearModrinthMemberships DELETE /api/me/modrinth/memberships - forgets every
// pack the caller cannot currently prove access to (token rejected, or no
// longer in the team): they leave the caller's list. The packs themselves and
// their builds stay; they belong to the Modrinth team.
func (h *PacksHandler) ClearModrinthMemberships(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	n, err := h.state.Store.ClearModrinthMemberships(userID)
	if err != nil {
		handlers.SendJSONError(w, "Could not clear", http.StatusInternalServerError)
		return
	}
	h.teamScanMu.Lock()
	delete(h.teamScans, userID)
	h.teamScanMu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "removed": n})
}
