package backup

import (
	"encoding/json"
	"log"
	"net/http"

	"dylaris-core/handlers/httpx"
)

// Where a tenant's NEW backups and templates go: Dylaris storage (null) or one
// of their own s3 targets. Changing it never moves data that already exists.

// GetStoragePrefs GET /api/me/storage-prefs
func (h *BackupHandler) GetStoragePrefs(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	// With own storage off, new data goes to the platform storage whatever is
	// saved, so that is what is shown. The saved choice is kept for later.
	isAdmin, _ := r.Context().Value("isAdmin").(bool)
	if !h.d.FeatureFlags.OwnStorageAllowed(r.Context(), isAdmin) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "backupsStorageId": nil, "templatesStorageId": nil,
		})
		return
	}
	backups, templates, err := h.d.Store.GetUserStoragePrefs(userID)
	if err != nil {
		log.Printf("storage prefs for %s: %v", userID, err)
		httpx.SendJSONError(w, "Failed to load your storage choices", http.StatusInternalServerError)
		return
	}
	// Show what backups actually use: a NULL pref resolves to the own default
	// (ResolveJobStorage), so showing "Dylaris storage" there would be wrong.
	if backups == nil {
		def, err := h.d.Store.GetUserDefaultBackupStorage(userID)
		if err != nil {
			log.Printf("storage prefs default for %s: %v", userID, err)
			httpx.SendJSONError(w, "Failed to load your storage choices", http.StatusInternalServerError)
			return
		}
		if def != nil {
			backups = &def.ID
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true, "backupsStorageId": backups, "templatesStorageId": templates,
	})
}

// PutStoragePrefs PUT /api/me/storage-prefs - body {"backupsStorageId": id|null,
// "templatesStorageId": id|null}; a field left out is not changed.
//
// Every field is validated before anything is written, and each one is then
// written to its own column: a read-modify-write of both would let two saves
// undo each other.
func (h *BackupHandler) PutStoragePrefs(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	// Two ids fit in a few bytes; the cap only stops a huge body from being decoded.
	if !httpx.CapBody(w, r, 4<<10) {
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	writes := []struct {
		field string
		set   func(string, *int) error
	}{
		{"backupsStorageId", h.d.Store.SetUserBackupsStoragePref},
		{"templatesStorageId", h.d.Store.SetUserTemplatesStoragePref},
	}
	chosen := map[string]*int{}
	for _, wr := range writes {
		raw, ok := body[wr.field]
		if !ok {
			continue
		}
		var id *int
		if err := json.Unmarshal(raw, &id); err != nil {
			httpx.SendJSONError(w, wr.field+" must be a storage id or null", http.StatusBadRequest)
			return
		}
		if id != nil {
			// Choosing the platform storage (null) stays allowed, so a user can
			// still move away from an own target after the switch went off.
			if h.refuseOwnStorageOff(w, r) {
				return
			}
			bs, err := h.d.Store.GetBackupStorage(*id)
			// Same 404 for missing and foreign as ownStorageFor: a distinct answer
			// would confirm that an id exists.
			if err != nil || bs == nil || bs.OwnerID == nil || *bs.OwnerID != userID {
				httpx.SendJSONError(w, "Storage not found", http.StatusNotFound)
				return
			}
			if !ownBackupStorageAllowed(bs.Provider) {
				httpx.SendJSONError(w, "Only s3-compatible storage can be chosen", http.StatusBadRequest)
				return
			}
		}
		chosen[wr.field] = id
	}
	if len(chosen) == 0 {
		httpx.SendJSONError(w, "Nothing to change: send backupsStorageId and/or templatesStorageId", http.StatusBadRequest)
		return
	}
	for _, wr := range writes {
		id, ok := chosen[wr.field]
		if !ok {
			continue
		}
		if err := wr.set(userID, id); err != nil {
			log.Printf("storage prefs %s for %s: %v", wr.field, userID, err)
			httpx.SendJSONError(w, "Failed to save your storage choice", http.StatusInternalServerError)
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
