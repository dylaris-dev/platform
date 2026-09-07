package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"

	"dylaris-core/store"
)

// userID pulls the authenticated caller id (set by AuthMiddleware).
func solderCaller(r *http.Request) string {
	uid, _ := r.Context().Value("userID").(string)
	return uid
}

// --- Solder clients (per-owner) ---

// ListClients GET /api/solder/clients - the Technic launcher clients the
// caller has registered.
func (h *SolderHandler) ListClients(w http.ResponseWriter, r *http.Request) {
	clients, err := h.state.Store.ListSolderClientsByOwner(solderCaller(r))
	if err != nil {
		sendJSONError(w, "Failed to list clients", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(clients)
}

// CreateClient POST /api/solder/clients - registers a Technic launcher client
// under the caller.
func (h *SolderHandler) CreateClient(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	c, err := h.state.Store.CreateSolderClient(req.Name, solderCaller(r))
	if err != nil {
		log.Printf("solder CreateClient: %v", err)
		sendJSONError(w, "Failed to create client", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "client": c})
}

// DeleteClient DELETE /api/solder/clients/{id} - removes one of the caller's
// clients; the delete is owner-scoped.
func (h *SolderHandler) DeleteClient(w http.ResponseWriter, r *http.Request) {
	id := atoiVar(r, "id")
	if err := h.state.Store.DeleteSolderClient(id, solderCaller(r)); err != nil {
		sendJSONError(w, "Failed to delete client", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// --- Pack-client whitelist (per-pack, both pack and client must belong to caller) ---

// ownsPackAndClient loads the pack and client scoped to the caller; writes a 404
// and returns false if either is missing/foreign.
func (h *SolderHandler) ownsPackAndClient(w http.ResponseWriter, r *http.Request, packID, clientID int) bool {
	uid := solderCaller(r)
	pack, err := h.state.Store.GetPack(packID)
	if err != nil || pack == nil || pack.OwnerID != uid {
		sendJSONError(w, "Pack not found", http.StatusNotFound)
		return false
	}
	client, err := h.state.Store.GetSolderClient(clientID, uid)
	if err != nil || client == nil {
		sendJSONError(w, "Client not found", http.StatusNotFound)
		return false
	}
	return true
}

// ListPackClientsHandler GET /api/packs/{id}/clients - the Technic clients
// whitelisted for one pack. A pack the caller does not own is 404.
func (h *SolderHandler) ListPackClientsHandler(w http.ResponseWriter, r *http.Request) {
	packID := atoiVar(r, "id")
	uid := solderCaller(r)
	pack, err := h.state.Store.GetPack(packID)
	if err != nil || pack == nil || pack.OwnerID != uid {
		sendJSONError(w, "Pack not found", http.StatusNotFound)
		return
	}
	clients, err := h.state.Store.ListPackClients(packID)
	if err != nil {
		sendJSONError(w, "Failed to list whitelist", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(clients)
}

// AddPackClient POST /api/packs/{id}/clients/{clientId} - whitelists one
// client for a private pack. Both the pack and the client must belong to the
// caller.
func (h *SolderHandler) AddPackClient(w http.ResponseWriter, r *http.Request) {
	packID, clientID := atoiVar(r, "id"), atoiVar(r, "clientId")
	if !h.ownsPackAndClient(w, r, packID, clientID) {
		return
	}
	if err := h.state.Store.AddPackClient(packID, clientID); err != nil {
		sendJSONError(w, "Failed to add to whitelist", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// RemovePackClient DELETE /api/packs/{id}/clients/{clientId} - drops a client
// from a pack's whitelist.
func (h *SolderHandler) RemovePackClient(w http.ResponseWriter, r *http.Request) {
	packID, clientID := atoiVar(r, "id"), atoiVar(r, "clientId")
	if !h.ownsPackAndClient(w, r, packID, clientID) {
		return
	}
	if err := h.state.Store.RemovePackClient(packID, clientID); err != nil {
		sendJSONError(w, "Failed to remove from whitelist", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// --- Solder keys (per-owner; plaintext shown once) ---

// ListKeys GET /api/solder/keys - the caller's Solder API keys, hashes only.
func (h *SolderHandler) ListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.state.Store.ListSolderKeysByOwner(solderCaller(r))
	if err != nil {
		sendJSONError(w, "Failed to list keys", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(keys)
}

// CreateKey POST /api/solder/keys - mints a 64-character Solder API key. Only
// its hash is stored, so the plaintext is shown once and cannot be recovered.
// solderPastedKeyRe bounds a key an operator supplies. The Technic Platform's is
// 32 hex characters today, but pinning that shape would break the first time it
// changes and this value is never parsed - only hashed and compared. So the rule
// is only what a key has to be to work: printable ASCII, no whitespace, long
// enough not to be guessed.
var solderPastedKeyRe = regexp.MustCompile(`^[!-~]{16,128}$`)

// CreateKey POST /api/solder/keys - registers a Solder API key.
//
// Two ways in, and the second is the one that links this Solder to the Technic
// Platform at all. Technic ISSUES the key: it lives on the operator's Technic
// profile under Solder Configuration, and Solder is expected to accept it, after
// which "Link Solder" makes technicpack.net call GET /solder/api/verify/{key}
// against this install. Minting our own could never satisfy that - Technic never
// learns a key we invented - so an install that could only generate one was
// unlinkable, and said so with a bare 403 from a URL that was otherwise correct.
// MEASURED: technicpack.net asked this install for a 32-hex key that had never
// been created here, and got 403 because nothing could ever have created it.
//
// A generated key is still useful and stays the default: it is the ?k= a
// launcher carries to see an owner's private packs, which has nothing to do with
// the Platform.
func (h *SolderHandler) CreateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		// Key is the value from the Technic Platform. Empty means "mint one".
		Key string `json:"key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	plaintext := strings.TrimSpace(req.Key)
	supplied := plaintext != ""
	if supplied {
		if !solderPastedKeyRe.MatchString(plaintext) {
			sendJSONError(w, "That does not look like a Solder API key: 16 to 128 characters, no spaces. Copy it from your Technic profile under Solder Configuration.", http.StatusBadRequest)
			return
		}
	} else {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			sendJSONError(w, "Failed to generate key", http.StatusInternalServerError)
			return
		}
		plaintext = hex.EncodeToString(buf) // 64 chars, shown once
	}

	k, err := h.state.Store.CreateSolderKey(req.Name, solderCaller(r), solderKeyHash(plaintext))
	if errors.Is(err, store.ErrNameTaken) {
		sendJSONError(w, "That key is already registered here.", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("solder CreateKey: %v", err)
		sendJSONError(w, "Failed to create key", http.StatusInternalServerError)
		return
	}
	// A key we generated is shown once, because this is the only moment it
	// exists in the clear. A key the operator pasted is not echoed: they already
	// have it, and sending it back would put it in one more place for no gain.
	out := map[string]interface{}{"success": true, "key": k}
	if !supplied {
		out["plaintext"] = plaintext
	}
	json.NewEncoder(w).Encode(out)
}

// DeleteKey DELETE /api/solder/keys/{id} - revokes one of the caller's Solder
// keys.
func (h *SolderHandler) DeleteKey(w http.ResponseWriter, r *http.Request) {
	id := atoiVar(r, "id")
	if err := h.state.Store.DeleteSolderKey(id, solderCaller(r)); err != nil {
		sendJSONError(w, "Failed to delete key", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// --- Solder handle (the account's own Solder address) ---

// solderHandleRe is the same shape as a pack slug: it goes in a URL a launcher
// stores, so it has to be lowercase, unambiguous and free of anything that has
// to be escaped.
var solderHandleRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9_-]{1,62}[a-z0-9])?$`)

// solderAddress renders the URL an operator pastes into the Technic Platform.
// Built from core_public_url, the same setting the Solder mirror is served
// from, so it cannot drift from the address launchers are actually given.
// Empty when that setting is unset, which the panel says rather than printing a
// URL with a missing host.
func (h *SolderHandler) solderAddress(handle string) string {
	if handle == "" {
		return ""
	}
	base, _ := h.state.Store.GetSetting("core_public_url")
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	return base + "/solder/u/" + handle + "/api/"
}

// GetHandle GET /api/solder/handle - the caller's Solder address, or the empty
// string when they have not claimed one.
func (h *SolderHandler) GetHandle(w http.ResponseWriter, r *http.Request) {
	handle, err := h.state.Store.GetSolderHandle(solderCaller(r))
	if err != nil {
		sendJSONError(w, "Failed to read the Solder handle", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"handle":  handle,
		"url":     h.solderAddress(handle),
	})
}

// SetHandle POST /api/solder/handle - claims the caller's Solder address, once.
//
// Once, because the Technic Platform stores the Solder URL per modpack and a
// launcher keeps it inside the installed pack. A rename would break every linked
// modpack and every install of it, silently, at a moment unrelated to the
// change - so it is refused rather than warned about.
func (h *SolderHandler) SetHandle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handle string `json:"handle"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	handle := strings.ToLower(strings.TrimSpace(req.Handle))
	if !solderHandleRe.MatchString(handle) {
		sendJSONError(w, "A Solder handle is 3 to 64 characters: lowercase letters, digits, dashes and underscores, starting and ending with a letter or digit.", http.StatusBadRequest)
		return
	}
	err := h.state.Store.SetSolderHandle(solderCaller(r), handle)
	if errors.Is(err, store.ErrSolderHandleSet) {
		sendJSONError(w, "Your Solder address is already set and cannot be changed: the Technic Platform stores it per modpack, and every installed copy would stop updating.", http.StatusConflict)
		return
	}
	if errors.Is(err, store.ErrNameTaken) {
		sendJSONError(w, "That Solder handle is taken.", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("solder SetHandle: %v", err)
		sendJSONError(w, "Failed to set the Solder handle", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"handle":  handle,
		"url":     h.solderAddress(handle),
	})
}
