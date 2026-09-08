package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"

	"dylaris-core/models"
	"dylaris-core/pkg/crypto"
	"dylaris-core/services"
	"dylaris-core/storage"
	backupstorage "dylaris-core/storage/backup"
)

// The platform's OWN backups, as opposed to a game server's.
//
// Every route here is settings.write, including the READ of a run's archive.
// That is not uniformity for its own sake: a platform bundle contains the whole
// database, and the database holds every node secret, every storage credential
// and every user row. Whoever holds the file and the passphrase holds the
// platform. settings.read is the capability for looking at configuration; it is
// not the capability for downloading the installation.
type PlatformBackupHandler struct {
	state *AppState
}

func NewPlatformBackupHandler(state *AppState) *PlatformBackupHandler {
	return &PlatformBackupHandler{state: state}
}

// ───────────── Jobs ─────────────

// ListJobs GET /api/platform-backups/jobs
func (h *PlatformBackupHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.state.Store.ListPlatformBackupJobs()
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "jobs": jobs})
}

type platformBackupJobRequest struct {
	Name           string                         `json:"name"`
	Schedule       string                         `json:"schedule"`
	Selection      models.PlatformBackupSelection `json:"selection"`
	StorageID      *int                           `json:"storageId"`
	RetentionCount *int                           `json:"retentionCount"`
	Enabled        *bool                          `json:"enabled"`
}

// CreateJob POST /api/platform-backups/jobs
func (h *PlatformBackupHandler) CreateJob(w http.ResponseWriter, r *http.Request) {
	var req platformBackupJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		sendJSONError(w, "A name is required", http.StatusBadRequest)
		return
	}
	if err := req.Selection.Validate(); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}

	job := &models.PlatformBackupJob{
		Name:           strings.TrimSpace(req.Name),
		Schedule:       scheduleOrManual(req.Schedule),
		Selection:      req.Selection,
		StorageID:      req.StorageID,
		RetentionCount: derefInt(req.RetentionCount, 3),
		Enabled:        derefBool(req.Enabled, true),
	}
	id, err := h.state.Store.CreatePlatformBackupJob(job)
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	job.ID = id
	json.NewEncoder(w).Encode(map[string]any{"success": true, "job": job})
}

// UpdateJob PATCH /api/platform-backups/jobs/{id}
func (h *PlatformBackupHandler) UpdateJob(w http.ResponseWriter, r *http.Request) {
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	var req platformBackupJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		job.Name = strings.TrimSpace(req.Name)
	}
	if req.Schedule != "" {
		job.Schedule = scheduleOrManual(req.Schedule)
	}
	// A selection is replaced wholesale rather than merged: the components are
	// booleans, and a merge could never express "stop including the library".
	if err := req.Selection.Validate(); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	job.Selection = req.Selection
	job.StorageID = req.StorageID
	if req.RetentionCount != nil {
		job.RetentionCount = *req.RetentionCount
	}
	if req.Enabled != nil {
		job.Enabled = *req.Enabled
	}

	if err := h.state.Store.UpdatePlatformBackupJob(job); err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "job": job})
}

// DeleteJob DELETE /api/platform-backups/jobs/{id}
func (h *PlatformBackupHandler) DeleteJob(w http.ResponseWriter, r *http.Request) {
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	if err := h.state.Store.DeletePlatformBackupJob(job.ID); err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true})
}

// RunJob POST /api/platform-backups/jobs/{id}/run
//
// Synchronous on purpose, for now: a platform run is started by a person who is
// looking at the screen, and the components it covers are the database and the
// storage areas rather than every world. The servers it selects are handed to
// their own jobs, which are the asynchronous part.
func (h *PlatformBackupHandler) RunJob(w http.ResponseWriter, r *http.Request) {
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	runner, err := h.runner()
	if err != nil {
		sendJSONError(w, err.Error(), 500)
		return
	}
	runID, rerr := runner.Run(r.Context(), job.ID)
	if rerr != nil {
		status := 500
		if errors.Is(rerr, services.ErrNoBackupPassphrase) ||
			errors.Is(rerr, models.ErrEmptyPlatformBackupSelection) {
			// A refusal by policy is the system working. Answering 500 puts it
			// in the same bucket as a broken queue, which is what alerting and
			// any API client key off.
			status = http.StatusConflict
		}
		// The run id is returned even on failure when one was opened, so the
		// screen can show WHICH run to look at rather than only that something
		// went wrong.
		json.NewEncoder(w).Encode(map[string]any{
			"success": false, "message": rerr.Error(), "runId": runID,
		})
		w.WriteHeader(status)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "runId": runID})
}

// ListRuns GET /api/platform-backups/jobs/{id}/runs
func (h *PlatformBackupHandler) ListRuns(w http.ResponseWriter, r *http.Request) {
	job, ok := h.loadJob(w, r)
	if !ok {
		return
	}
	runs, err := h.state.Store.ListPlatformBackupRuns(job.ID, 50)
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "runs": runs})
}

// DownloadRun GET /api/platform-backups/runs/{id}/download
//
// Streamed through Core rather than redirected to a presigned URL. A presigned
// link to a platform bundle is a bearer token for the whole installation that
// outlives the session and travels in a URL - through browser history, a proxy
// log and anything that records one.
func (h *PlatformBackupHandler) DownloadRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid run id", http.StatusBadRequest)
		return
	}
	run, err := h.state.Store.GetPlatformBackupRun(id)
	if err != nil || run == nil {
		sendJSONError(w, "Run not found", http.StatusNotFound)
		return
	}
	if run.Status != "success" || run.StorageKey == "" {
		sendJSONError(w, "This run produced no archive", http.StatusConflict)
		return
	}
	bs, err := services.ResolveJobStorage(h.state.Store, run.StorageID, "")
	if err != nil {
		sendJSONError(w, "The storage this run wrote to is not available", http.StatusConflict)
		return
	}
	store, err := backupstorage.Open(r.Context(), bs, NewBackupHandler(h.state).backupDeps())
	if err != nil {
		sendJSONError(w, "Could not open the storage", 500)
		return
	}
	rc, err := store.Get(r.Context(), run.StorageKey)
	if err != nil {
		sendJSONError(w, "The archive could not be read", 500)
		return
	}
	defer rc.Close()

	name := fmt.Sprintf("dylaris-platform-%d-%s.dylaris-bundle", run.ID, run.StartedAt.UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if run.SizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(run.SizeBytes, 10))
	}
	io.Copy(w, rc)
}

// ListTargets GET /api/platform-backups/targets
//
// What the selection screen offers. The BYON flag travels with each row because
// the panel filters on it, and "all BYON servers" is only a meaningful choice on
// a store-connected platform - a self-hoster has no BYON tenants to tell apart.
func (h *PlatformBackupHandler) ListTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := h.state.Store.ListBackupTargetServers()
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"success":     true,
		"targets":     targets,
		"byonOffered": h.state.StoreEnabled,
	})
}

// ───────────── Passphrase ─────────────

// PassphraseStatus GET /api/platform-backups/passphrase - whether one is set,
// never what it is.
func (h *PlatformBackupHandler) PassphraseStatus(w http.ResponseWriter, r *http.Request) {
	v, err := h.state.Store.GetSetting(services.PlatformBackupPassphraseSetting)
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true, "isSet": v != ""})
}

type setPassphraseRequest struct {
	Passphrase string `json:"passphrase"`
	// Replace must be sent when one is already set. Not a formality: bundles
	// already written keep the OLD passphrase, and nothing re-encrypts them.
	Replace bool `json:"replace"`
}

// SetPassphrase PUT /api/platform-backups/passphrase
func (h *PlatformBackupHandler) SetPassphrase(w http.ResponseWriter, r *http.Request) {
	var req setPassphraseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.Passphrase) < crypto.MinPassphraseLength {
		sendJSONError(w, fmt.Sprintf("The passphrase must be at least %d characters", crypto.MinPassphraseLength), http.StatusBadRequest)
		return
	}
	existing, err := h.state.Store.GetSetting(services.PlatformBackupPassphraseSetting)
	if err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	if existing != "" && !req.Replace {
		// Refused rather than overwritten, because the consequence is invisible
		// at the moment of the change and only appears at a restore: every
		// bundle written under the old passphrase still needs the old one.
		sendJSONError(w,
			"A backup passphrase is already set. Replacing it does not re-encrypt bundles already written - those still need the old passphrase. Send replace: true to confirm.",
			http.StatusConflict)
		return
	}
	if err := h.state.Store.SetSetting(services.PlatformBackupPassphraseSetting, req.Passphrase); err != nil {
		sendJSONError(w, "Database error", 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"success": true})
}

// ───────────── wiring ─────────────

// runner builds a PlatformBackupRunner from this Core's own configuration.
//
// Built per request rather than held: the destination, the core storage config
// and the database version can all change under a running Core, and every other
// provider in this codebase resolves per request too.
func (h *PlatformBackupHandler) runner() (*services.PlatformBackupRunner, error) {
	if h.state.PlatformDB.Name == "" {
		return nil, errors.New("this Core does not know how to reach its own database for a backup")
	}
	return &services.PlatformBackupRunner{
		Store:         h.state.Store,
		Release:       h.state.ReleaseVersion,
		ClusterSecret: h.state.ClusterSecret,
		WorkDir:       h.state.PlatformBackupWorkDir,
		DB:            h.state.PlatformDB,
		ServerMajor:   h.state.PlatformDBMajor,

		OpenDest: func(ctx context.Context, bs *models.BackupStorage) (services.BundleDestination, error) {
			return backupstorage.Open(ctx, bs, NewBackupHandler(h.state).backupDeps())
		},
		OpenCoreStorage: func(area string) (services.CoreStorageArea, error) {
			prefix := CoreStoragePrefixLibrary
			if area == "modpacks" {
				prefix = CoreStoragePrefixModpacks
			}
			prov, err := h.state.buildCoreStorageProvider(prefix)
			if err != nil {
				return nil, err
			}
			return &coreStorageArea{prov: prov}, nil
		},
		TriggerServerBackup: h.triggerServerBackup,
		MetricsConfigured: func() bool {
			return services.LoadMetricsDBTarget(h.state.Store).IsSeparate()
		},
	}, nil
}

// triggerServerBackup runs a server's OWN backup jobs.
//
// Its own jobs, not a fresh archive taken by Core: the archive then lands under
// that server owner's quota, retention and destination and stays individually
// restorable, and no world travels through Core. A server with no job at all is
// an error rather than a silent success - the operator selected it, and
// pretending it was covered is the one answer that gets someone hurt later.
func (h *PlatformBackupHandler) triggerServerBackup(ctx context.Context, serverID int) (string, error) {
	jobs, err := h.state.Store.ListBackupJobs(serverID)
	if err != nil {
		return "", fmt.Errorf("reading the server's backup jobs: %w", err)
	}
	var started []string
	for i := range jobs {
		if !jobs[i].Enabled {
			continue
		}
		runID, err := h.startBackupRunForPlatform(ctx, &jobs[i])
		if err != nil {
			return "", fmt.Errorf("job %q: %w", jobs[i].Name, err)
		}
		started = append(started, fmt.Sprintf("run %d", runID))
	}
	if len(started) == 0 {
		return "", errors.New("this server has no enabled backup job, so a platform run cannot cover it")
	}
	return strings.Join(started, ", "), nil
}

// startBackupRunForPlatform dispatches one server backup job through the same
// path the manual trigger uses, so quota, storage resolution and the run record
// behave identically.
func (h *PlatformBackupHandler) startBackupRunForPlatform(ctx context.Context, job *models.BackupJob) (int, error) {
	return NewBackupHandler(h.state).startBackupRun(ctx, job)
}

// coreStorageArea adapts a scoped Core storage provider to what the runner
// needs: enumerate, and open one object.
type coreStorageArea struct {
	prov storage.StorageProvider
}

func (a *coreStorageArea) Walk(ctx context.Context) ([]services.BundleFile, error) {
	// ListFiles returns ONE directory level on every backend, so the full key
	// space needs the recursive walk rather than a single listing.
	files, err := storage.WalkProvider(ctx, a.prov, "")
	if err != nil {
		return nil, err
	}
	out := make([]services.BundleFile, 0, len(files))
	for _, f := range files {
		out = append(out, services.BundleFile{Key: f.Key, Size: f.Size})
	}
	return out, nil
}

func (a *coreStorageArea) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	return a.prov.GetFile(ctx, key)
}

func scheduleOrManual(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "manual"
	}
	return s
}

func derefInt(v *int, fallback int) int {
	if v == nil {
		return fallback
	}
	return *v
}

func derefBool(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func (h *PlatformBackupHandler) loadJob(w http.ResponseWriter, r *http.Request) (*models.PlatformBackupJob, bool) {
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid job id", http.StatusBadRequest)
		return nil, false
	}
	job, err := h.state.Store.GetPlatformBackupJob(id)
	if err != nil || job == nil {
		sendJSONError(w, "Job not found", http.StatusNotFound)
		return nil, false
	}
	return job, true
}
