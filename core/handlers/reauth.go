package handlers

import (
	"net/http"

	"golang.org/x/crypto/bcrypt"
)

// Re-authentication for the actions that turn a borrowed session into a
// permanent one.
//
// Session invalidation in this codebase is strong and covers every path:
// AuthMiddleware compares a fingerprint derived from the stored password hash,
// so a self-service change, an admin reset, an emailed reset and any write path
// added later all kill every outstanding session at once, with nobody having to
// remember to stamp a column.
//
// It reaches neither of these two:
//
//   - an API key authenticates by its OWN hash (GetAPIKeyByHash), so it keeps
//     working after the password change that killed every session;
//   - security questions ARE the password-reset path, so overwriting them
//     survives even revoking every key.
//
// So both are asked for the credential again, the way DisableTOTP and
// RegenerateBackupCodes already were. Same two fields, so the API has one
// convention rather than a second one invented here.
type reauthFields struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// reauthError carries the status the caller has to answer with. A wrong
// password and a broken database are not the same event and must not collapse
// into one message.
type reauthError struct {
	status  int
	message string
}

func (e *reauthError) Error() string { return e.message }

// requireReauth re-proves the identity behind userID. nil means proceed.
//
// The password alone is enough for an account with no 2FA, and that carve-out
// is deliberate rather than an oversight: there is no second factor to ask such
// an account for, and demanding a code anyway would not harden anything - it
// would lock every user without 2FA out of the action completely. The password
// is the entire credential at login for those accounts too.
//
// Backup codes count, via verifyTOTPOrBackupFor. These are account-management
// actions, and a user whose phone is gone has to be able to manage their
// account; the ticket-restore Danger Zone deliberately refuses them for the
// opposite reason, being destructive rather than administrative.
func requireReauth(state *AppState, userID, password, code string) *reauthError {
	if state == nil || state.Store == nil {
		return &reauthError{http.StatusServiceUnavailable, "Database not connected"}
	}
	user, err := state.Store.GetUserByID(userID)
	if err != nil || user == nil {
		return &reauthError{http.StatusUnauthorized, "Unauthorized"}
	}
	if bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)) != nil {
		return &reauthError{http.StatusUnauthorized, "Your password is not correct"}
	}
	if !user.Is2FAEnabled {
		return nil
	}
	// Without spending the step - see verifyTOTPOrBackupWith. A save that makes
	// two writes asks once, and the replay this protects against is the one at
	// the login endpoint, which nobody reaches with a session they already have.
	ok, verr := verifyTOTPOrBackupWith(state, user, code, false)
	if verr != nil {
		return &reauthError{http.StatusInternalServerError, "Verification failed"}
	}
	if !ok {
		return &reauthError{http.StatusUnauthorized, "Your two-factor code is not correct"}
	}
	return nil
}

// writeReauthError answers a failed re-authentication.
func writeReauthError(w http.ResponseWriter, e *reauthError) {
	sendJSONError(w, e.message, e.status)
}

// adminReauthRequest is the shape the ADMIN account-management actions carry
// their re-authentication in.
//
// Nested rather than the flat password/code the two self-service endpoints
// take, and the reason is not style: on an admin endpoint "password" already
// means the TARGET's new password, and on a create it means the new account's.
// One word cannot mean both the credential being SET and the credential being
// PROVEN. The self-service endpoints keep the flat form, where "password" is
// unambiguously the caller's own and integrators already send it that way.
type adminReauthRequest struct {
	Reauth reauthFields `json:"reauth"`
}

// requireAdminReauth re-proves the acting administrator, and answers the
// request itself when it cannot. body is the already-decoded reauth block.
//
// It applies to the actions that hand somebody DURABLE access, which is the
// same rule the file's opening comment draws for API keys and security
// questions - one level up. Measured on production: with an admin session
// alone and no password, any account's password could be set, its second
// factor stripped, its address changed, and a fresh admin account created. The
// session-kill covers none of that: it ends the VICTIM's sessions, while the
// attacker's borrowed admin session is the thing doing the asking.
//
// The seven call sites are the three takeovers (password, two-factor, email)
// and the four grants of power (creating a privileged account, role,
// permission flags, panel role).
//
// Three of them ask only when the call CHANGES something - role, permission
// flags and email - because the panel re-sends the current values on every
// save and a save that grants nothing is not what this guard is for. The other
// four ask whenever they are called at all, since calling them IS the change.
func requireAdminReauth(w http.ResponseWriter, r *http.Request, state *AppState, body adminReauthRequest) bool {
	actorID, _ := r.Context().Value("userID").(string)
	if actorID == "" {
		sendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	if rerr := requireReauth(state, actorID, body.Reauth.Password, body.Reauth.Code); rerr != nil {
		writeReauthError(w, rerr)
		return false
	}
	return true
}
