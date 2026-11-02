package backup

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
	"dylaris-core/services"
	backupstorage "dylaris-core/storage/backup"
	"dylaris-core/store"

	"github.com/gorilla/mux"
)

// adminStorageTarget is what an admin sees of a user's own storage target.
// Built field by field rather than from the stored row so the access key and
// the secret cannot reach the response: the list path strips the secret, but
// not the access key, which is the tenant's credential for their own bucket.
type adminStorageTarget struct {
	ID           int                       `json:"id"`
	Name         string                    `json:"name"`
	Provider     string                    `json:"provider"`
	EndpointHost string                    `json:"endpointHost"`
	Bucket       string                    `json:"bucket"`
	Prefix       string                    `json:"prefix"`
	IsDefault    bool                      `json:"isDefault"`
	CreatedAt    time.Time                 `json:"createdAt"`
	Usage        models.BackupStorageUsage `json:"usage"`
}

// AdminListUserStorages GET /api/admin/users/{id}/storage-targets - a user's
// own storage targets, with what still uses each one. Never the credentials.
func (h *BackupHandler) AdminListUserStorages(w http.ResponseWriter, r *http.Request) {
	list, err := h.d.Store.ListBackupStoragesByOwner(mux.Vars(r)["id"])
	if err != nil {
		httpx.SendJSONError(w, "Failed to load the user's storage targets", http.StatusInternalServerError)
		return
	}
	out := make([]adminStorageTarget, 0, len(list))
	for _, bs := range list {
		var cfg backupstorage.S3Config
		_ = json.Unmarshal(bs.Config, &cfg)
		host, _, _ := services.HostPortFromEndpoint(cfg.Endpoint)
		usage, err := h.d.Store.BackupStorageUsage(bs.ID)
		if err != nil {
			httpx.SendJSONError(w, "Failed to load the user's storage targets", http.StatusInternalServerError)
			return
		}
		out = append(out, adminStorageTarget{
			ID: bs.ID, Name: bs.Name, Provider: bs.Provider,
			EndpointHost: host, Bucket: cfg.Bucket, Prefix: cfg.Prefix,
			IsDefault: bs.IsDefault, CreatedAt: bs.CreatedAt, Usage: usage,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "storages": out})
}

// AdminDeleteUserStorage DELETE /api/admin/users/{id}/storage-targets/{storageId}
//
// Refused while anything still names the target, exactly as the user's own
// delete is, and for the same reason: the reference would fall back to the
// platform storage. A platform storage or another user's target answers 404.
func (h *BackupHandler) AdminDeleteUserStorage(w http.ResponseWriter, r *http.Request) {
	userID := mux.Vars(r)["id"]
	id, err := strconv.Atoi(mux.Vars(r)["storageId"])
	if err != nil {
		httpx.SendJSONError(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	bs, err := h.d.Store.GetBackupStorage(id)
	if err != nil || bs == nil || bs.OwnerID == nil || *bs.OwnerID != userID {
		httpx.SendJSONError(w, "Storage target not found", http.StatusNotFound)
		return
	}
	if err := h.d.Store.DeleteBackupStorage(bs.ID); err != nil {
		if errors.Is(err, store.ErrStorageInUse) {
			httpx.SendJSONError(w, "Backups, backup schedules, templates or the user's storage choices still use this storage target. The user has to move or delete them first.", http.StatusConflict)
			return
		}
		httpx.SendJSONError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	actor, _ := r.Context().Value("userID").(string)
	log.Printf("admin %s deleted storage target %d of user %s", actor, bs.ID, userID)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
