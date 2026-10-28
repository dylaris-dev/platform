package packs

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"dylaris-core/handlers"

	"github.com/gorilla/mux"
)

// The pack history (audit) and the wrapper that records every pack write.
// Panel collaborators were removed 2026-10-28: a bound pack's access comes
// from its Modrinth team (modrinth_access.go).

// PackAudit GET /api/packs/{id}/audit and GET /api/me/pack-audit - who
// changed what on the caller's packs.
func (h *PacksHandler) PackAudit(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	ownerID, packID := userID, 0
	if v, ok := mux.Vars(r)["id"]; ok {
		packID, _ = strconv.Atoi(v)
		// Whoever may manage the pack as a whole (its Modrinth delete right,
		// or the creator of an unbound pack), and an admin, read its history.
		p, manages := h.packAccess(r, packID, packDeletePack)
		isAdmin, _ := r.Context().Value("isAdmin").(bool)
		if p == nil || (!manages && !isAdmin) {
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
