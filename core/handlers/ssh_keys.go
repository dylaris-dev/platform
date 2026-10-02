package handlers

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/gorilla/mux"
	"golang.org/x/crypto/ssh"
)

// maxSSHKeysPerUser bounds what the SFTP sync republishes for one account every
// minute.
const maxSSHKeysPerUser = 10

// Identity audit events for the keys an account signs in to SFTP with.
const (
	AuditEventSSHKeyAdded   = "ssh_key_added"
	AuditEventSSHKeyRemoved = "ssh_key_removed"
)

// SSHKeysHandler serves /me/ssh-keys: the public keys the caller signs in to
// SFTP with. Always the caller's own; there is no route that names another
// account.
type SSHKeysHandler struct{ state *AppState }

func NewSSHKeysHandler(state *AppState) *SSHKeysHandler { return &SSHKeysHandler{state: state} }

// List GET /me/ssh-keys
func (h *SSHKeysHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	keys, err := h.state.Store.ListSSHKeysByUser(userID)
	if err != nil {
		sendJSONError(w, "Failed to load SSH keys", http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "keys": keys})
}

type addSSHKeyRequest struct {
	Name      string `json:"name"`
	PublicKey string `json:"publicKey"`
	// A key opens every file the account can reach, without the second
	// factor, so adding one asks for the password and the code again - the
	// same rule as minting an API key.
	reauthFields
}

// Create POST /me/ssh-keys
func (h *SSHKeysHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var req addSSHKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 128 {
		sendJSONError(w, "Give the key a name of up to 128 characters", http.StatusBadRequest)
		return
	}
	line, fp, err := normalizeSSHPublicKey(req.PublicKey)
	if err != nil {
		sendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if rerr := requireReauth(h.state, userID, req.Password, req.Code); rerr != nil {
		writeReauthError(w, rerr)
		return
	}
	added, err := h.state.Store.AddSSHKey(&models.SSHKey{UserID: userID, Name: req.Name, PublicKey: line, Fingerprint: fp}, maxSSHKeysPerUser)
	switch {
	case errors.Is(err, store.ErrSSHKeyExists):
		sendJSONError(w, "This key is already on your account", http.StatusConflict)
		return
	case err != nil:
		log.Printf("ssh keys: add for %s: %v", userID, err)
		sendJSONError(w, "Failed to add the SSH key", http.StatusInternalServerError)
		return
	case !added:
		sendJSONError(w, "An account can hold up to "+strconv.Itoa(maxSSHKeysPerUser)+" SSH keys. Remove one first.", http.StatusConflict)
		return
	}
	LogIdentityAudit(h.state, r, AuditEventSSHKeyAdded, userID, userID, map[string]interface{}{"name": req.Name, "fingerprint": fp})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "fingerprint": fp,
		"note": "SFTP accepts the key within a minute."})
}

// Delete DELETE /me/ssh-keys/{id}
func (h *SSHKeysHandler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	id, err := strconv.Atoi(mux.Vars(r)["id"])
	if err != nil {
		sendJSONError(w, "Invalid key ID", http.StatusBadRequest)
		return
	}
	found, err := h.state.Store.DeleteSSHKey(userID, id)
	if err != nil {
		sendJSONError(w, "Failed to remove the SSH key", http.StatusInternalServerError)
		return
	}
	if !found {
		sendJSONError(w, "Key not found", http.StatusNotFound)
		return
	}
	LogIdentityAudit(h.state, r, AuditEventSSHKeyRemoved, userID, userID, map[string]interface{}{"id": id})
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// normalizeSSHPublicKey parses one authorized_keys line and returns it as
// "<type> <base64>" - the comment and any options dropped - with its SHA256
// fingerprint. Only key types that are not weak are taken.
func normalizeSSHPublicKey(raw string) (line, fingerprint string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n") {
		return "", "", errors.New("Paste one public key, on one line (the contents of a .pub file)")
	}
	pk, _, opts, _, perr := ssh.ParseAuthorizedKey([]byte(raw))
	if perr != nil {
		return "", "", errors.New("That is not an SSH public key. Paste the contents of the .pub file, not the private key")
	}
	if len(opts) > 0 {
		return "", "", errors.New("Remove the options in front of the key; paste only the key itself")
	}
	switch pk.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoSKED25519,
		ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoSKECDSA256:
	case ssh.KeyAlgoRSA:
		if bits := rsaKeyBits(pk); bits < 3072 {
			return "", "", errors.New("RSA keys need at least 3072 bits. An Ed25519 key is the better choice: ssh-keygen -t ed25519")
		}
	default:
		return "", "", errors.New("Use an Ed25519, ECDSA or RSA (3072+ bit) key")
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))), ssh.FingerprintSHA256(pk), nil
}

// rsaKeyBits is the modulus size of an RSA public key, 0 when it is not one.
func rsaKeyBits(pk ssh.PublicKey) int {
	cpk, ok := pk.(ssh.CryptoPublicKey)
	if !ok {
		return 0
	}
	type sizer interface{ Size() int }
	if k, ok := cpk.CryptoPublicKey().(sizer); ok {
		return k.Size() * 8
	}
	return 0
}

// revokeAllSSHKeys removes every SFTP key of an account, on the same recovery
// paths as revokeAllAPIKeys and for the same reason: a key added while the
// account was in someone else's hands is a way back in no password reset sees.
func revokeAllSSHKeys(state *AppState, userID, why string) int {
	if state == nil || state.Store == nil {
		return 0
	}
	n, err := state.Store.DeleteAllSSHKeys(userID)
	if err != nil {
		log.Printf("%s: remove SSH keys of %s: %v", why, userID, err)
	}
	return n
}
