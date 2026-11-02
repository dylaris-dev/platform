package tabs

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"dylaris-core/handlers/httpx"
)

// Spark profiler integration. Backend surface is intentionally
// small: store + list profile-completion records. Profile start/stop runs
// through the existing /console/command endpoint (panel-driven). URL
// detection happens in the panel too, watching the console SSE stream — when
// it sees a spark.lucko.me URL during an active profile, it POSTs here to
// persist for cross-session history. Access is enforced by the spark.use
// RequireCap chokepoint in routes.go; the old in-handler checkServerAccess
// gate is gone.

type SparkHandler struct {
	d Deps
}

func NewSparkHandler(d Deps) *SparkHandler {
	return &SparkHandler{d: d}
}

// sparkURLPattern matches spark.lucko.me share URLs printed by the spark
// plugin/mod when a profile completes. The trailing chars are alphanumeric
// + dashes — generous to survive any future ID-format tweak.
var sparkURLPattern = regexp.MustCompile(`^https://spark\.lucko\.me/[a-zA-Z0-9_-]+$`)

type sparkProfileRequest struct {
	URL       string `json:"url"`
	StartedAt string `json:"startedAt,omitempty"`
}

// Record POST /api/servers/{id}/spark/profiles - records a finished spark
// profile. The URL must be a spark.lucko.me share link; Core stores the
// reference, never the profile data.
func (h *SparkHandler) Record(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	var req sparkProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if !sparkURLPattern.MatchString(req.URL) {
		httpx.SendJSONError(w, "URL must be a spark.lucko.me share link", http.StatusBadRequest)
		return
	}
	srv, _ := h.d.Store.GetServerByID(serverID)
	userID, _ := r.Context().Value("userID").(string)
	var startedAt sql.NullTime
	if req.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339, req.StartedAt); err == nil {
			startedAt = sql.NullTime{Time: t, Valid: true}
		}
	}
	var requestedBy interface{}
	if userID != "" {
		requestedBy = userID
	}
	_, err := h.d.Redis.Pipeline().Exec(r.Context())
	_ = err // unrelated; quiet linter

	// Direct DB write — postgres store doesn't have a typed helper for
	// spark_profiles yet (one-table feature), inline here to avoid a
	// store-interface ripple for V1.
	db := h.dbFromState()
	if db == nil {
		httpx.SendJSONError(w, "DB unavailable", http.StatusInternalServerError)
		return
	}
	_, err = db.Exec(`INSERT INTO spark_profiles
		(server_id, sub_server_name, url, started_at, requested_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (server_id, url) DO NOTHING`,
		serverID, srv.ActiveSubServer, req.URL, startedAt, requestedBy,
	)
	if err != nil {
		httpx.SendJSONError(w, "Failed to record profile", http.StatusInternalServerError)
		return
	}

	h.d.Events.Publish(r.Context(), "spark_profiles.changed", map[string]interface{}{
		"serverId": serverID,
	})

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// List GET /api/servers/{id}/spark/profiles - the 50 most recent spark
// profiles for one server, newest first.
func (h *SparkHandler) List(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	db := h.dbFromState()
	if db == nil {
		httpx.SendJSONError(w, "DB unavailable", http.StatusInternalServerError)
		return
	}
	rows, err := db.Query(`SELECT id, server_id, sub_server_name, url, started_at, completed_at, requested_by
		FROM spark_profiles WHERE server_id=$1 ORDER BY completed_at DESC LIMIT 50`, serverID)
	if err != nil {
		httpx.SendJSONError(w, "Query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	// requested_by is a UUID column (users.id) - it was scanned into a
	// sql.NullInt64 here, which fails for every row Record writes, and the
	// error was swallowed by a `continue`. That silently emptied the list for
	// every profile an authenticated user recorded, which is all of them.
	type profile struct {
		ID            int        `json:"id"`
		ServerID      int        `json:"serverId"`
		SubServerName string     `json:"subServerName"`
		URL           string     `json:"url"`
		StartedAt     *time.Time `json:"startedAt,omitempty"`
		CompletedAt   time.Time  `json:"completedAt"`
		RequestedBy   *string    `json:"requestedBy,omitempty"`
	}
	out := []profile{}
	for rows.Next() {
		var p profile
		var startedAt sql.NullTime
		var requestedBy sql.NullString
		if err := rows.Scan(&p.ID, &p.ServerID, &p.SubServerName, &p.URL, &startedAt, &p.CompletedAt, &requestedBy); err != nil {
			httpx.SendJSONError(w, "Query failed", http.StatusInternalServerError)
			return
		}
		if startedAt.Valid {
			v := startedAt.Time
			p.StartedAt = &v
		}
		if requestedBy.Valid {
			v := requestedBy.String
			p.RequestedBy = &v
		}
		out = append(out, p)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"profiles": out,
	})
}

// Delete DELETE /api/servers/{id}/spark/profiles/{profileId} - removes one
// profile reference. The delete is scoped to the server in the path, so a
// profile id from another server is 404.
func (h *SparkHandler) Delete(w http.ResponseWriter, r *http.Request) {
	serverID, _ := strconv.Atoi(mux.Vars(r)["id"])
	profileID, _ := strconv.Atoi(mux.Vars(r)["profileId"])
	db := h.dbFromState()
	if db == nil {
		httpx.SendJSONError(w, "DB unavailable", http.StatusInternalServerError)
		return
	}
	res, err := db.Exec(`DELETE FROM spark_profiles WHERE id=$1 AND server_id=$2`, profileID, serverID)
	if err != nil {
		httpx.SendJSONError(w, "Delete failed", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.SendJSONError(w, "Not found", http.StatusNotFound)
		return
	}
	h.d.Events.Publish(r.Context(), "spark_profiles.changed", map[string]interface{}{
		"serverId": serverID,
	})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// dbFromState fishes the *sql.DB out of the postgres store. We avoid adding
// a Store-interface method for a single one-shot feature; if Spark grows
// more queries this becomes worth wrapping.
type sparkDBProvider interface {
	RawDB() *sql.DB
}

func (h *SparkHandler) dbFromState() *sql.DB {
	if h.d.Store == nil {
		return nil
	}
	provider, ok := h.d.Store.(sparkDBProvider)
	if !ok {
		return nil
	}
	return provider.RawDB()
}
