package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/authz"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/storage"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"

	"dylaris-pkg/validate"
	pbNode "dylaris-proto/node"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// validSubServer rejects a backup job's sub-server name that is not a plain
// directory name.
//
// The value reaches the Node as-is and is filepath.Join'ed onto the server's
// data directory there, and Join CLEANS rather than confines: a "../.." walks
// out of the server root, so an unchecked name turns a backup into an archive
// of whatever the node process can read, and a restore into a write outside the
// server. The name is decoded straight from the request body into models.BackupJob
// (only ID/ServerID/schedule are forced), so it is caller-controlled.
// validate.IsSubServerName is the same rule the console handlers already apply
// to this exact parameter.
func validSubServer(w http.ResponseWriter, sub *string) bool {
	if sub == nil || *sub == "" {
		return true // NULL / empty means the whole container, which is a valid job
	}
	if !validate.IsSubServerName(*sub) {
		sendJSONError(w, "Invalid sub-server name", http.StatusBadRequest)
		return false
	}
	return true
}

// errBackupQuotaReached marks the two quota refusals in startBackupRun so the
// HTTP layer can answer 409 instead of 500. Sentinel rather than a string
// match on the message, since both messages embed live figures.
var errBackupQuotaReached = errors.New("backup quota reached")

// backupQuotaRefusal renders one of the two quota refusals.
//
// A cap of ZERO is a different situation from a quota that filled up, and the
// generic wording is unfollowable there: deleting old backups frees nothing
// when the allowance is none. It is also a state an install reaches without
// meaning to - the limit convention made 0 a real cap where it used to mean
// unlimited - and the old message rendered it as "(0 / 0 GB used) - delete old
// backups", which reads as a fault rather than as a setting somebody can
// change. MEASURED in production: a stored platform quota of 0 refused every
// backup for every tenant holding no entitlement, with that message.
//
// scope names whose budget it is (the R2 one is the tenant's, the node-local
// one is this server's); raise says where the number lives.
func backupQuotaRefusal(usedBytes, quotaBytes int64, scope, raise string) error {
	const gb = 1024 * 1024 * 1024
	if quotaBytes == 0 {
		return fmt.Errorf("%w - the backup storage allowance%s is 0 GB, so no backup can be stored: %s",
			errBackupQuotaReached, scope, raise)
	}
	return fmt.Errorf("%w (%.1f / %.1f GB used%s) - delete old backups or %s",
		errBackupQuotaReached, float64(usedBytes)/gb, float64(quotaBytes)/gb, scope, raise)
}

type BackupHandler struct {
	state *AppState
}

func NewBackupHandler(state *AppState) *BackupHandler {
	return &BackupHandler{state: state}
}

// ───────────── Storages (PANEL settings.read/write) ─────────────
// Platform-shared storage-provider configs; gated at the route via RequireCap
// (routes.go), not in-handler. Admin still passes via the resolver's admin
// short-circuit; a panel-role holder of settings.* also passes.

// ListStorages GET /api/backup-storages - every configured backup target, with
// the S3 secret stripped from each row so a settings.read holder cannot
// harvest backup credentials out of a list.
func (h *BackupHandler) ListStorages(w http.ResponseWriter, r *http.Request) {
	storages, err := h.state.Store.ListBackupStorages()
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	if storages == nil {
		storages = []models.BackupStorage{}
	}
	// Never return the s3 secret. A settings.read holder could otherwise
	// harvest every backup credential straight from this list.
	for i := range storages {
		storages[i] = redactBackupStorageSecret(storages[i])
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "storages": storages})
}

// validBackupProvider mirrors the switch in storage/backup/factory.go:Open.
// Adding a provider requires a conscious change in BOTH places. Without this
// allowlist a typo persists to backup_storages and only fails much later at
// backup.Open, which is exactly what would make "core-storage" and
// "corestorage" indistinguishable to an operator.
//
// NOTE: this is an input allowlist, not an authorization boundary.
// BackupConfig.Mode governs what the PANEL offers; the API accepts any
// allowlisted provider regardless of Mode, exactly as it does today for the
// existing three.
func validBackupProvider(p string) bool {
	switch p {
	case "local", "shared", "s3", "node-local", "core-storage", "connection":
		return true
	}
	return false
}

// CreateStorage POST /api/backup-storages - adds a backup target. The provider
// must be one of shared, s3, node-local, core-storage or connection, and a
// duplicate name is 409.
func (h *BackupHandler) CreateStorage(w http.ResponseWriter, r *http.Request) {
	var req models.BackupStorage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if req.Name == "" || req.Provider == "" {
		sendJSONError(w, "name and provider are required", 400)
		return
	}
	if !validBackupProvider(req.Provider) {
		sendJSONError(w, "invalid provider (expected shared, s3, node-local, core-storage or connection)", 400)
		return
	}
	if err := validateBackupStorageEndpoint(req); err != nil {
		sendJSONError(w, err.Error(), 400)
		return
	}
	id, err := h.state.Store.CreateBackupStorage(&req)
	if err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			sendJSONError(w, "A backup storage with that name already exists", 409)
			return
		}
		sendJSONError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": id})
}

// UpdateStorage PATCH /api/backup-storages/{id} - edits a backup target; an
// unknown id is 404.
func (h *BackupHandler) UpdateStorage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", 400)
		return
	}
	var req models.BackupStorage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	req.ID = id
	if !validBackupProvider(req.Provider) {
		sendJSONError(w, "invalid provider (expected shared, s3, node-local, core-storage or connection)", 400)
		return
	}
	if err := validateBackupStorageEndpoint(req); err != nil {
		sendJSONError(w, err.Error(), 400)
		return
	}
	// The panel edits with the secret redacted, so an update normally carries no
	// secret. Backfill the stored one unless a new value was submitted, or the
	// edit would silently wipe the credential - but only while the edit leaves
	// the secret pointed at the same endpoint, bucket and access key. Changing
	// any of those without supplying a secret is refused rather than merged; see
	// mergeBackupStorageSecret.
	if existing, err := h.state.Store.GetBackupStorage(id); err == nil {
		merged, merr := mergeBackupStorageSecret(req, existing)
		if merr != nil {
			sendJSONError(w, merr.Error(), 400)
			return
		}
		req = merged
	}
	if err := h.state.Store.UpdateBackupStorage(&req); err != nil {
		switch {
		case errors.Is(err, store.ErrNameTaken):
			sendJSONError(w, "A backup storage with that name already exists", 409)
		case errors.Is(err, sql.ErrNoRows):
			sendJSONError(w, "Backup storage not found", 404)
		default:
			sendJSONError(w, err.Error(), 500)
		}
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// DeleteStorage DELETE /api/backup-storages/{id} - removes a backup target.
func (h *BackupHandler) DeleteStorage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", 400)
		return
	}
	if err := h.state.Store.DeleteBackupStorage(id); err != nil {
		sendJSONError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// TestStorage POST /api/backup-storages/{id}/test — round-trip put/get/delete
// a tiny object to confirm credentials and bucket access.
func (h *BackupHandler) TestStorage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", 400)
		return
	}
	storage, err := h.state.Store.GetBackupStorage(id)
	if err != nil {
		sendJSONError(w, "Storage not found", 404)
		return
	}
	provider, err := backupstorage.Open(r.Context(), storage, h.backupDeps())
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	// Round-trip put/read-back/delete: a backend that accepts a write but hands
	// back different or no bytes is broken, and put-then-delete alone reported it
	// as green.
	if ok, msg := probeBackupStorage(r.Context(), provider); !ok {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": msg})
		return
	}
	// A local/shared path on the container's own filesystem passes every probe
	// yet loses every archive on the next container recreation. Report that as a
	// warning riding along with success, never as a plain green result.
	if warning := backupStorageEphemeralWarning(storage); warning != "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "warning": warning})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ───────────── Jobs ─────────────

// ListJobs GET /api/servers/{id}/backup-jobs - the backup schedules configured
// for one server.
func (h *BackupHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	serverID, srv, ok := h.resolveServer(w, r)
	if !ok {
		return
	}
	_ = srv
	jobs, err := h.state.Store.ListBackupJobs(serverID)
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	if jobs == nil {
		jobs = []models.BackupJob{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "jobs": jobs})
}

// CreateJob POST /api/servers/{id}/backup-jobs - adds a backup schedule and
// computes its first run from the cron expression.
func (h *BackupHandler) CreateJob(w http.ResponseWriter, r *http.Request) {
	serverID, _, ok := h.resolveServer(w, r)
	if !ok {
		return
	}
	var req models.BackupJob
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}
	if !validSubServer(w, req.SubServer) {
		return
	}
	req.ServerID = serverID
	if req.RetentionCount <= 0 {
		req.RetentionCount = 3
	}
	req.Schedule = strings.TrimSpace(req.Schedule)
	if req.Schedule == "" {
		req.Schedule = "manual"
	}
	// The parser was never wrong about "banana": it returned nil, meaning "I
	// cannot schedule this", and this handler stored the job anyway. The result
	// is a job that is listed, enabled, and never runs - which you find out
	// when you need the backup. Measured on a live stack: banana, every 0h,
	// every -3h, every 6m, "* * * * *", every 6H, Every 6h, every6h and daily
	// were all accepted with 200 and left with no next run. Scheduled TASKS
	// have refused an unparseable cron with a 400 all along; this is the
	// sibling that did not.
	if !services.ValidBackupSchedule(req.Schedule) {
		sendJSONError(w, `Schedule must be "manual" or "every <n>h" / "every <n>d" - for example "every 6h"`, 400)
		return
	}
	req.NextRunAt = services.ComputeBackupNextRun(req.Schedule, time.Now())
	id, err := h.state.Store.CreateBackupJob(&req)
	if err != nil {
		sendJSONError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": id})
}

// updateBackupJobRequest is a PATCH body: every field is a pointer, and nil
// means "the caller did not mention this, leave it alone".
//
// It used to decode straight into models.BackupJob and write the whole row, so
// a PATCH that sent only a schedule reset everything it omitted to the zero
// value. Measured on a live stack, `{"schedule":"every 6h"}` against a job
// named nightly-world on sub-server "creative" with patterns, storage 1 and
// enabled=true left: no name, no sub-server (so it backed up the whole
// container instead), no storage (so the archive went somewhere else), no
// patterns - and enabled FALSE. Changing how often a backup runs turned the
// backup off. Only retentionCount, schedule and serverID survived, because
// those three had explicit fallbacks; this gives the rest the same treatment.
//
// SubServer and StorageID are nullable in the model, and JSON null decodes to
// the same nil an absent field does, so an explicit "" / 0 is how a caller
// says "clear it" - the same convention the custom-tab share expiry uses.
type updateBackupJobRequest struct {
	Name            *string   `json:"name"`
	SubServer       *string   `json:"subServer"`
	Schedule        *string   `json:"schedule"`
	IncludePatterns *[]string `json:"includePatterns"`
	ExcludePatterns *[]string `json:"excludePatterns"`
	RetentionCount  *int      `json:"retentionCount"`
	StorageID       *int      `json:"storageId"`
	Enabled         *bool     `json:"enabled"`
}

// UpdateJob PATCH /api/backup-jobs/{jobId} - edits a job and recomputes its
// next run. Gated on backups.create for the job's own server, not on the
// caller's access to some other one.
func (h *BackupHandler) UpdateJob(w http.ResponseWriter, r *http.Request) {
	jobID, job, ok := h.resolveJobWithAccess(w, r, "backups.create")
	if !ok {
		return
	}
	var req updateBackupJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	// Start from what is stored and apply only what arrived.
	next := *job
	next.ID = jobID
	next.ServerID = job.ServerID

	if req.Name != nil {
		next.Name = strings.TrimSpace(*req.Name)
	}
	if req.SubServer != nil {
		if !validSubServer(w, req.SubServer) {
			return
		}
		if s := strings.TrimSpace(*req.SubServer); s == "" {
			next.SubServer = nil // back up the whole container
		} else {
			next.SubServer = &s
		}
	}
	if req.IncludePatterns != nil {
		next.IncludePatterns = *req.IncludePatterns
	}
	if req.ExcludePatterns != nil {
		next.ExcludePatterns = *req.ExcludePatterns
	}
	if req.RetentionCount != nil && *req.RetentionCount > 0 {
		next.RetentionCount = *req.RetentionCount
	}
	if req.StorageID != nil {
		if *req.StorageID <= 0 {
			next.StorageID = nil // fall back to the default storage
		} else {
			v := *req.StorageID
			next.StorageID = &v
		}
	}
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	// Only what the CALLER sent is validated. An omitted schedule keeps the
	// stored one untouched - including one of the broken values saved before
	// that check existed, which would otherwise make such a job impossible to
	// edit at all, even to rename it.
	if req.Schedule != nil {
		s := strings.TrimSpace(*req.Schedule)
		if s != "" {
			if !services.ValidBackupSchedule(s) {
				sendJSONError(w, `Schedule must be "manual" or "every <n>h" / "every <n>d" - for example "every 6h"`, 400)
				return
			}
			next.Schedule = s
		}
	}
	next.NextRunAt = services.ComputeBackupNextRun(next.Schedule, time.Now())
	if err := h.state.Store.UpdateBackupJob(&next); err != nil {
		sendJSONError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// DeleteJob DELETE /api/backup-jobs/{jobId} - removes a schedule, gated on
// backups.delete for the job's server.
func (h *BackupHandler) DeleteJob(w http.ResponseWriter, r *http.Request) {
	jobID, _, ok := h.resolveJobWithAccess(w, r, "backups.delete")
	if !ok {
		return
	}
	if err := h.state.Store.DeleteBackupJob(jobID); err != nil {
		sendJSONError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// TriggerJob POST /api/backup-jobs/{jobId}/trigger - starts a run immediately.
// Hitting the backup quota answers 409, not 500: that is the system working
// rather than failing, and both operator alerting and API clients key off the
// difference.
func (h *BackupHandler) TriggerJob(w http.ResponseWriter, r *http.Request) {
	_, job, ok := h.resolveJobWithAccess(w, r, "backups.create")
	if !ok {
		return
	}
	runID, err := h.startBackupRun(r.Context(), job)
	if err != nil {
		// A quota refusal is the system working, not failing. Answering 500 put
		// a policy decision in the same bucket as a broken queue, which is what
		// an operator's alerting and any API client both key off.
		if errors.Is(err, errBackupQuotaReached) {
			sendJSONError(w, err.Error(), http.StatusConflict)
			return
		}
		sendJSONError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "runId": runID})
}

// ───────────── Runs ─────────────

// ListRuns GET /api/backup-jobs/{jobId}/runs - the 50 most recent runs of one
// schedule.
func (h *BackupHandler) ListRuns(w http.ResponseWriter, r *http.Request) {
	jobID, _, ok := h.resolveJobWithAccess(w, r, "backups.read")
	if !ok {
		return
	}
	runs, err := h.state.Store.ListBackupRuns(jobID, 50)
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	if runs == nil {
		runs = []models.BackupRun{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "runs": runs})
}

// DownloadRun GET /api/backup-runs/{runId}/download
// For S3-backed storage returns 302 to a pre-signed URL. For local storage
// streams the file directly through Core.
func (h *BackupHandler) DownloadRun(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.Atoi(mux.Vars(r)["runId"])
	if err != nil {
		sendJSONError(w, "Invalid run ID", 400)
		return
	}
	run, err := h.state.Store.GetBackupRun(runID)
	if err != nil {
		sendJSONError(w, "Run not found", 404)
		return
	}
	job, err := h.state.Store.GetBackupJob(run.JobID)
	if err != nil {
		sendJSONError(w, "Job not found", 404)
		return
	}
	if !h.hasServerAccess(r, job.ServerID, "backups.read") {
		sendJSONError(w, "Forbidden", 403)
		return
	}
	// Same resolution the run path uses: the archive's own storage, else the
	// job's, else the default. Refusing on a nil StorageID made every job
	// created with the panel's "Default storage" option undownloadable even
	// though its backup succeeded.
	bs, err := services.ResolveRunStorage(h.state.Store, run, job.StorageID,
		services.BackupJobOwner(h.state.Store, job.ServerID))
	if err != nil {
		if errors.Is(err, services.ErrNoBackupStorage) {
			sendJSONError(w, "No backup storage is configured", 400)
		} else {
			sendJSONError(w, "Storage not found", 404)
		}
		return
	}
	provider, err := backupstorage.Open(r.Context(), bs, h.backupDeps())
	if err != nil {
		sendJSONError(w, err.Error(), 500)
		return
	}

	// Prefer pre-signed URL when supported (S3) — keeps Core out of the data path.
	if url, _ := provider.DownloadURL(r.Context(), run.StorageKey, time.Hour); url != "" {
		http.Redirect(w, r, url, http.StatusFound)
		return
	}

	// Local fallback: stream the file through Core with the right headers.
	reader, err := provider.Get(r.Context(), run.StorageKey)
	if err != nil {
		sendJSONError(w, "Object not found", 404)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="backup-%d.tar.gz"`, run.ID))
	io.Copy(w, reader)
}

// RestoreRun POST /api/backup-runs/{runId}/restore
// Dispatches a restore command to the node. The node stops the affected
// sub-server, streams the archive from storage, extracts in place and
// restarts the container. We don't block the HTTP request on the actual
// restore — it can take minutes for large worlds — the panel polls run
// status instead.
func (h *BackupHandler) RestoreRun(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.Atoi(mux.Vars(r)["runId"])
	if err != nil {
		sendJSONError(w, "Invalid run ID", 400)
		return
	}
	run, err := h.state.Store.GetBackupRun(runID)
	if err != nil {
		sendJSONError(w, "Run not found", 404)
		return
	}
	if run.Status != "success" {
		sendJSONError(w, "Cannot restore a run that did not complete successfully", 400)
		return
	}
	job, err := h.state.Store.GetBackupJob(run.JobID)
	if err != nil {
		sendJSONError(w, "Job not found", 404)
		return
	}
	if !h.hasServerAccess(r, job.ServerID, "backups.restore") {
		sendJSONError(w, "Forbidden", 403)
		return
	}
	srv, err := h.state.Store.GetServerByID(job.ServerID)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return
	}
	node, err := h.state.Store.GetNodeByID(srv.NodeID)
	if err != nil {
		sendJSONError(w, "Node not found", 404)
		return
	}
	// Same resolution the run path uses. This is the one that matters most: a
	// backup that cannot be restored is worse than no backup, because the
	// failure only shows up when someone is already recovering.
	storage, err := services.ResolveRunStorage(h.state.Store, run, job.StorageID, srv.OwnerID)
	if err != nil {
		if errors.Is(err, services.ErrNoBackupStorage) {
			sendJSONError(w, "No backup storage is configured", 400)
		} else {
			sendJSONError(w, "Storage not found", 404)
		}
		return
	}

	// Track the restore attempt in the DB so the panel can show history
	// even after the node finishes (or fails).
	username := r.Context().Value("username").(string)
	var requestedBy *string
	if user, _ := h.state.Store.GetUserByUsername(username); user != nil {
		v := user.ID
		requestedBy = &v
	}
	restoreID, err := h.state.Store.CreateBackupRestore(&models.BackupRestore{
		RunID:       run.ID,
		ServerID:    job.ServerID,
		RequestedBy: requestedBy,
		Status:      "queued",
	})
	if err != nil {
		sendJSONError(w, "Failed to record restore: "+err.Error(), 500)
		return
	}

	if h.state.Queue == nil {
		h.state.Store.UpdateBackupRestoreStatus(restoreID, "failed", "queue unavailable", time.Now())
		sendJSONError(w, "Queue unavailable", 500)
		return
	}

	storageCfgJSON, presignedGet, err := services.PrepareNodeStorage(r.Context(), h.state.Store, storage, node, run.StorageKey, "get", h.backupDeps())
	if err != nil {
		h.state.Store.UpdateBackupRestoreStatus(restoreID, "failed", err.Error(), time.Now())
		sendJSONError(w, err.Error(), 500)
		return
	}
	subServer := ""
	if job.SubServer != nil {
		subServer = *job.SubServer
	}
	payload := map[string]interface{}{
		"action":          "backup_restore",
		"runId":           run.ID,
		"restoreId":       restoreID,
		"jobId":           job.ID,
		"serverUuid":      srv.UUID,
		"subServer":       subServer,
		"storageKey":      run.StorageKey,
		"storage":         json.RawMessage(storageCfgJSON),
		"presignedGetUrl": presignedGet,
	}
	// Publish to the node's durable :cmds stream (BC1) instead of RPush to the
	// retired dylaris:node:<token>:queue list, which nothing reads anymore.
	if err := h.state.Queue.SendRawCommand(r.Context(), node.Token, payload); err != nil {
		h.state.Store.UpdateBackupRestoreStatus(restoreID, "failed", "queue push failed: "+err.Error(), time.Now())
		sendJSONError(w, "Failed to queue restore: "+err.Error(), 500)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "restoreId": restoreID})
}

// ListRestores GET /api/servers/{id}/backup-restores
// Recent restore history for a server, newest first.
func (h *BackupHandler) ListRestores(w http.ResponseWriter, r *http.Request) {
	serverID, srv, ok := h.resolveServer(w, r)
	if !ok {
		return
	}
	restores, err := h.state.Store.ListBackupRestores(serverID, 25)
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	if restores == nil {
		restores = []models.BackupRestore{}
	}
	// A restore left "queued" by a node that went away looks exactly like one
	// that is about to start. See restore_stall.go.
	if srv != nil && h.state.GRPCRegistry != nil {
		annotateStalledRestores(r.Context(), h.state.Redis, h.state.GRPCRegistry.IsConnected, srv.NodeID, srv.UUID, restores)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "restores": restores})
}

// DeleteRun DELETE /api/backup-runs/{runId} - deletes the stored archive and
// then the run row. The object delete is best effort: if the storage is
// unreachable the row goes anyway, so the UI keeps no phantom entry, and the
// orphaned archive is logged.
func (h *BackupHandler) DeleteRun(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.Atoi(mux.Vars(r)["runId"])
	if err != nil {
		sendJSONError(w, "Invalid run ID", 400)
		return
	}
	run, err := h.state.Store.GetBackupRun(runID)
	if err != nil {
		sendJSONError(w, "Run not found", 404)
		return
	}
	job, err := h.state.Store.GetBackupJob(run.JobID)
	if err != nil {
		sendJSONError(w, "Job not found", 404)
		return
	}
	if !h.hasServerAccess(r, job.ServerID, "backups.delete") {
		sendJSONError(w, "Forbidden", 403)
		return
	}
	// Best-effort object delete first; even if the storage is unreachable we
	// still remove the DB row so the UI doesn't keep a phantom entry.
	//
	// That trade is about an UNREACHABLE storage. It used to cover a second case
	// it was never meant to: `job.StorageID != nil` skipped the delete outright
	// for a job on the platform default, where storage_id is NULL and the storage
	// is perfectly reachable. Since the panel's storage dropdown offers "Default
	// storage" first, deleting a backup from the UI usually removed the row and
	// left the archive behind for good.
	if bs, sErr := services.ResolveRunStorage(h.state.Store, run, job.StorageID,
		services.BackupJobOwner(h.state.Store, job.ServerID)); sErr == nil {
		if provider, pErr := backupstorage.Open(r.Context(), bs, h.backupDeps()); pErr == nil {
			if dErr := provider.Delete(r.Context(), run.StorageKey); dErr != nil {
				log.Printf("DeleteBackupRun: run %d object %s not deleted: %v — the row is removed anyway, so this archive is now untracked",
					runID, run.StorageKey, dErr)
			}
		}
	}
	if err := h.state.Store.DeleteBackupRun(runID); err != nil {
		sendJSONError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// BackupUsage GET /api/servers/{id}/backup-usage
//
// Returns the on-disk bytes used by node-local backups for the given
// server, plus archive count. Available regardless of the active backup
// mode — for s3/shared the numbers come back zero, which the Overview
// tab uses to decide whether to render the split storage display.
func (h *BackupHandler) BackupUsage(w http.ResponseWriter, r *http.Request) {
	serverID, srv, ok := h.resolveServer(w, r)
	if !ok {
		return
	}
	_ = serverID

	if h.state.GRPCRegistry == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"usedBytes": 0,
			"count":     0,
		})
		return
	}

	reqID := uuid.NewString()
	msg := &pbNode.NodeMessage{
		RequestId:  reqID,
		ServerUuid: srv.UUID,
		Payload: &pbNode.NodeMessage_BackupUsageReq{
			BackupUsageReq: &pbNode.BackupUsageReq{},
		},
	}
	resp, err := h.state.GRPCRegistry.SendRequest(srv.NodeID, msg, 5*time.Second)
	if err != nil {
		// Node offline or RPC failure — return zeros so the Overview tab
		// degrades gracefully (no quota row instead of an error toast).
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"usedBytes": 0,
			"count":     0,
			"degraded":  true,
		})
		return
	}
	usage := resp.GetBackupUsageResp()
	if usage == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"usedBytes": 0,
			"count":     0,
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"usedBytes": usage.UsedBytes,
		"count":     usage.Count,
	})
}

// ───────────── helpers ─────────────

// backupDeps assembles the runtime handles the storage factory passes to
// the node-local and core-storage providers. s3 / shared ignore all of it, so
// the same Deps can be reused everywhere a Storage is opened in this
// handler — keeps the call-sites uniform.
func (h *BackupHandler) backupDeps() backupstorage.Deps {
	return backupstorage.Deps{
		Registry:    h.state.GRPCRegistry,
		NodeStore:   h.state.Store,
		CoreStorage: h.state.CoreStorageBackupBuilder(),
		Connection:  h.state.ConnectionBackupBuilder(),
	}
}

// CoreStorageBackupBuilder returns the closure that opens the shared Core file
// storage as a backup backend. Exported because the backup scheduler lives in
// package services and is wired from main.go, which cannot reach the
// unexported provider builder - and a scheduler without this is refused by
// backupstorage.Open for every "core-storage" job, which is how the reaper's
// storage probe came to be inert for that provider.
//
// The returned closure resolves PER CALL, never caching: the shared Core file
// storage config can change under a running Core, and every other provider in
// this codebase resolves per request too.
func (s *AppState) CoreStorageBackupBuilder() func(subPrefix string) (backupstorage.Storage, error) {
	return func(subPrefix string) (backupstorage.Storage, error) {
		prov, err := s.buildCoreStorageProvider(subPrefix)
		if err != nil {
			return nil, err
		}
		return storage.NewCoreStorageBackupAdapter(prov), nil
	}
}

// ConnectionBackupBuilder returns the closure that opens a saved storage
// connection as a backup backend, scoped to prefix. Exported for the same
// reason as CoreStorageBackupBuilder: the scheduler lives in package services
// and is wired from main.go, and without this every "connection" job would be
// refused by backupstorage.Open.
//
// Resolves PER CALL. That is the entire benefit of referencing a connection
// rather than copying its keys into the backup row: rotating the credential in
// one place takes effect everywhere, with no stale copy left behind.
func (s *AppState) ConnectionBackupBuilder() func(connectionID int, prefix string) (backupstorage.Storage, error) {
	return func(connectionID int, prefix string) (backupstorage.Storage, error) {
		if s.Store == nil {
			return nil, fmt.Errorf("storage connection %d: no store", connectionID)
		}
		conn, err := s.Store.GetStorageConnection(connectionID)
		if err != nil {
			return nil, fmt.Errorf("storage connection %d could not be loaded: %w", connectionID, err)
		}
		cfg := coreStorageConfigFromConnection(conn)
		if err := validateCoreStorageConfig(cfg); err != nil {
			return nil, fmt.Errorf("storage connection %q is not usable: %w", conn.Name, err)
		}
		// Same provider construction Core file storage uses, so a connection
		// behaves identically whichever subsystem points at it - including the
		// connection's own prefix, which nests ABOVE the backup prefix.
		prov, err := newStorageProviderForConfig(cfg, prefix, s.StorageGate, s.StorageS3)
		if err != nil {
			return nil, fmt.Errorf("storage connection %q: %w", conn.Name, err)
		}
		return storage.NewCoreStorageBackupAdapter(prov), nil
	}
}

// resolveServer parses the server ID from the URL and loads the row. Access
// is enforced at the route via RequireCap (routes.go) for every caller of
// this helper, so it only extracts data the handler needs, not authz.
func (h *BackupHandler) resolveServer(w http.ResponseWriter, r *http.Request) (int, *models.Server, bool) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid server ID", 400)
		return 0, nil, false
	}
	srv, err := h.state.Store.GetServerByID(id)
	if err != nil {
		sendJSONError(w, "Server not found", 404)
		return 0, nil, false
	}
	return id, srv, true
}

// resolveJobWithAccess loads the job and checks capID against the job's
// server. The /backup-jobs/{jobId} family resolves its server from the job
// row rather than a path {id}/{uuid}, so RequireCap cannot gate it at the
// route (Rule R5) - this stays in-handler, routed through the same resolver.
func (h *BackupHandler) resolveJobWithAccess(w http.ResponseWriter, r *http.Request, capID string) (int, *models.BackupJob, bool) {
	jobID, err := strconv.Atoi(mux.Vars(r)["jobId"])
	if err != nil {
		sendJSONError(w, "Invalid job ID", 400)
		return 0, nil, false
	}
	job, err := h.state.Store.GetBackupJob(jobID)
	if err != nil {
		sendJSONError(w, "Job not found", 404)
		return 0, nil, false
	}
	if !h.hasServerAccess(r, job.ServerID, capID) {
		sendJSONError(w, "Forbidden", 403)
		return 0, nil, false
	}
	return jobID, job, true
}

// hasServerAccess routes through the same capability resolver every
// route-gated handler uses: owner short-circuit, or a direct/proxy/account
// grant holding capID.
func (h *BackupHandler) hasServerAccess(r *http.Request, serverID int, capID string) bool {
	username, _ := r.Context().Value("username").(string)
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	userID, _ := r.Context().Value("userID").(string)
	res, err := h.state.Authz.Resolve(authz.Identity{UserID: userID, Username: username, IsAdmin: isAdmin}, serverID)
	return err == nil && res.HasCap(capID)
}

// startBackupRun creates a backup_run row, dispatches a node command, and
// returns the new run-id. The actual heavy lifting happens on the node; this
// function returns immediately so the HTTP request stays snappy.
func (h *BackupHandler) startBackupRun(ctx context.Context, job *models.BackupJob) (int, error) {
	// The server first, because who owns it decides which storage answers.
	srv, err := h.state.Store.GetServerByID(job.ServerID)
	if err != nil {
		return 0, fmt.Errorf("server not found: %w", err)
	}
	// One chain, the same one the scheduler walks: the job's storage, else the
	// owner's own default, else the platform's. This used to hand-roll two of
	// the three steps and skip the owner entirely, so a tenant who had connected
	// their own bucket had it honoured on some paths and ignored on this one.
	storage, err := services.ResolveJobStorage(h.state.Store, job.StorageID, srv.OwnerID)
	if err != nil {
		if errors.Is(err, services.ErrNoBackupStorage) {
			return 0, fmt.Errorf("no storage configured - set a default in Settings, Backups first")
		}
		return 0, fmt.Errorf("storage not found: %w", err)
	}
	// Platform backup allowance for the SERVER'S OWNER, not for whoever pressed
	// the button - an administrator running a backup on a customer's server has
	// to meet the customer's ceiling, or the ceiling means nothing. Skipped
	// where `storage` is one the tenant connected themselves; see
	// BackupAllowanceExceeded.
	if exceeded, used, quota := services.BackupAllowanceExceeded(h.state.Store, srv.OwnerID, h.state.StoreEnabled, storage); exceeded {
		return 0, backupQuotaRefusal(used, quota, "", "raise the limit in Settings, Backups")
	}
	// Per-server node-local cap. Separate budget from the allowance above: that
	// counts a tenant's object-storage bytes, this counts the .dylaris-backups/
	// folder on the MC host, and only one of the two modes is ever active.
	//
	// No administrator exemption here, unlike the allowance: this bounds a real
	// disk, and a full one takes down every server on the host including other
	// people's.
	if exceeded, used, quota := services.NodeLocalBackupQuotaExceeded(h.state.Store, h.state.GRPCRegistry, srv); exceeded {
		return 0, backupQuotaRefusal(used, quota, " on this server", "raise the per-server limit in Settings, Backups")
	}
	node, err := h.state.Store.GetNodeByID(srv.NodeID)
	if err != nil {
		return 0, fmt.Errorf("node not found: %w", err)
	}

	storageKey := fmt.Sprintf("backups/%s/job-%d/%s.tar.gz", srv.UUID, job.ID, time.Now().UTC().Format("20060102-150405"))
	runID, err := h.state.Store.CreateBackupRun(&models.BackupRun{
		JobID:  job.ID,
		Status: "running",
		// Recorded from the storage this dispatch resolved, so the archive stays
		// findable if the job is pointed elsewhere later.
		StorageID:  &storage.ID,
		StorageKey: storageKey,
	})
	if err != nil {
		return 0, err
	}

	// Mark scheduled bookkeeping. Manual triggers don't change the next-run.
	now := time.Now()
	next := services.ComputeBackupNextRun(job.Schedule, now)
	if next != nil {
		h.state.Store.SetBackupJobScheduled(job.ID, now, *next)
	}

	if h.state.Queue == nil {
		return runID, fmt.Errorf("queue unavailable")
	}
	// BYON nodes get a presigned PUT URL + creds-stripped storage so the tenant's
	// machine never receives the bucket credentials. Operator nodes are unchanged.
	storageCfgJSON, presignedPut, err := services.PrepareNodeStorage(ctx, h.state.Store, storage, node, storageKey, "put", h.backupDeps())
	if err != nil {
		h.state.Store.UpdateBackupRunStatus(runID, "failed", err.Error(), 0, "", time.Now())
		return runID, err
	}
	// What this archive contains, described. The SAME bytes go to the node (to
	// be written into the archive as its first entry) and onto the run row, so a
	// downloaded archive describes itself on a foreign platform while a
	// same-instance restore reads the row and fetches nothing. Built here rather
	// than when the node reports back, so it describes the moment the archive
	// was taken and not the moment the report arrived.
	//
	// Best-effort: a description that could not be written is not a reason to
	// refuse a backup. The restore side already treats an absent manifest as
	// "this archive says nothing" and leaves the rows alone.
	manifest := services.EncodeBackupManifest(
		services.BuildBackupManifest(h.state.Store, srv.ID, deref(job.SubServer), h.state.ReleaseVersion))
	if len(manifest) > 0 {
		if err := h.state.Store.SetBackupRunManifest(runID, string(manifest)); err != nil {
			log.Printf("backup manifest: run %d: %v", runID, err)
		}
	}
	payload := map[string]interface{}{
		"action":          "backup_run",
		"runId":           runID,
		"jobId":           job.ID,
		"serverUuid":      srv.UUID,
		"subServer":       deref(job.SubServer),
		"includePatterns": job.IncludePatterns,
		"excludePatterns": job.ExcludePatterns,
		"storageKey":      storageKey,
		"storage":         json.RawMessage(storageCfgJSON),
		"presignedPutUrl": presignedPut,
	}
	// Publish to the node's durable :cmds stream (BC1) instead of RPush to the
	// retired dylaris:node:<token>:queue list, which nothing reads anymore.
	// Only when there IS one: json.RawMessage(nil) marshals to the four bytes
	// "null", and the node writes what it is given without parsing it - so an
	// absent manifest would reach the archive as a file containing "null".
	if len(manifest) > 0 {
		payload["manifest"] = json.RawMessage(manifest)
	}
	if err := h.state.Queue.SendRawCommand(ctx, node.Token, payload); err != nil {
		h.state.Store.UpdateBackupRunStatus(runID, "failed", "queue push failed: "+err.Error(), 0, "", time.Now())
		return runID, err
	}
	return runID, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// strReader is a tiny helper used by the TestStorage probe.
//
// Deliberately NOT strings.NewReader, and not to be "tidied" into one. This
// returns a bare io.Reader: no Seek, no Len. That is what a real backup body
// looks like - an archive streams out of an io.Pipe while it is still being
// written - and it is what makes the probe exercise the same S3 request shape
// a real upload takes.
//
// It has already earned that once. The core-storage probe next door DOES use
// strings.NewReader, so it stayed green against Cloudflare R2 while every
// backup write to the same bucket failed with BadDigest, because a seekable
// body let the SDK put its checksum in a header instead of an aws-chunked
// trailer. A probe that is easier on the backend than the real path is a probe
// that reports success for a thing that does not work.
func strReader(s string) io.Reader {
	return &stringReader{s: s}
}

type stringReader struct {
	s   string
	off int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.off >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.off:])
	r.off += n
	return n, nil
}
