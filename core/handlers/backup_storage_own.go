package handlers

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/models"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// A tenant's own object storage for backups.
//
// Deliberately its own route set rather than an owner branch inside the admin
// handlers. Those are platform-wide by definition - they list every storage and
// offer any of them as a target - and bolting "unless the caller is a tenant"
// onto each one is how a surface ends up enforcing the rule on three paths out
// of four. Here every handler starts from the caller's own id and cannot be
// asked about anybody else's row.
//
// The bytes are then outside our billing entirely: they do not count toward the
// backup quota, they are not metered, and the retention sweep never deletes from
// a bucket we do not pay for.

// ownBackupStorageProviders is what a TENANT may point at, and it is one entry
// long on purpose.
//
// The platform's own providers are not merely unnecessary here, they are a way
// out of the account: "local" and "shared" write to a path on Core's filesystem,
// "node-local" writes onto a machine the tenant may not own, and "connection"
// dereferences a storage connection configured by an admin. Each would let a
// tenant address storage that is not theirs by naming it in a config blob.
func ownBackupStorageAllowed(provider string) bool { return provider == "s3" }

// ownStorageFor loads a storage and confirms it belongs to the caller.
//
// Not found and belongs-to-somebody-else answer the SAME 404. A distinct 403
// would confirm that a given id exists, which is the one thing an enumeration
// over /api/me/backup-storages/{id} is trying to learn.
func (h *BackupHandler) ownStorageFor(w http.ResponseWriter, r *http.Request) (*models.BackupStorage, bool) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return nil, false
	}
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid ID", http.StatusBadRequest)
		return nil, false
	}
	bs, err := h.state.Store.GetBackupStorage(id)
	if err != nil || bs == nil || bs.OwnerID == nil || *bs.OwnerID != userID {
		sendJSONError(w, "Backup storage not found", http.StatusNotFound)
		return nil, false
	}
	return bs, true
}

// ListOwnStorages GET /api/me/backup-storages - the caller's own storages, with
// secrets stripped, and which of them is their default.
func (h *BackupHandler) ListOwnStorages(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	list, err := h.state.Store.ListBackupStoragesByOwner(userID)
	if err != nil {
		sendJSONError(w, "Failed to load your backup storages", http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []models.BackupStorage{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "storages": list})
}

// CreateOwnStorage POST /api/me/backup-storages - connects a bucket of the
// caller's own.
func (h *BackupHandler) CreateOwnStorage(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	var req models.BackupStorage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		sendJSONError(w, "name is required", http.StatusBadRequest)
		return
	}
	if !ownBackupStorageAllowed(req.Provider) {
		sendJSONError(w, "Only s3-compatible storage can be connected to an account", http.StatusBadRequest)
		return
	}
	if err := validateBackupStorageEndpoint(req); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Taken from the session, never from the body. A payload-supplied owner is
	// the whole attack: it would let any holder of this capability write a row
	// into somebody else's account, or into the PLATFORM scope by sending null,
	// where it becomes a target admins can select.
	req.OwnerID = &userID
	req.ID = 0
	id, err := h.state.Store.CreateBackupStorage(&req)
	if err != nil {
		if errors.Is(err, store.ErrNameTaken) {
			sendJSONError(w, "You already have a backup storage with that name", http.StatusConflict)
			return
		}
		sendJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": id})
}

// UpdateOwnStorage PATCH /api/me/backup-storages/{id}
func (h *BackupHandler) UpdateOwnStorage(w http.ResponseWriter, r *http.Request) {
	existing, ok := h.ownStorageFor(w, r)
	if !ok {
		return
	}
	var req models.BackupStorage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if !ownBackupStorageAllowed(req.Provider) {
		sendJSONError(w, "Only s3-compatible storage can be connected to an account", http.StatusBadRequest)
		return
	}
	if err := validateBackupStorageEndpoint(req); err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Identity comes from the row that was just checked, not from the body: an
	// id or owner in the payload would be a way to edit a different row than the
	// one the ownership check passed on.
	req.ID = existing.ID
	req.OwnerID = existing.OwnerID
	// Same write-only-secret handling the admin path has: the form never
	// receives the secret, so a save carries none, and the stored one is kept
	// unless the endpoint, bucket or access key moved - in which case reusing it
	// would point the tenant's credential at a target they did not authenticate
	// against.
	merged, merr := mergeBackupStorageSecret(req, existing)
	if merr != nil {
		sendJSONError(w, merr.Error(), http.StatusBadRequest)
		return
	}
	merged.ID, merged.OwnerID = existing.ID, existing.OwnerID
	if err := h.state.Store.UpdateBackupStorage(&merged); err != nil {
		switch {
		case errors.Is(err, store.ErrNameTaken):
			sendJSONError(w, "You already have a backup storage with that name", http.StatusConflict)
		case errors.Is(err, sql.ErrNoRows):
			sendJSONError(w, "Backup storage not found", http.StatusNotFound)
		default:
			sendJSONError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// DeleteOwnStorage DELETE /api/me/backup-storages/{id}
//
// The archives already written to it are NOT deleted: they are in a bucket the
// tenant controls, and this only removes our record of how to reach it. Their
// run rows survive with a dangling storage reference, which the run's own
// storage_id turns into an honest "the storage is gone" rather than a silent
// re-resolution onto ours.
func (h *BackupHandler) DeleteOwnStorage(w http.ResponseWriter, r *http.Request) {
	bs, ok := h.ownStorageFor(w, r)
	if !ok {
		return
	}
	if err := h.state.Store.DeleteBackupStorage(bs.ID); err != nil {
		sendJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// TestOwnStorage POST /api/me/backup-storages/{id}/test - the same round trip
// the admin path runs, against the caller's own storage only.
func (h *BackupHandler) TestOwnStorage(w http.ResponseWriter, r *http.Request) {
	bs, ok := h.ownStorageFor(w, r)
	if !ok {
		return
	}
	provider, err := backupstorage.Open(r.Context(), bs, h.backupDeps())
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	if ok, msg := probeBackupStorage(r.Context(), provider); !ok {
		// The guard's refusal names the address it resolved, and a tenant can
		// type any internal name as the endpoint: that would map our network.
		if strings.Contains(msg, "blocked non-public address") {
			msg = "This endpoint resolves to a private or internal address. Use your storage provider's public endpoint."
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": msg})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
