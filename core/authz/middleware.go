package authz

import (
	"net/http"
	"strconv"

	"github.com/gorilla/mux"
)

// RequireCap wraps a handler so it only runs when the request principal holds
// capID. It is the enforcement half of the chokepoint; identity (AuthMiddleware)
// stays separate from authorization (this). Wiring it into the ~400 routes is
// phase 2; phase 1 only defines and unit-tests it.
//
//   - Unknown capID          -> 500 (developer misconfiguration, fail loud).
//   - No identity            -> 401.
//   - SERVER cap, no server  -> 403 (path lacks a resolvable {id}/{uuid}).
//   - Deny                   -> 403.
//   - Grant                  -> inner handler.
func (r *Resolver) RequireCap(capID string) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			c, ok := Get(capID)
			if !ok {
				denyJSON(w, "unknown capability", http.StatusInternalServerError)
				return
			}
			id := IdentityFromContext(req.Context())
			if !id.IsAdmin && id.UserID == "" {
				denyJSON(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			serverID := 0
			if c.Scope == ScopeServer {
				serverID = r.serverIDFromRequest(req)
				if serverID == 0 {
					denyJSON(w, "Forbidden", http.StatusForbidden)
					return
				}
			}
			res, err := r.Resolve(id, serverID)
			if err != nil || !res.HasCap(capID) {
				denyJSON(w, "Forbidden", http.StatusForbidden)
				return
			}
			// Every server-scoped ACTION passes here with both the capability and
			// the server already resolved, which is why the audit record is taken
			// here rather than in each handler - see write_audit.go for what leaving
			// it to the handlers cost. Reads are not recorded: the trail answers
			// "what was done to this server", and one browse of the file manager
			// would otherwise bury every action in it.
			// GET and HEAD are excluded by METHOD as well as by verb: spark's single
			// "use" capability gates its listing as well as its recording, so the
			// verb alone would file a GET as an action. It also keeps a websocket
			// upgrade - always a GET - away from the wrapper entirely.
			if r.writeAudit != nil && c.Scope == ScopeServer && c.Verb != VerbRead &&
				req.Method != http.MethodGet && req.Method != http.MethodHead {
				aw := &auditedWriter{ResponseWriter: w}
				req = req.WithContext(withAuditSlot(req.Context()))
				next(aw, req)
				// Both conditions are decided HERE so every recorder gets the same
				// contract: called once, for an action that happened, that nobody
				// has already described better.
				if aw.status >= 200 && aw.status < 300 && !WasAudited(req.Context()) {
					r.writeAudit(req, serverID, capID, aw.status)
				}
				return
			}
			next(w, req)
		}
	}
}

// serverIDFromRequest reads the numeric server id from the {id} path var, or
// resolves the {uuid} path var to its numeric id. Returns 0 when neither is
// present or resolvable.
func (r *Resolver) serverIDFromRequest(req *http.Request) int {
	vars := mux.Vars(req)
	if idStr := vars["id"]; idStr != "" {
		if n, err := strconv.Atoi(idStr); err == nil {
			return n
		}
	}
	if uuid := vars["uuid"]; uuid != "" {
		if srv, err := r.store.GetServerByUUID(uuid); err == nil && srv != nil {
			return srv.ID
		}
	}
	return 0
}

func denyJSON(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}
