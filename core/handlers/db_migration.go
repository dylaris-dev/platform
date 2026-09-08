package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"dylaris-core/services"
)

// DBMigrationHandler exposes the in-panel cross-database migration: connect a
// new target database, copy everything into it under maintenance mode, and
// verify. All endpoints are admin-only. The job is shared (Redis-backed) so
// every admin sees the same live status. The live Core keeps using the OLD
// database; the operator restarts with the new DB_* env vars after success.
type DBMigrationHandler struct {
	state *AppState
}

func NewDBMigrationHandler(state *AppState) *DBMigrationHandler {
	return &DBMigrationHandler{state: state}
}

// dbTargetRequest is the connection form submitted by the panel. dbType is the
// multiple-choice target backend (postgresql | timescaledb).
type dbTargetRequest struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	User     string `json:"user"`
	Password string `json:"password"`
	DBName   string `json:"dbName"`
	SSLMode  string `json:"sslMode"`
	DBType   string `json:"dbType"`
}

func (req dbTargetRequest) toParams() services.DBConnParams {
	return services.DBConnParams{
		Host:     strings.TrimSpace(req.Host),
		Port:     strings.TrimSpace(req.Port),
		User:     strings.TrimSpace(req.User),
		Password: req.Password,
		DBName:   strings.TrimSpace(req.DBName),
		SSLMode:  strings.TrimSpace(req.SSLMode),
		DBType:   strings.TrimSpace(req.DBType),
	}
}

// GetMigration GET /api/admin/db/migration - shared job status. PANEL
// settings.read (RequireCap at the route).
func (h *DBMigrationHandler) GetMigration(w http.ResponseWriter, r *http.Request) {
	if h.state.DBMigration == nil {
		sendJSONError(w, "DB migration not available", http.StatusServiceUnavailable)
		return
	}
	job, ok, err := h.state.DBMigration.GetJob(r.Context())
	if err != nil {
		sendJSONError(w, "Failed to read job: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]interface{}{"success": true, "hasJob": ok}
	if ok {
		resp["job"] = job
	}
	json.NewEncoder(w).Encode(resp)
}

// StartMigration POST /api/admin/db/migration - start a migration to the
// target. PANEL settings.write (RequireCap at the route).
func (h *DBMigrationHandler) StartMigration(w http.ResponseWriter, r *http.Request) {
	if h.state.DBMigration == nil {
		sendJSONError(w, "DB migration not available", http.StatusServiceUnavailable)
		return
	}
	var req dbTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	actorName := h.resolveUsername(actorID)

	job, err := h.state.DBMigration.Start(r.Context(), req.toParams(), actorID, actorName)
	if err != nil {
		switch {
		case err == services.ErrMigrationRunning:
			sendJSONError(w, err.Error(), http.StatusConflict)
		default:
			sendJSONError(w, err.Error(), http.StatusBadRequest)
		}
		return
	}

	LogIdentityAudit(h.state, r, AuditEventMaintenanceToggled, actorID, "", map[string]interface{}{
		"action": "db_migration_start",
		"target": job.Target,
		"jobId":  job.ID,
	})
	if h.state.Events != nil {
		h.state.Events.Publish(r.Context(), "dbmigration.changed", nil)
	}

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "job": job})
}

// TestConnection POST /api/admin/db/migration/test-connection — open the target
// and return its server version, so admins confirm they hit the right database.
// PANEL settings.write (RequireCap at the route).
func (h *DBMigrationHandler) TestConnection(w http.ResponseWriter, r *http.Request) {
	var req dbTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	params := req.toParams()

	// Reachability first, and separately. "Failed to connect" used to cover a
	// hostname that does not resolve and a password that was refused, which are
	// fixed in different places by different people - see services/conncheck.go.
	if reach := services.Reachable(r.Context(), params.Host, params.Port); !reach.OK {
		sendConnTestFailure(w, reach)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	db, err := params.Open(ctx, 6*time.Second)
	if err != nil {
		sendConnTestFailure(w, services.Rejected(services.DescribePostgresError(err)))
		return
	}
	defer db.Close()

	var version string
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&version); err != nil {
		sendConnTestFailure(w, services.Rejected("Connected and authenticated, but the first query failed: "+
			services.DescribePostgresError(err)))
		return
	}
	// Report whether the target already has the TimescaleDB extension, so the UI
	// can warn if the chosen target DB_TYPE does not match the server's reality.
	var tsInstalled bool
	_ = db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')").Scan(&tsInstalled)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":            true,
		"version":            version,
		"timescaleInstalled": tsInstalled,
	})
}

// VerifyMigration POST /api/admin/db/migration/verify — run the source-vs-target
// comparison on demand (the manual "Verify / Test" button). Read-only on both
// sides: checks every table exists on both and that row counts match, returning a
// per-table report plus a human-readable log.
func (h *DBMigrationHandler) VerifyMigration(w http.ResponseWriter, r *http.Request) {
	if h.state.DBMigration == nil {
		sendJSONError(w, "DB migration not available", http.StatusServiceUnavailable)
		return
	}
	var req dbTargetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// COUNT(*) over large tables can take a while; allow a generous budget.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	report, err := h.state.DBMigration.VerifyTarget(ctx, req.toParams())
	if err != nil {
		sendJSONError(w, "Verify failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "report": report})
}

// timescaleRecommendRows is the server_stats row estimate above which the panel
// recommends switching a plain-PostgreSQL deployment to TimescaleDB. Conservative
// on purpose: this is advisory, never blocking.
const timescaleRecommendRows = 5_000_000

// HypertableStatus GET /api/admin/db/hypertable — reports the current backend
// state: whether the timescaledb extension is present, whether server_stats is
// already a hypertable, whether an in-place conversion is possible, and whether
// we recommend switching to TimescaleDB for performance.
func (h *DBMigrationHandler) HypertableStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	tsInstalled, _ := h.state.Store.TimescaleEnabled(ctx)
	isHyper := false
	if tsInstalled {
		isHyper, _ = h.state.Store.IsServerStatsHypertable(ctx)
	}
	rows, _ := h.state.Store.EstimateServerStatsRows(ctx)

	// Running plain when DB_TYPE is postgres, or when timescale was requested but
	// the extension is missing (server_stats is a plain table either way).
	plain := h.state.DBType == "postgres" || !tsInstalled
	recommend := plain && rows >= timescaleRecommendRows

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":                 true,
		"dbType":                  h.state.DBType,
		"timescaleInstalled":      tsInstalled,
		"isHypertable":            isHyper,
		"eligibleForConversion":   tsInstalled && !isHyper,
		"serverStatsRowsEstimate": rows,
		"recommendTimescale":      recommend,
		"recommendThreshold":      timescaleRecommendRows,
	})
}

// ConvertHypertable POST /api/admin/db/hypertable/convert — promote the existing
// plain server_stats table to a TimescaleDB hypertable IN PLACE (same database).
// For the case where the operator swapped a plain-Postgres image for a TimescaleDB
// one on the same volume.
func (h *DBMigrationHandler) ConvertHypertable(w http.ResponseWriter, r *http.Request) {
	// migrate_data rewrites the table; on a large server_stats this can take a while.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	tsInstalled, err := h.state.Store.TimescaleEnabled(ctx)
	if err != nil {
		sendJSONError(w, "Failed to check TimescaleDB: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !tsInstalled {
		sendJSONError(w, "The TimescaleDB extension is not installed in this database. Switch to a TimescaleDB image first.", http.StatusBadRequest)
		return
	}
	if isHyper, _ := h.state.Store.IsServerStatsHypertable(ctx); isHyper {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "alreadyHypertable": true})
		return
	}

	if err := h.state.Store.ConvertServerStatsToHypertable(ctx); err != nil {
		sendJSONError(w, "Conversion failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogIdentityAudit(h.state, r, AuditEventMaintenanceToggled, actorID, "", map[string]interface{}{
		"action": "server_stats_hypertable_convert",
	})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "converted": true})
}

// resolveUsername best-effort maps a user UUID to a display name.
func (h *DBMigrationHandler) resolveUsername(id string) string {
	if id == "" {
		return ""
	}
	if u, err := h.state.Store.GetUserByID(id); err == nil && u != nil {
		return u.Username
	}
	return ""
}
