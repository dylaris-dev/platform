package handlers

import (
	"dylaris-pkg/validate"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// SetupHandler covers /api/setup/*. Two open routes:
//
//	GET  /api/setup/status - what mode are we in?
//	POST /api/setup/admin  - create the first admin
//
// Mode is inferred per-request from the DB:
//   - fresh_install: user_count = 0 (no users at all)
//   - lost_admin:    admin_count = 0 AND user_count >= 1
//   - complete:      admin_count >= 1
type SetupHandler struct {
	state *AppState
	auth  *AuthHandler // for JWT issuance after successful setup
}

func NewSetupHandler(state *AppState, auth *AuthHandler) *SetupHandler {
	return &SetupHandler{state: state, auth: auth}
}

// setupGate builds the gate from live counts plus config. Both /setup endpoints
// go through here so the page and the handler can never disagree about whether
// the door is open.
func (h *SetupHandler) setupGate(userCount, adminCount int) setupGate {
	return setupGate{
		UserCount:        userCount,
		AdminCount:       adminCount,
		SetupEnabled:     h.state.SetupEnabled,
		SecretConfigured: h.state.AdminSecretConfigured(),
	}
}

type setupStatusResp struct {
	Success               bool   `json:"success"`
	Mode                  string `json:"mode"`
	AdminSecretConfigured bool   `json:"adminSecretConfigured"`
	FrontendURL           string `json:"frontendUrl,omitempty"`
	// SetupEnabled is env SETUP as configured. Reported separately from Open so
	// the panel can say WHY the wizard is closed rather than only that it is.
	SetupEnabled bool `json:"setupEnabled"`
	// Open is the single answer the panel renders from: is there a working form
	// here. Computed by the same gate the create endpoint uses.
	Open bool `json:"open"`
	// NeedsSecretWarning asks the panel for the red "no admin token configured"
	// banner. See setupGate.NeedsSecretWarning.
	NeedsSecretWarning bool `json:"needsSecretWarning"`
}

// Status GET /api/setup/status - open route. Always reachable (the setup-lock
// middleware passes /api/setup/* through unchanged). adminSecretConfigured lets
// the panel pick its wizard UI without exposing the secret itself.
func (h *SetupHandler) Status(w http.ResponseWriter, r *http.Request) {
	adminCount, _ := h.state.Store.CountAdmins()
	userCount, _ := h.state.Store.CountUsers()
	gate := h.setupGate(userCount, adminCount)
	out := setupStatusResp{
		Success:               true,
		FrontendURL:           h.state.FrontendURL,
		AdminSecretConfigured: h.state.AdminSecretConfigured(),
		SetupEnabled:          h.state.SetupEnabled,
		Open:                  gate.Open(),
		NeedsSecretWarning:    gate.NeedsSecretWarning(),
	}
	switch {
	case adminCount > 0:
		out.Mode = "complete"
	case userCount == 0:
		out.Mode = "fresh_install"
	default:
		out.Mode = "lost_admin"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type setupTOTPInfo struct {
	Secret string `json:"secret"`
	Code   string `json:"code"`
}

type setupAdminReq struct {
	Username    string         `json:"username"`
	Password    string         `json:"password"`
	AdminSecret string         `json:"adminSecret,omitempty"`
	TOTP        *setupTOTPInfo `json:"totp,omitempty"`
}

type setupAdminResp struct {
	Success bool        `json:"success"`
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
	User    interface{} `json:"user,omitempty"`
	Token   string      `json:"token,omitempty"`
}

// CreateAdmin POST /api/setup/admin - open route (rate-limited). adminCreateAllowed
// enforces the ADMIN_SECRET rule BEFORE any username/password work so an
// unauthorized caller learns nothing about validation. The guarded CTE in
// CreateFirstAdmin still protects against racing first-admin inserts across
// N Cores; CreateAdditionalAdmin serves the break-glass path.
func (h *SetupHandler) CreateAdmin(w http.ResponseWriter, r *http.Request) {
	var req setupAdminReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendSetupError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON.")
		return
	}

	// Fail closed: if we cannot determine the setup state, do NOT fall through to
	// the authorization gate (a mis-read count could treat lost_admin as a fresh
	// install and reopen secret-less admin creation).
	userCount, err := h.state.Store.CountUsers()
	if err != nil {
		sendSetupError(w, http.StatusInternalServerError, "count_failed", "Could not determine setup state.")
		return
	}
	adminCount, err := h.state.Store.CountAdmins()
	if err != nil {
		sendSetupError(w, http.StatusInternalServerError, "count_failed", "Could not determine setup state.")
		return
	}

	gate := h.setupGate(userCount, adminCount)
	if !adminCreateAllowed(gate, h.state.AdminSecret, req.AdminSecret) {
		switch {
		case !gate.Open():
			// Named separately from the secret failure on purpose: the operator
			// who switched SETUP off needs to be told that, not sent hunting for
			// a wrong secret. It reveals nothing an attacker can use - that this
			// instance has an admin is already public from the login page.
			sendSetupError(w, http.StatusForbidden, "setup_disabled", "Setup is switched off on this instance. Set SETUP=true in Core's environment and restart to create another admin.")
		case h.state.AdminSecretConfigured():
			sendSetupError(w, http.StatusForbidden, "invalid_admin_secret", "The admin secret is missing or incorrect.")
		default:
			sendSetupError(w, http.StatusForbidden, "admin_recovery_disabled", "Admin creation is closed. Set ADMIN_SECRET in Core's environment and restart to create a new admin.")
		}
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	if !validate.IsUsername(req.Username) {
		sendSetupError(w, http.StatusBadRequest, "invalid_username", "Username must be 3-32 chars (alphanumeric, _ or -).")
		return
	}
	if len(req.Password) < 8 {
		sendSetupError(w, http.StatusBadRequest, "invalid_password", "Password must be at least 8 characters.")
		return
	}
	if req.TOTP != nil {
		if req.TOTP.Secret == "" || req.TOTP.Code == "" {
			sendSetupError(w, http.StatusBadRequest, "invalid_totp", "TOTP secret + code required.")
			return
		}
		if !totp.Validate(req.TOTP.Code, req.TOTP.Secret) {
			sendSetupError(w, http.StatusBadRequest, "invalid_totp", "TOTP code did not verify.")
			return
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		sendSetupError(w, http.StatusInternalServerError, "hash_failed", "Could not hash password.")
		return
	}
	totpSecret := ""
	if req.TOTP != nil {
		totpSecret = req.TOTP.Secret
	}

	var user *models.User
	if adminCount == 0 {
		// Guarded CTE: race-safe first admin (Fresh-Install or Lost-Admin).
		user, err = h.state.Store.CreateFirstAdmin(req.Username, string(hash), totpSecret)
	} else {
		// Only reachable with a matching secret (break-glass additional admin).
		user, err = h.state.Store.CreateAdditionalAdmin(req.Username, string(hash), totpSecret)
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrSetupAlreadyComplete):
			sendSetupError(w, http.StatusConflict, "setup_already_complete", "Setup is already complete. Go to /login.")
		case errors.Is(err, store.ErrUsernameTaken):
			sendSetupError(w, http.StatusConflict, "username_taken", "That username is already taken. Choose another.")
		default:
			sendSetupError(w, http.StatusInternalServerError, "create_failed", err.Error())
		}
		return
	}

	token, err := h.auth.IssueToken(user.Username, user.IsAdmin, sessionKey(user))
	if err != nil {
		sendSetupError(w, http.StatusInternalServerError, "token_failed", "Admin created but token issuance failed: "+err.Error())
		return
	}

	// Sign them in, the same way a login does. This endpoint issues a session
	// and every place that issues one has to install the cookie: the token in
	// the body below is what the panel used to keep in localStorage, and with
	// that gone it is read by nothing. Without this the wizard finishes, sends
	// the new operator to /servers, and the authed layout bounces them to /login
	// to type the password they chose ten seconds ago.
	setSessionCookie(w, r, token, int(sessionTTL.Seconds()))

	// SSE so other Cores' setup-lock middleware unlocks instantly + panels
	// re-check setup status.
	if h.state.Events != nil {
		h.state.Events.Publish(r.Context(), "setup.completed", nil)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(setupAdminResp{
		Success: true, User: user, Token: token,
	})
}

func sendSetupError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(setupAdminResp{Success: false, Error: code, Message: msg})
}
