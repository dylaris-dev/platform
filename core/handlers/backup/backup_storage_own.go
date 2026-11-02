package backup

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/handlers/httpx"
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

// refuseOwnStorageOff answers 403 own_storage_disabled when the operator has
// switched own storage off for the caller, and reports whether it did. Only
// adding a target and choosing one are refused. Listing, deleting, and
// updating or testing an existing one stay open: data already on an own target
// stays reachable, and rotating its credentials writes no new data there.
func (h *BackupHandler) refuseOwnStorageOff(w http.ResponseWriter, r *http.Request) bool {
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if h.state.FeatureFlags.OwnStorageAllowed(r.Context(), isAdmin) {
		return false
	}
	httpx.SendJSONErrorCode(w, "Your administrator has turned off own storage. Existing data stays reachable.", "own_storage_disabled", http.StatusForbidden)
	return true
}

// ownStorageFor loads a storage and confirms it belongs to the caller.
//
// Not found and belongs-to-somebody-else answer the SAME 404. A distinct 403
// would confirm that a given id exists, which is the one thing an enumeration
// over /api/me/backup-storages/{id} is trying to learn.
func (h *BackupHandler) ownStorageFor(w http.ResponseWriter, r *http.Request) (*models.BackupStorage, bool) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return nil, false
	}
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		httpx.SendJSONError(w, "Invalid ID", http.StatusBadRequest)
		return nil, false
	}
	bs, err := h.state.Store.GetBackupStorage(id)
	if err != nil || bs == nil || bs.OwnerID == nil || *bs.OwnerID != userID {
		httpx.SendJSONError(w, "Backup storage not found", http.StatusNotFound)
		return nil, false
	}
	return bs, true
}

// ListOwnStorages GET /api/me/backup-storages - the caller's own storages, with
// secrets stripped, and which of them is their default.
func (h *BackupHandler) ListOwnStorages(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	list, err := h.state.Store.ListBackupStoragesByOwner(userID)
	if err != nil {
		httpx.SendJSONError(w, "Failed to load your backup storages", http.StatusInternalServerError)
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
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if h.refuseOwnStorageOff(w, r) {
		return
	}
	var req models.BackupStorage
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		httpx.SendJSONError(w, "name is required", http.StatusBadRequest)
		return
	}
	if !ownBackupStorageAllowed(req.Provider) {
		httpx.SendJSONError(w, "Only s3-compatible storage can be connected to an account", http.StatusBadRequest)
		return
	}
	if err := validateBackupStorageEndpoint(req); err != nil {
		httpx.SendJSONError(w, err.Error(), http.StatusBadRequest)
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
			httpx.SendJSONError(w, "You already have a backup storage with that name", http.StatusConflict)
			return
		}
		httpx.SendJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := h.syncBackupsPref(userID, id, req.IsDefault); err != nil {
		log.Printf("own storage %d: backups preference for %s: %v", id, userID, err)
		httpx.SendJSONError(w, "The storage was saved, but choosing it for backups failed. Choose it under Account > Storage.", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": id})
}

// syncBackupsPref keeps the backups preference following the "default" flag:
// marking a storage default chooses it, clearing the flag on the chosen one
// returns backups to the chain. One statement on the backups column only, so
// it cannot undo a templates choice (or a backups choice) saved meanwhile.
func (h *BackupHandler) syncBackupsPref(userID string, id int, isDefault bool) error {
	if isDefault {
		return h.state.Store.SetUserBackupsStoragePref(userID, &id)
	}
	return h.state.Store.ClearUserBackupsStoragePrefIf(userID, id)
}

// clearPrefsNaming returns both of the user's choices that name id to the
// platform storage. The templates read-then-write cannot race a choice of an
// own target: it is only called while choosing one is refused.
func (h *BackupHandler) clearPrefsNaming(userID string, id int) error {
	if err := h.state.Store.ClearUserBackupsStoragePrefIf(userID, id); err != nil {
		return err
	}
	_, tpl, err := h.state.Store.GetUserStoragePrefs(userID)
	if err != nil || tpl == nil || *tpl != id {
		return err
	}
	return h.state.Store.SetUserTemplatesStoragePref(userID, nil)
}

// UpdateOwnStorage PATCH /api/me/backup-storages/{id}
func (h *BackupHandler) UpdateOwnStorage(w http.ResponseWriter, r *http.Request) {
	existing, ok := h.ownStorageFor(w, r)
	if !ok {
		return
	}
	// isDefault is a pointer here, shadowing the embedded field: an edit form
	// that re-sends a flag it loaded earlier must not move the backups target,
	// which /api/me/storage-prefs chooses. Absent leaves flag and choice alone.
	var body struct {
		models.BackupStorage
		IsDefault *bool `json:"isDefault"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	// Marking it default chooses it for backups, which is refused while off;
	// clearing the flag only moves backups away from it and stays allowed.
	if body.IsDefault != nil && *body.IsDefault && h.refuseOwnStorageOff(w, r) {
		return
	}
	req := body.BackupStorage
	req.IsDefault = existing.IsDefault
	if body.IsDefault != nil {
		req.IsDefault = *body.IsDefault
	}
	if !ownBackupStorageAllowed(req.Provider) {
		httpx.SendJSONError(w, "Only s3-compatible storage can be connected to an account", http.StatusBadRequest)
		return
	}
	if err := validateBackupStorageEndpoint(req); err != nil {
		httpx.SendJSONError(w, err.Error(), http.StatusBadRequest)
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
		httpx.SendJSONError(w, merr.Error(), http.StatusBadRequest)
		return
	}
	merged.ID, merged.OwnerID = existing.ID, existing.OwnerID
	if err := h.state.Store.UpdateBackupStorage(&merged); err != nil {
		switch {
		case errors.Is(err, store.ErrNameTaken):
			httpx.SendJSONError(w, "You already have a backup storage with that name", http.StatusConflict)
		case errors.Is(err, sql.ErrNoRows):
			httpx.SendJSONError(w, "Backup storage not found", http.StatusNotFound)
		default:
			httpx.SendJSONError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if body.IsDefault == nil {
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	if err := h.syncBackupsPref(*existing.OwnerID, existing.ID, merged.IsDefault); err != nil {
		log.Printf("own storage %d: backups preference for %s: %v", existing.ID, *existing.OwnerID, err)
		httpx.SendJSONError(w, "The storage was saved, but updating your backups choice failed. Check it under Account > Storage.", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// DeleteOwnStorage DELETE /api/me/backup-storages/{id}
//
// Refused while a backup, a schedule, a storage choice or a template still names
// it (store.ErrStorageInUse): the reference would fall back to the platform
// default, so the tenant's archives would be billed as ours and a restore would
// fetch from a bucket that never held them.
func (h *BackupHandler) DeleteOwnStorage(w http.ResponseWriter, r *http.Request) {
	bs, ok := h.ownStorageFor(w, r)
	if !ok {
		return
	}
	// With own storage off the choices are ignored and the Storage page shows
	// none, so a choice naming this target would block the delete with nothing
	// the user can see to undo. Dropping it first is what they would do anyway.
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if !h.state.FeatureFlags.OwnStorageAllowed(r.Context(), isAdmin) {
		if err := h.clearPrefsNaming(*bs.OwnerID, bs.ID); err != nil {
			log.Printf("own storage %d: clear prefs: %v", bs.ID, err)
			httpx.SendJSONError(w, "Could not remove this storage", http.StatusInternalServerError)
			return
		}
	}
	if err := h.state.Store.DeleteBackupStorage(bs.ID); err != nil {
		if errors.Is(err, store.ErrStorageInUse) {
			httpx.SendJSONError(w, "Backups, backup schedules, templates or your storage choices still use this storage. Under Account > Storage, choose another storage for backups and templates; then move the schedules, and delete the backups and templates stored in it.", http.StatusConflict)
			return
		}
		httpx.SendJSONError(w, err.Error(), http.StatusInternalServerError)
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
			// The operator, not the tenant, can change this, so the hint goes
			// to the log with the address it would have to allow.
			log.Printf("own storage %d: test refused a non-public address; an operator can allow a private network with STORAGE_PRIVATE_ALLOW: %s", bs.ID, msg)
			msg = "This endpoint resolves to a private or internal address. Use your storage provider's public endpoint."
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": msg})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
