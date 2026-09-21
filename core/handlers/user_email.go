package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"dylaris-core/services"
)

// Changing an account's email address, which nothing could do before.
//
// The admin screen showed the address nowhere and no endpoint existed to write
// it - only /username had one. That is a hole rather than an omission, because
// while security questions are off the reset link is the ONLY way back into an
// account: a customer whose address is wrong, or who has lost the mailbox, is
// locked out permanently and the operator cannot help. Renaming them was
// possible; reaching them was not.
type UserEmailHandler struct {
	state *AppState
}

func NewUserEmailHandler(state *AppState) *UserEmailHandler {
	return &UserEmailHandler{state: state}
}

type setUserEmailRequest struct {
	Email string `json:"email"`
	adminReauthRequest
}

// SetEmail PATCH /api/admin/users/{id}/email - RequireCap("users.write") at the
// route.
//
// The new address is stored UNVERIFIED, and a verification mail goes out when
// the policy requires one. Both follow from the same fact: an admin typing an
// address has not shown that anyone reads it. Marking it verified would hand a
// verified badge to a mailbox nobody has answered, and the reset link aims
// there.
func (h *UserEmailHandler) SetEmail(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	id, ok := parseUserID(w, r)
	if !ok {
		return
	}
	var req setUserEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !emailRegex.MatchString(email) {
		sendJSONError(w, "Invalid email address", http.StatusBadRequest)
		return
	}

	target, err := h.state.Store.GetUserByID(id)
	if err != nil || target == nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	// The address is where a password reset goes, so this is a password change
	// by other means.
	if !mayManageAccount(h.state, r, target) {
		sendJSONError(w, "You cannot change the email of an account with more rights than yours", http.StatusForbidden)
		return
	}

	// Unchanged is a no-op rather than a rewrite. Storing it again would clear
	// email_verified_at, so a stray save on a screen that shows the current
	// address would un-verify an account that was fine - and, with the policy
	// on, lock its owner out until they found a new mail.
	if strings.EqualFold(strings.TrimSpace(target.Email), email) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"email":     email,
			"unchanged": true,
		})
		return
	}

	// There is no unique index on users.email, so this is the only thing
	// standing between two accounts and the same reset mailbox.
	if existing, eerr := h.state.Store.GetUserByEmail(email); eerr == nil && existing != nil && existing.ID != id {
		sendJSONError(w, "That email address is already in use", http.StatusConflict)
		return
	}

	// Re-authenticate last, after every cheap check and before the write.
	// Nothing above this line changes anything, and a bcrypt comparison in
	// front of the validation would let a malformed body buy CPU time - the
	// same order the API key mint gives its own reasons for. A save that
	// changes nothing returned above without asking at all.
	if !requireAdminReauth(w, r, h.state, req.adminReauthRequest) {
		return
	}

	if err := h.state.Store.SetUserEmail(id, email); err != nil {
		sendJSONError(w, "Failed to update the email address", http.StatusInternalServerError)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	// The addresses themselves are deliberately absent from the audit metadata:
	// the identity log is readable by anyone with audit access, and the row
	// already names WHO was changed and by WHOM, which is what an investigation
	// needs.
	LogIdentityAudit(h.state, r, AuditEventUserEmailChanged, actorID, id, nil)

	// Send the verification the account now needs. Only when the policy
	// requires one: without it the account is usable immediately and an
	// unexpected "confirm your account" mail would be noise.
	verifySent := false
	policy := LoadAuthPolicy(h.state)
	if policy.EmailVerifyRequired {
		if token, terr := randomToken(32); terr == nil {
			if serr := h.state.Store.SetEmailVerificationToken(id, token); serr == nil {
				if merr := sendVerificationEmail(h.state, email, target.Username, token); merr != nil {
					// The address is already stored, so this is not a failed
					// change - it is a user who cannot get in until a mail
					// arrives, which is exactly the kind of failure that used
					// to reach nobody.
					services.ReportOperatorError("admin-email-change",
						"verification mail to %s failed: %v", email, merr)
				} else {
					verifySent = true
				}
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"email":           email,
		"emailVerifySent": verifySent,
	})
}
