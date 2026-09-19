package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"dylaris-core/pkg/crypto"
	"dylaris-core/services"
)

// Modrinth PAT management endpoints. The plaintext PAT only ever
// crosses three boundaries:
//   1. POST from the user (set)
//   2. internal calls to Modrinth API (publish, validate)
//   3. never returned in any read response
//
// Storage: AES-256-GCM ciphertext (pkg/crypto), key derived from CLUSTER_SECRET
// + the "modrinth-pat" purpose tag.

type ModrinthPATHandler struct {
	state         *AppState
	clusterSecret string
}

func NewModrinthPATHandler(state *AppState, clusterSecret string) *ModrinthPATHandler {
	return &ModrinthPATHandler{state: state, clusterSecret: clusterSecret}
}

func (h *ModrinthPATHandler) key() []byte {
	return crypto.DeriveKey(h.clusterSecret, "modrinth-pat")
}

type setPATRequest struct {
	Token string `json:"token"`
}

type patStatus struct {
	Success          bool       `json:"success"`
	Connected        bool       `json:"connected"`
	ModrinthUsername string     `json:"modrinthUsername,omitempty"`
	LastValidatedAt  *time.Time `json:"lastValidatedAt,omitempty"`
	Message          string     `json:"message,omitempty"`
}

// Status GET /api/me/modrinth-pat — returns whether the user has a PAT
// configured + (if so) the username + last_validated timestamp. Never
// returns the PAT itself.
func (h *ModrinthPATHandler) Status(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	pat, err := h.state.Store.GetModrinthPAT(userID)
	if err != nil {
		sendJSONError(w, "DB error", http.StatusInternalServerError)
		return
	}
	if pat == nil {
		json.NewEncoder(w).Encode(patStatus{Success: true, Connected: false})
		return
	}
	json.NewEncoder(w).Encode(patStatus{
		Success:          true,
		Connected:        true,
		ModrinthUsername: pat.ModrinthUsername,
		LastValidatedAt:  pat.LastValidatedAt,
	})
}

// Set PUT /api/me/modrinth-pat — accepts the plaintext PAT, validates it
// against Modrinth /v2/user, encrypts, and stores. Plaintext is never
// echoed back.
func (h *ModrinthPATHandler) Set(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req setPATRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Token = strings.TrimSpace(req.Token)
	if req.Token == "" {
		sendJSONError(w, "Token required", http.StatusBadRequest)
		return
	}
	username, err := h.validatePAT(r.Context(), req.Token)
	if err != nil {
		sendJSONError(w, "Token rejected by Modrinth: "+err.Error(), http.StatusBadRequest)
		return
	}
	ciphertext, err := crypto.Encrypt(h.key(), []byte(req.Token))
	if err != nil {
		sendJSONError(w, "Failed to encrypt token", http.StatusInternalServerError)
		return
	}
	if err := h.state.Store.SetModrinthPAT(userID, ciphertext, username); err != nil {
		sendJSONError(w, "Failed to store token", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(patStatus{
		Success:          true,
		Connected:        true,
		ModrinthUsername: username,
		Message:          "Connected as " + username,
	})
}

// Clear DELETE /api/me/modrinth-pat — wipes the PAT entirely.
func (h *ModrinthPATHandler) Clear(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if err := h.state.Store.ClearModrinthPAT(userID); err != nil {
		sendJSONError(w, "Failed to clear PAT", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// validatePAT calls Modrinth /v2/user with the PAT and returns the username
// on success. A 401 means the PAT is bad; anything else is a transient
// upstream issue and we let the user retry.
func (h *ModrinthPATHandler) validatePAT(ctx context.Context, plaintext string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.modrinth.com/v2/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", plaintext)
	req.Header.Set("User-Agent", services.DylarisUserAgent())
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", fmt.Errorf("unauthorized (PAT invalid or scope missing)")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var u struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return "", err
	}
	if u.Username == "" {
		return "", fmt.Errorf("Modrinth returned no username")
	}
	return u.Username, nil
}

// LoadPAT decrypts and returns the plaintext PAT for outgoing publishing
// calls. Exported so the modpacks-publish handler can use it without
// duplicating the crypto wiring.
func (h *ModrinthPATHandler) LoadPAT(userID string) (string, string, error) {
	pat, err := h.state.Store.GetModrinthPAT(userID)
	if err != nil {
		return "", "", err
	}
	if pat == nil {
		return "", "", fmt.Errorf("no Modrinth PAT configured")
	}
	plaintext, err := crypto.Decrypt(h.key(), pat.Ciphertext)
	if err != nil {
		return "", "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), pat.ModrinthUsername, nil
}
