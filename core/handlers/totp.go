package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"dylaris-core/models"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// totpIssuer appears in the user's Authenticator app entry.
const totpIssuer = "Dylaris"

const (
	// totpStepSeconds and totpSkewSteps mirror what totp.Validate does by
	// default: 30-second steps, plus or minus one for clock drift.
	totpStepSeconds = 30
	totpSkewSteps   = 1
	// totpReplayTTL is how long a spent step is remembered. It only has to
	// outlive the window in which the code is still acceptable, which is the
	// skew on both sides of the step; the rest is margin.
	totpReplayTTL = 3 * totpStepSeconds * time.Second
)

// matchTOTPStep reports which time step a code belongs to, or ok=false for
// none. totp.Validate answers only yes or no, and "which step" is what makes a
// code claimable exactly once.
func matchTOTPStep(code, secret string, now time.Time) (step int64, ok bool) {
	opts := totp.ValidateOpts{Period: totpStepSeconds, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
	for off := -totpSkewSteps; off <= totpSkewSteps; off++ {
		t := now.Add(time.Duration(off) * totpStepSeconds * time.Second)
		valid, err := totp.ValidateCustom(code, secret, t, opts)
		if err == nil && valid {
			return t.Unix() / totpStepSeconds, true
		}
	}
	return 0, false
}

// claimTOTPStep takes one time step for one account and reports whether it was
// still free. A step yields exactly one code, so claiming the step spends the
// code.
//
// A time-based one-time password is one-time or it is not a one-time password.
// This check did not exist: measured on production, the same six digits logged
// the same account in repeatedly, and the neighbouring steps worked too, so a
// code seen once was good for roughly 90 seconds and for as many sessions as
// the holder wanted. The backup codes two branches down have always been
// destructive on use, and RFC 6238 asks for the same of these - the password is
// still required either way, which is exactly why the second factor has to be
// the part that cannot be replayed.
//
// A legitimate user who needs a code twice inside one step waits for the next
// one, which their authenticator already shows.
//
// FAILS OPEN when Redis cannot answer, and logs it. Redis being down means Core
// is degraded in ways this is the least of; refusing every second factor would
// lock out every account including the operator's, to stop a replay that needs
// the password as well.
func claimTOTPStep(state *AppState, userID string, step int64) bool {
	if state == nil || state.Redis == nil {
		return true
	}
	key := fmt.Sprintf("dylaris:totp:used:%s:%d", userID, step)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	free, err := state.Redis.SetNX(ctx, key, "1", totpReplayTTL).Result()
	if err != nil {
		log.Printf("totp: could not claim the one-time step for user %s, accepting the code without replay protection: %v", userID, err)
		return true
	}
	return free
}

// backupCodeCount is the number of single-use codes generated when 2FA is set up.
const backupCodeCount = 10

// SetupTOTPRequest is the empty body of /auth/2fa/setup — auth is via JWT.
type SetupTOTPResponse struct {
	Success bool   `json:"success"`
	Secret  string `json:"secret"`     // base32, raw — the user types/scans this
	OTPAuth string `json:"otpAuthURL"` // otpauth://... — frontend renders as QR
	Issuer  string `json:"issuer"`
	Account string `json:"account"`
}

// SetupTOTPHandler — POST /api/auth/2fa/setup
// Generates a fresh TOTP secret + otpauth URI for the authenticated user.
// The secret is NOT yet persisted — frontend must call /verify with a code
// derived from the same secret to confirm possession.
func (h *AuthHandler) SetupTOTPHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}
	username, _ := r.Context().Value("username").(string)
	user, err := h.state.Store.GetUserByUsername(username)
	if err != nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}

	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: user.Username,
	})
	if err != nil {
		sendJSONError(w, "Failed to generate secret", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(SetupTOTPResponse{
		Success: true,
		Secret:  key.Secret(),
		OTPAuth: key.URL(),
		Issuer:  totpIssuer,
		Account: user.Username,
	})
}

type VerifyTOTPRequest struct {
	Secret string `json:"secret"`
	Code   string `json:"code"`
	// Password re-authenticates the account holder before 2FA is switched on.
	// Disable and RegenerateBackupCodes have always asked for it; enrolment did
	// not, which was the wrong way round - see VerifyTOTPHandler.
	Password string `json:"password"`
}

type VerifyTOTPResponse struct {
	Success     bool     `json:"success"`
	BackupCodes []string `json:"backupCodes"` // shown ONCE, never returned again
	Message     string   `json:"message,omitempty"`
}

// VerifyTOTPHandler — POST /api/auth/2fa/verify
// Validates the user's 6-digit code against the freshly-generated secret.
// On success: persists secret, generates + hashes backup codes, enables 2FA.
// Backup codes are returned in cleartext exactly once — never retrievable later.
func (h *AuthHandler) VerifyTOTPHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}
	username, _ := r.Context().Value("username").(string)
	user, err := h.state.Store.GetUserByUsername(username)
	if err != nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}

	var req VerifyTOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	req.Code = strings.TrimSpace(req.Code)
	req.Secret = strings.TrimSpace(req.Secret)

	if req.Secret == "" || req.Code == "" {
		sendJSONError(w, "Secret and code required", http.StatusBadRequest)
		return
	}

	// Re-authenticate before enabling. Without this, a session token alone was
	// enough to enrol 2FA: the caller supplies the secret, so a stolen or
	// borrowed session could bind the ATTACKER's authenticator to the account,
	// collect the one-time backup codes, and lock the real owner out - turning a
	// temporary session compromise into durable account takeover, using the
	// security feature as the lock.
	//
	// The sibling operations already worked this way (DisableTOTPHandler,
	// RegenerateBackupCodesHandler both compare the password first); enrolment
	// was the one that did not, which is backwards - it is the step that decides
	// who owns the second factor from then on.
	// Exception: a 2fa_setup token. AuthMiddleware only ever sees one because
	// the login handler minted it moments earlier for THIS user after checking
	// the password, it expires in 15 minutes and it reaches only three paths. The
	// password is already proven for its bearer, and asking again would block the
	// forced-enrolment flow behind a prompt the user just answered.
	if TokenPurpose(r) != "2fa_setup" {
		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
			sendJSONError(w, "Invalid password", http.StatusUnauthorized)
			return
		}
	}

	if !totp.Validate(req.Code, req.Secret) {
		// Say WHY in the log. "Invalid code" alone left clock skew and a stale
		// authenticator entry indistinguishable, and both are common enough that
		// diagnosing them meant guessing. The response stays generic; only the
		// operator's log gets the detail, and it never contains the secret or the
		// code.
		log.Printf("2fa setup: rejected code for user %s: %s", user.Username, diagnoseRejectedTOTP(req.Secret, req.Code, time.Now()))
		sendJSONError(w, "Invalid code", http.StatusUnauthorized)
		return
	}

	// Generate 10 single-use backup codes (16 hex chars each).
	plainCodes := make([]string, backupCodeCount)
	hashed := make([]string, backupCodeCount)
	for i := 0; i < backupCodeCount; i++ {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			sendJSONError(w, "Failed to generate backup codes", http.StatusInternalServerError)
			return
		}
		plainCodes[i] = hex.EncodeToString(b)
		// bcrypt cost 10 — light enough for 10 hashes here, strong enough
		// since the codes already have ~64 bits of entropy.
		h, err := bcrypt.GenerateFromPassword([]byte(plainCodes[i]), 10)
		if err != nil {
			sendJSONError(w, "Failed to hash backup code", http.StatusInternalServerError)
			return
		}
		hashed[i] = string(h)
	}
	hashedJSON, _ := json.Marshal(hashed)

	if err := h.state.Store.SetUserTOTP(user.ID, req.Secret, string(hashedJSON), true); err != nil {
		sendJSONError(w, "Failed to enable 2FA", http.StatusInternalServerError)
		return
	}

	LogIdentityAudit(h.state, r, AuditEvent2FASetupCompleted, user.ID, user.ID, map[string]interface{}{
		"backup_codes_generated": backupCodeCount,
	})

	json.NewEncoder(w).Encode(VerifyTOTPResponse{
		Success:     true,
		BackupCodes: plainCodes,
	})
}

// RegenerateBackupCodesHandler — POST /api/auth/2fa/regenerate-backup-codes
// Issues a fresh set of 10 backup codes for a user who has 2FA already enabled.
// Same defence-in-depth as DisableTOTP: requires current password + a valid
// TOTP/backup code. Returns the new codes in cleartext exactly once.
func (h *AuthHandler) RegenerateBackupCodesHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}
	username, _ := r.Context().Value("username").(string)
	user, err := h.state.Store.GetUserByUsername(username)
	if err != nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	if !user.Is2FAEnabled {
		sendJSONError(w, "2FA is not enabled — set it up first", http.StatusBadRequest)
		return
	}

	var req DisableTOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		sendJSONError(w, "Invalid password", http.StatusUnauthorized)
		return
	}
	ok, err := h.verifyTOTPOrBackup(user, req.Code)
	if err != nil {
		sendJSONError(w, "Verification failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		sendJSONError(w, "Invalid code", http.StatusUnauthorized)
		return
	}

	plainCodes := make([]string, backupCodeCount)
	hashed := make([]string, backupCodeCount)
	for i := 0; i < backupCodeCount; i++ {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			sendJSONError(w, "Failed to generate backup codes", http.StatusInternalServerError)
			return
		}
		plainCodes[i] = hex.EncodeToString(b)
		bcryptHash, err := bcrypt.GenerateFromPassword([]byte(plainCodes[i]), 10)
		if err != nil {
			sendJSONError(w, "Failed to hash backup code", http.StatusInternalServerError)
			return
		}
		hashed[i] = string(bcryptHash)
	}
	hashedJSON, _ := json.Marshal(hashed)
	if err := h.state.Store.SetUserTOTP(user.ID, user.TOTPSecret, string(hashedJSON), true); err != nil {
		sendJSONError(w, "Failed to persist new codes", http.StatusInternalServerError)
		return
	}

	LogIdentityAudit(h.state, r, AuditEvent2FABackupRegenerated, user.ID, user.ID, map[string]interface{}{
		"count": backupCodeCount,
	})

	json.NewEncoder(w).Encode(VerifyTOTPResponse{
		Success:     true,
		BackupCodes: plainCodes,
	})
}

// Get2FAStatusHandler — GET /api/auth/2fa/status
// Returns whether 2FA is enabled and the count of unconsumed backup codes.
// Never returns the codes themselves — only the count, so users can tell
// when they're running low (single-digit) and regenerate proactively.
func (h *AuthHandler) Get2FAStatusHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}
	username, _ := r.Context().Value("username").(string)
	user, err := h.state.Store.GetUserByUsername(username)
	if err != nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}

	remaining := 0
	if user.TOTPBackupCodes != "" && user.TOTPBackupCodes != "[]" {
		var hashed []string
		if err := json.Unmarshal([]byte(user.TOTPBackupCodes), &hashed); err == nil {
			remaining = len(hashed)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":              true,
		"enabled":              user.Is2FAEnabled,
		"remainingBackupCodes": remaining,
	})
}

type DisableTOTPRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"` // current TOTP or backup code (defence in depth)
}

// DisableTOTPHandler — POST /api/auth/2fa/disable
// User-initiated 2FA disable. Requires both the current password and a valid
// TOTP/backup code to mitigate session-hijack risk.
func (h *AuthHandler) DisableTOTPHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}
	username, _ := r.Context().Value("username").(string)
	user, err := h.state.Store.GetUserByUsername(username)
	if err != nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	if !user.Is2FAEnabled {
		sendJSONError(w, "2FA is not enabled", http.StatusBadRequest)
		return
	}

	var req DisableTOTPRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(req.Password)); err != nil {
		sendJSONError(w, "Invalid password", http.StatusUnauthorized)
		return
	}
	ok, err := h.verifyTOTPOrBackup(user, req.Code)
	if err != nil {
		sendJSONError(w, "Verification failed", http.StatusInternalServerError)
		return
	}
	if !ok {
		sendJSONError(w, "Invalid code", http.StatusUnauthorized)
		return
	}

	if err := h.state.Store.DisableUserTOTP(user.ID); err != nil {
		sendJSONError(w, "Failed to disable 2FA", http.StatusInternalServerError)
		return
	}

	LogIdentityAudit(h.state, r, AuditEvent2FADisabled, user.ID, user.ID, nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// AdminResetTOTPHandler — DELETE /api/users/{id}/2fa  (admin-only)
// Wipes a user's 2FA without their password — for the case when the user
// has lost both their authenticator AND their backup codes. After this
// the user logs in with just their password and can re-enable 2FA.
func (h *AuthHandler) AdminResetTOTPHandler(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", http.StatusServiceUnavailable)
		return
	}
	id, ok := parseUserID(w, r)
	if !ok {
		return
	}
	target, terr := h.state.Store.GetUserByID(id)
	if terr != nil || target == nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageAccount(h.state, r, target) {
		sendJSONError(w, "You cannot reset 2FA on an account with more rights than yours", http.StatusForbidden)
		return
	}
	// Taking somebody's second factor away leaves their password as the whole
	// credential, and the administrator's own session is not proof that the
	// administrator asked for it. A DELETE carrying a body is unusual and
	// deliberate: the alternative was a second method for the same action.
	var req adminReauthRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // an absent body fails the check below, with its own message
	}
	if !requireAdminReauth(w, r, h.state, req) {
		return
	}
	if err := h.state.Store.DisableUserTOTP(id); err != nil {
		sendJSONError(w, "Reset failed", http.StatusInternalServerError)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	LogIdentityAudit(h.state, r, AuditEvent2FAAdminReset, actorID, id, nil)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// verifyTOTPOrBackup checks `code` against the user's TOTP secret first
// (most common path for normal logins), then falls back to matching against
// any unconsumed backup code. A successful backup-code match is destructive:
// the matched code is removed and the updated list is persisted, ensuring
// it can't be reused.
func (h *AuthHandler) verifyTOTPOrBackup(user *models.User, code string) (bool, error) {
	return verifyTOTPOrBackupFor(h.state, user, code)
}

// verifyTOTPOrBackupFor is the same check without an AuthHandler, so the
// re-authentication helper can reach it from handlers that have only the
// AppState. Splitting it was the alternative to plumbing an AuthHandler into
// two more structs for one call each.
func verifyTOTPOrBackupFor(state *AppState, user *models.User, code string) (bool, error) {
	return verifyTOTPOrBackupWith(state, user, code, true)
}

// verifyTOTPOrBackupWith is the same check with the one-time claim as a
// choice. claimStep=false verifies a TOTP code without spending its step; a
// backup code is still consumed either way, since that is the code itself
// being used up rather than a window.
//
// Only re-authentication passes false, and the reason is the attack each path
// faces. At LOGIN a code plus a phished password is the whole credential, so a
// replay is the attack and the step has to be spent. At re-authentication the
// caller has already proved the password over a session they already hold -
// somebody able to replay a code there could simply sign in. What false buys
// is that one prompt can cover the two writes a single save makes: the panel's
// "role and permissions" button calls two endpoints, and the second would
// otherwise be refused as a replay of the code the operator had just typed.
func verifyTOTPOrBackupWith(state *AppState, user *models.User, code string, claimStep bool) (bool, error) {
	code = strings.TrimSpace(code)
	if code == "" || user == nil {
		return false, nil
	}

	// 1) TOTP — fast path for the regular case, and single-use like the backup
	// codes below.
	if user.TOTPSecret != "" {
		if step, ok := matchTOTPStep(code, user.TOTPSecret, time.Now()); ok {
			if !claimStep || claimTOTPStep(state, user.ID, step) {
				return true, nil
			}
			// A code that was already spent is simply not a valid code. Falling
			// through to the backup branch costs one bcrypt per stored code and
			// cannot match, so refuse here.
			return false, nil
		}
	}

	// 2) Backup codes — bcrypt-compare each, drop the matched one on success
	if user.TOTPBackupCodes == "" || user.TOTPBackupCodes == "[]" {
		return false, nil
	}
	var hashed []string
	if err := json.Unmarshal([]byte(user.TOTPBackupCodes), &hashed); err != nil {
		return false, err
	}
	for i, hashedCode := range hashed {
		if bcrypt.CompareHashAndPassword([]byte(hashedCode), []byte(code)) == nil {
			// Match — drop this code and persist the new list so it
			// can't be reused.
			remaining := append(hashed[:i], hashed[i+1:]...)
			out, _ := json.Marshal(remaining)
			if err := state.Store.SetUserTOTP(user.ID, user.TOTPSecret, string(out), user.Is2FAEnabled); err != nil {
				return false, err
			}
			// Audit the consumption so admins can spot recovery-code abuse
			// — multiple consumptions on one account, or from unfamiliar IPs,
			// are a strong signal that codes leaked. The handler caller
			// owns the request so we can't grab it here; we log via the
			// nil-request path which still captures actor/target IDs.
			LogIdentityAudit(state, nil, AuditEvent2FABackupCodeConsumed, user.ID, user.ID, map[string]interface{}{
				"remaining": len(remaining),
			})
			return true, nil
		}
	}
	return false, nil
}
