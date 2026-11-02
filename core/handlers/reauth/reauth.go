package reauth

import (
	"errors"
	"net/http"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	"dylaris-core/handlers/httpx"
	"dylaris-core/models"
	"dylaris-core/store"
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
type ReauthFields struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// ReauthError carries the status the caller has to answer with. A wrong
// password and a broken database are not the same event and must not collapse
// into one message.
type ReauthError struct {
	status  int
	message string
}

func (e *ReauthError) Error() string { return e.message }

// CodeVerifier checks a second-factor code (TOTP or backup code) for user.
// The check itself, with its replay and wrong-code limits and the backup-code
// audit, lives with the login; ErrTooManyCodes is its over-the-limit answer.
type CodeVerifier func(user *models.User, code string) (bool, error)

// ErrTooManyCodes is a CodeVerifier refusing to check a code because the
// account is over the wrong-code limit.
var ErrTooManyCodes = errors.New("too many wrong codes")

// RequireReauth re-proves the identity behind userID. nil means proceed.
//
// The password alone is enough for an account with no 2FA, and that carve-out
// is deliberate rather than an oversight: there is no second factor to ask such
// an account for, and demanding a code anyway would not harden anything - it
// would lock every user without 2FA out of the action completely. The password
// is the entire credential at login for those accounts too.
//
// Backup codes count, via the verifier (verifyTOTPOrBackupWith). These are account-management
// actions, and a user whose phone is gone has to be able to manage their
// account; the ticket-restore Danger Zone deliberately refuses them for the
// opposite reason, being destructive rather than administrative.
func RequireReauth(st store.Store, verify CodeVerifier, userID, password, code string) *ReauthError {
	if st == nil {
		return &ReauthError{http.StatusServiceUnavailable, "Database not connected"}
	}
	user, err := st.GetUserByID(userID)
	if err != nil || user == nil {
		return &ReauthError{http.StatusUnauthorized, "Unauthorized"}
	}
	if bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)) != nil {
		return &ReauthError{http.StatusUnauthorized, "Your password is not correct"}
	}
	if !user.Is2FAEnabled {
		return nil
	}
	// Without spending the step - see verifyTOTPOrBackupWith. A save that makes
	// two writes asks once, and the replay this protects against is the one at
	// the login endpoint, which nobody reaches with a session they already have.
	ok, verr := verify(user, code)
	if errors.Is(verr, ErrTooManyCodes) {
		return &ReauthError{http.StatusTooManyRequests, "Too many wrong codes. Try again in 15 minutes."}
	}
	if verr != nil {
		return &ReauthError{http.StatusInternalServerError, "Verification failed"}
	}
	if !ok {
		return &ReauthError{http.StatusUnauthorized, "Your two-factor code is not correct"}
	}
	return nil
}

// FailedHeader marks a refused re-authentication. It stays a 401 -
// integrators of the API-key and admin endpoints may rely on that - but the
// panel reads every 401 as an expired session and signed the user out over a
// mistyped password. The panel skips its sign-out for a response carrying this.
const FailedHeader = "X-Dylaris-Reauth"

// WriteReauthError answers a failed re-authentication.
func WriteReauthError(w http.ResponseWriter, e *ReauthError) {
	w.Header().Set(FailedHeader, "failed")
	httpx.SendJSONError(w, e.message, e.status)
}

// AdminReauthRequest is the shape the ADMIN account-management actions carry
// their re-authentication in.
//
// Nested rather than the flat password/code the two self-service endpoints
// take, and the reason is not style: on an admin endpoint "password" already
// means the TARGET's new password, and on a create it means the new account's.
// One word cannot mean both the credential being SET and the credential being
// PROVEN. The self-service endpoints keep the flat form, where "password" is
// unambiguously the caller's own and integrators already send it that way.
type AdminReauthRequest struct {
	Reauth ReauthFields `json:"reauth"`
}

// RequireAdminReauth re-proves the acting administrator, and answers the
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
func RequireAdminReauth(w http.ResponseWriter, r *http.Request, st store.Store, verify CodeVerifier, body AdminReauthRequest) bool {
	actorID, _ := r.Context().Value("userID").(string)
	if actorID == "" {
		httpx.SendJSONError(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	if rerr := RequireReauth(st, verify, actorID, body.Reauth.Password, body.Reauth.Code); rerr != nil {
		WriteReauthError(w, rerr)
		return false
	}
	return true
}

// TOTPStepSeconds and TOTPSkewSteps mirror what totp.Validate does by
// default: 30-second steps, plus or minus one for clock drift.
const (
	TOTPStepSeconds = 30
	TOTPSkewSteps   = 1
)

// MatchTOTPStep reports which time step a code belongs to, or ok=false for
// none. totp.Validate answers only yes or no, and "which step" is what makes a
// code claimable exactly once.
func MatchTOTPStep(code, secret string, now time.Time) (step int64, ok bool) {
	// An empty secret is an empty HMAC key, and the code for it is public:
	// pquerna validates it. A secret the store cannot open reads as "".
	if secret == "" {
		return 0, false
	}
	opts := totp.ValidateOpts{Period: TOTPStepSeconds, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}
	for off := -TOTPSkewSteps; off <= TOTPSkewSteps; off++ {
		t := now.Add(time.Duration(off) * TOTPStepSeconds * time.Second)
		valid, err := totp.ValidateCustom(code, secret, t, opts)
		if err == nil && valid {
			return t.Unix() / TOTPStepSeconds, true
		}
	}
	return 0, false
}
