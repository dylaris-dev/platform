package handlers

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dylaris-core/mailer"
	"dylaris-core/services"

	"golang.org/x/crypto/bcrypt"
)

type PasswordResetHandler struct {
	state *AppState
}

func NewPasswordResetHandler(state *AppState) *PasswordResetHandler {
	return &PasswordResetHandler{state: state}
}

type forgotPasswordRequest struct {
	Email string `json:"email"`
}

// ForgotPassword POST /api/auth/forgot-password — public.
// Always returns success regardless of whether the email is on file. This
// is the standard enumeration-safe pattern: legitimate users get a mail,
// attackers get the same "ok" response either way.
//
// Side effect when the email IS on file: a fresh single-use token replaces
// any existing one (so two pending resets can't both succeed) and an email
// goes out with the reset link.
func (h *PasswordResetHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	// Cheap shape check — full validation is on the SMTP side anyway.
	if !emailRegex.MatchString(email) {
		// Same silent-success: do not leak that the address was malformed
		// because that distinguishes "user doesn't exist" from "garbage input"
		// for an attacker. Just return success.
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}

	user, err := h.state.Store.GetUserByEmail(email)
	if err != nil || user == nil {
		if err != nil && err != sql.ErrNoRows {
			log.Printf("forgot-password: lookup error for %s: %v", email, err)
		}
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}

	policy := LoadAuthPolicy(h.state)
	token, err := randomToken(32)
	if err != nil {
		// Token gen really shouldn't fail — log it for ops, return silent success.
		log.Printf("forgot-password: token generation failed: %v", err)
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	// Per-MAILBOX cooldown, the twin of the one on /auth/resend-verification.
	//
	// The per-IP limiter on this route bounds one CALLER; it does not bound one
	// mailbox, which is what an attacker rotating source addresses aims at. And
	// this is not only an inbox-filling and mail-bill problem: the write below
	// REPLACES the reset token, so an unthrottled loop keeps invalidating the
	// link the user is trying to click, for as long as it runs. Its sibling
	// spells that reasoning out and has been enforcing it; this endpoint,
	// which does the same thing to the same mailbox, had nothing.
	//
	// No new column: a reset token's expiry is its send time plus the policy
	// TTL, so subtracting the cooldown from the expiry being written gives the
	// boundary the store compares against, atomically inside the UPDATE.
	expires := time.Now().Add(time.Duration(policy.PasswordResetLinkTTLMinutes) * time.Minute)
	issued, err := h.state.Store.SetPasswordResetToken(user.ID, token, expires, expires.Add(-passwordResetCooldown))
	if err != nil {
		log.Printf("forgot-password: SetPasswordResetToken for userID=%s: %v", user.ID, err)
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	if !issued {
		// A fresh link is already in that inbox. Silent success, same as every
		// other no-op branch here - telling the caller they were throttled
		// would confirm the address is on file.
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}

	if err := sendPasswordResetEmail(h.state, user.Email, user.Username, token, policy.PasswordResetLinkTTLMinutes); err != nil {
		// Mail failed but the token is stored — the user can retry. Not
		// surfaced to the caller: silent-success is the rule, so that the
		// endpoint cannot be used to tell a real address from an invented one.
		// That is also why this goes to the operator's error stream and not
		// only to the log - the requester is told nothing by design, so the
		// operator is the only one left who can learn that resets are dead.
		services.ReportOperatorError("password-reset", "send to %s failed: %v", user.Email, err)
	}

	LogIdentityAudit(h.state, r, AuditEventPasswordResetRequested, "", user.ID, map[string]interface{}{
		"email":       user.Email,
		"ttl_minutes": policy.PasswordResetLinkTTLMinutes,
	})

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// passwordResetCooldown is the minimum gap between two reset emails to ONE
// address. Deliberately the same 60 seconds as resendVerificationCooldown: both
// endpoints send mail on an anonymous request, both replace the token they just
// sent, and a caller inside the per-IP route limit is inside this one too.
const passwordResetCooldown = 60 * time.Second

type validateResetTokenRequest struct {
	Token string `json:"token"`
}

// ValidateResetToken POST /api/auth/validate-reset-token — public.
// Lets the /reset-password page tell the user whose account they're resetting
// before they type a new password, and surfaces "link expired" cleanly
// instead of waiting for the submit to fail.
//
// Returns username on success — that's intentional: the link itself implies
// possession of the account, and showing the username helps the user confirm
// they're resetting the right one. Email is NOT returned to avoid leaking
// it to anyone with a stolen link.
func (h *PasswordResetHandler) ValidateResetToken(w http.ResponseWriter, r *http.Request) {
	var req validateResetTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Token) < 16 {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "valid": false})
		return
	}
	user, err := h.state.Store.GetUserByPasswordResetToken(req.Token)
	if err != nil || user == nil {
		// Token not found / expired — both look the same to the caller.
		// Don't 4xx; the page renders an "invalid or expired" panel.
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "valid": false})
		return
	}

	out := map[string]interface{}{
		"success":  true,
		"valid":    true,
		"username": user.Username,
	}

	// Surface the user's chosen questions when the policy
	// demands answers at reset AND the user actually has questions set up.
	// Users without questions get a free pass: reset works without a
	// verification step (otherwise the policy locks them out forever).
	policy := LoadAuthPolicy(h.state)
	if policy.SecurityQuestionsEnabled && policy.SecurityQuestionsRequiredAtReset {
		qs, _ := h.state.Store.GetUserSecurityQuestions(user.ID)
		if len(qs) > 0 {
			out["requiresSecurityQuestions"] = true
			out["questions"] = qs
		}
	}
	json.NewEncoder(w).Encode(out)
}

type resetPasswordRequest struct {
	Token           string   `json:"token"`
	Password        string   `json:"password"`
	SecurityAnswers []string `json:"securityAnswers,omitempty"`
}

// ResetPassword POST /api/auth/reset-password — public.
// Consumes the token, sets the new password, clears any pending verification
// state. The token clear is part of the same UPDATE so a concurrent second
// reset using the same token fails cleanly.
func (h *PasswordResetHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Token) < 16 {
		sendJSONError(w, "Invalid token", http.StatusBadRequest)
		return
	}
	policy := LoadAuthPolicy(h.state)
	if len(req.Password) < policy.PasswordMinLength {
		sendJSONError(w, fmt.Sprintf("Password must be at least %d characters", policy.PasswordMinLength), http.StatusBadRequest)
		return
	}

	user, err := h.state.Store.GetUserByPasswordResetToken(req.Token)
	if err != nil || user == nil {
		// Same response shape as the verify-email gone branch — caller-side
		// renders "your link expired, request a new one".
		sendJSONError(w, "Reset link is invalid or expired", http.StatusGone)
		return
	}

	// Verify security answers when the policy demands it AND
	// the user has questions on file. Skipping the check when the user has
	// none on file is intentional: matches ValidateResetToken's behavior and
	// avoids permanently locking out users created before the policy flipped on.
	if policy.SecurityQuestionsEnabled && policy.SecurityQuestionsRequiredAtReset {
		stored, _ := h.state.Store.GetUserSecurityQuestionsRaw(user.ID)
		if stored != "" && stored != "[]" {
			if !verifySecurityAnswersAgainstStored(stored, req.SecurityAnswers) {
				LogIdentityAudit(h.state, r, AuditEventSecurityAnswersFailedAtReset, "", user.ID, nil)
				sendJSONError(w, "Security answers did not match", http.StatusUnauthorized)
				return
			}
		}
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		sendJSONError(w, "Failed to hash password", http.StatusInternalServerError)
		return
	}

	// One statement: UpdateUserPassword clears the reset token in the same
	// UPDATE that writes the password, so the link is spent exactly when the
	// password lands. This used to be two calls, ordered so a crash between
	// them left the token consumable - which also meant a failed clear left a
	// live link behind. Atomic is both simpler and stricter.
	if err := h.state.Store.UpdateUserPassword(user.ID, string(hashed)); err != nil {
		sendJSONError(w, "Failed to update password", http.StatusInternalServerError)
		return
	}

	LogIdentityAudit(h.state, r, AuditEventPasswordResetCompleted, "", user.ID, nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"username": user.Username,
	})
}

// sendPasswordResetEmail renders the password-reset template and sends it.
//
// The wording lives in the template now rather than in a Sprintf here, which
// is what makes it editable without a deploy. Kept separate from the
// verification mail on purpose: they are two definitions with two variable
// sets, and ttl_minutes only makes sense on this one.
func sendPasswordResetEmail(state *AppState, to, username, token string, ttlMinutes int) error {
	link := strings.TrimRight(state.FrontendURL, "/") + "/reset-password?token=" + token
	return services.SendMail(state.Store, mailer.KeyPasswordReset, to, state.FrontendURL, map[string]string{
		"username":    username,
		"reset_link":  link,
		"ttl_minutes": strconv.Itoa(ttlMinutes),
	})
}
