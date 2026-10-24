package packs

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/handlers"

	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// Modpack collaboration: an owner lets other users work on their packs. See
// docs/superpowers/specs/2026-10-24-modpack-collaboration-design.md.

type packCollaboratorRequest struct {
	Username  string `json:"username"`
	PackIDs   []int  `json:"packIds"` // empty = every pack, including future ones
	CanCreate bool   `json:"canCreate"`
	CanEdit   bool   `json:"canEdit"`
	CanDelete bool   `json:"canDelete"`
}

// ListCollaborators GET /api/me/pack-collaborators - the invites the caller
// handed out, and the ones they hold.
func (h *PacksHandler) ListCollaborators(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	given, err := h.state.Store.ListPackCollaboratorsByOwner(userID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to list collaborators", http.StatusInternalServerError)
		return
	}
	held, err := h.state.Store.ListPackCollaborationsForUser(userID)
	if err != nil {
		handlers.SendJSONError(w, "Failed to list collaborations", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "collaborators": given, "collaborations": held})
}

// InviteCollaborator POST /api/me/pack-collaborators - invites a user to the
// caller's packs: all of them (packIds empty) or the ones named, which must be
// the caller's own.
func (h *PacksHandler) InviteCollaborator(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	var req packCollaboratorRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.PackIDs) > 200 {
		handlers.SendJSONError(w, "Too many packs in one invite", http.StatusBadRequest)
		return
	}
	u, err := h.state.Store.GetUserByUsername(strings.TrimSpace(req.Username))
	if err != nil || u == nil {
		handlers.SendJSONError(w, "No user with that name", http.StatusNotFound)
		return
	}
	if u.ID == userID {
		handlers.SendJSONError(w, "You already own your packs", http.StatusBadRequest)
		return
	}
	scopes := []*int{nil}
	if len(req.PackIDs) > 0 {
		scopes = scopes[:0]
		for _, id := range req.PackIDs {
			p, err := h.state.Store.GetPack(id)
			if err != nil || p == nil || p.OwnerID != userID {
				handlers.SendJSONError(w, "You can only share your own packs", http.StatusForbidden)
				return
			}
			pid := id
			scopes = append(scopes, &pid)
		}
	}
	ids := []int{}
	for _, pid := range scopes {
		id, err := h.state.Store.UpsertPackCollaborator(store.PackCollaborator{
			OwnerID: userID, UserID: u.ID, PackID: pid,
			CanCreate: req.CanCreate, CanEdit: req.CanEdit, CanDelete: req.CanDelete,
		}, userID)
		if err != nil {
			handlers.SendJSONError(w, "Failed to save the invite", http.StatusInternalServerError)
			return
		}
		ids = append(ids, id)
		packID := 0
		if pid != nil {
			packID = *pid
		}
		h.audit(r, userID, packID, "collaborator.invited", map[string]any{
			"user": u.Username, "canCreate": req.CanCreate && pid == nil, "canEdit": req.CanEdit, "canDelete": req.CanDelete,
			"allPacks": pid == nil,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "ids": ids})
}

// UpdateCollaborator PATCH /api/me/pack-collaborators/{cid} - changes the
// rights of one of the caller's invites.
func (h *PacksHandler) UpdateCollaborator(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	cid, _ := strconv.Atoi(mux.Vars(r)["cid"])
	var req packCollaboratorRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		handlers.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	c, err := h.state.Store.GetPackCollaborator(userID, cid)
	if err != nil {
		handlers.SendJSONError(w, "Invite not found", http.StatusNotFound)
		return
	}
	if err := h.state.Store.UpdatePackCollaboratorRights(userID, cid, req.CanCreate, req.CanEdit, req.CanDelete); err != nil {
		handlers.SendJSONError(w, "Invite not found", http.StatusNotFound)
		return
	}
	h.audit(r, userID, packIDOf(c), "collaborator.changed", map[string]any{
		"user": c.Username, "canCreate": req.CanCreate && c.PackID == nil, "canEdit": req.CanEdit, "canDelete": req.CanDelete,
	})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// RemoveCollaborator DELETE /api/me/pack-collaborators/{cid} - the owner
// revokes an invite. A collaborator leaves with the same call on an invite
// they hold (owner = the invite's owner).
func (h *PacksHandler) RemoveCollaborator(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	cid, _ := strconv.Atoi(mux.Vars(r)["cid"])
	c, err := h.state.Store.GetPackCollaborator(userID, cid)
	ownerID := userID
	if errors.Is(err, store.ErrPackCollaboratorNotFound) {
		// Not the caller's invite to give: it may be one they hold.
		held, lerr := h.state.Store.ListPackCollaborationsForUser(userID)
		if lerr != nil {
			handlers.SendJSONError(w, "Failed to load the invite", http.StatusInternalServerError)
			return
		}
		for i := range held {
			if held[i].ID == cid {
				c, ownerID, err = &held[i], held[i].OwnerID, nil
			}
		}
	}
	if err != nil || c == nil {
		handlers.SendJSONError(w, "Invite not found", http.StatusNotFound)
		return
	}
	if err := h.state.Store.DeletePackCollaborator(ownerID, cid); err != nil {
		handlers.SendJSONError(w, "Invite not found", http.StatusNotFound)
		return
	}
	h.audit(r, ownerID, packIDOf(c), "collaborator.removed", map[string]any{"user": c.Username, "left": ownerID != userID})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// PackAudit GET /api/packs/{id}/audit and GET /api/me/pack-audit - who
// changed what on the owner's packs. Only the owner (and an admin) reads it.
func (h *PacksHandler) PackAudit(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	ownerID, packID := userID, 0
	if v, ok := mux.Vars(r)["id"]; ok {
		packID, _ = strconv.Atoi(v)
		p, err := h.state.Store.GetPack(packID)
		isAdmin, _ := r.Context().Value("isAdmin").(bool)
		if err != nil || p == nil || (p.OwnerID != userID && !isAdmin) {
			handlers.SendJSONError(w, "Forbidden", http.StatusForbidden)
			return
		}
		ownerID = p.OwnerID
	}
	events, err := h.state.Store.ListPackAudit(ownerID, packID, 200)
	if err != nil {
		handlers.SendJSONError(w, "Failed to load the history", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "events": events})
}

func packIDOf(c *store.PackCollaborator) int {
	if c == nil || c.PackID == nil {
		return 0
	}
	return *c.PackID
}

// audit writes one pack history row; a failure is logged, never fatal.
func (h *PacksHandler) audit(r *http.Request, ownerID string, packID int, action string, detail map[string]any) {
	actor, _ := r.Context().Value("userID").(string)
	name := ""
	if packID > 0 {
		if p, err := h.state.Store.GetPack(packID); err == nil && p != nil {
			name = p.InternalName
		}
	}
	if err := h.state.Store.AddPackAudit(ownerID, packID, name, actor, action, detail); err != nil {
		log.Printf("packs: audit %s on pack %d: %v", action, packID, err)
	}
}

// Audited records a successful write on a pack in its history: who did what,
// owner or collaborator. Wraps the write routes rather than each handler, so a
// new write route is audited by being registered with it. The pack is read
// BEFORE the handler runs, so a delete still knows the owner and the name.
func (h *PacksHandler) Audited(action string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		packID, _ := strconv.Atoi(mux.Vars(r)["id"])
		p, _ := h.state.Store.GetPack(packID)
		rec := &statusRecorder{ResponseWriter: w}
		next(rec, r)
		if p == nil || rec.status >= 300 {
			return
		}
		detail := map[string]any{}
		for _, k := range []string{"buildId", "modversionId", "linkId"} {
			if v := mux.Vars(r)[k]; v != "" {
				detail[k] = v
			}
		}
		actor, _ := r.Context().Value("userID").(string)
		if err := h.state.Store.AddPackAudit(p.OwnerID, p.ID, p.InternalName, actor, action, detail); err != nil {
			log.Printf("packs: audit %s on pack %d: %v", action, p.ID, err)
		}
	}
}

// statusRecorder remembers the status a wrapped handler wrote, so Audited can
// tell a success from a refusal. Same rules as handlers' keyStatusWriter: the
// first status wins and a body without a header is a 200.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
