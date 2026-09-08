package handlers

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"dylaris-core/mailer"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-pkg/validate"

	"golang.org/x/crypto/bcrypt"
)

type RegistrationHandler struct {
	state *AppState
}

func NewRegistrationHandler(state *AppState) *RegistrationHandler {
	return &RegistrationHandler{state: state}
}

// emailRegex is intentionally cheap — full RFC 5322 validation is a rabbit
// hole. We accept anything that looks roughly like local@host.tld; SMTP
// will reject bad addresses on Send.
var emailRegex = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

type registerRequest struct {
	Username          string              `json:"username"`
	Email             string              `json:"email"`
	Password          string              `json:"password"`
	SecurityQuestions []securityQARequest `json:"securityQuestions,omitempty"`
}

// RegistrationStatus GET /api/auth/registration-status — public.
// Tells the login page whether to render the "Register" link and what the
// password policy is (so the registration form can show the requirement
// before the user submits and gets a 400). The security-questions flags let
// the register page render the picker step inline instead of forcing the user
// through a second visit.
func (h *RegistrationHandler) RegistrationStatus(w http.ResponseWriter, r *http.Request) {
	p := LoadAuthPolicy(h.state)
	out := map[string]interface{}{
		"success":             true,
		"registrationEnabled": p.RegistrationEnabled,
		"emailVerifyRequired": p.EmailVerifyRequired,
		"passwordMinLength":   p.PasswordMinLength,
	}
	if p.SecurityQuestionsEnabled && p.SecurityQuestionsRequiredAtSignup {
		out["securityQuestionsRequired"] = true
		out["securityQuestionsCount"] = p.SecurityQuestionsCount
		out["securityQuestionsPool"] = LoadSecurityQuestionPool(h.state)
	} else {
		out["securityQuestionsRequired"] = false
	}
	json.NewEncoder(w).Encode(out)
}

// Register POST /api/auth/register — public, gated on auth.registration_enabled.
// Always returns success even on dup-email/dup-username to avoid leaking
// account enumeration — the legitimate user will discover the conflict via
// their existing account or via the verification email never arriving.
func (h *RegistrationHandler) Register(w http.ResponseWriter, r *http.Request) {
	policy := LoadAuthPolicy(h.state)
	if !policy.RegistrationEnabled {
		sendJSONError(w, "Registration is disabled", http.StatusForbidden)
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(req.Username)
	email := strings.ToLower(strings.TrimSpace(req.Email))
	password := req.Password

	if !validate.IsUsername(username) {
		sendJSONError(w, "Invalid username (3–32 chars, letters/digits/._- only)", http.StatusBadRequest)
		return
	}
	if !emailRegex.MatchString(email) {
		sendJSONError(w, "Invalid email address", http.StatusBadRequest)
		return
	}
	if len(password) < policy.PasswordMinLength {
		sendJSONError(w, fmt.Sprintf("Password must be at least %d characters", policy.PasswordMinLength), http.StatusBadRequest)
		return
	}

	// Username uniqueness is hard-checked (the DB UNIQUE constraint would
	// kick in anyway — surface it as a clean error).
	// Case-INSENSITIVE, so `NewComer` cannot be claimed beside an existing
	// `newcomer`. The database has the last word (unique index on
	// LOWER(username)); this is the friendly message.
	if taken, _ := h.state.Store.UsernameTaken(username, ""); taken {
		sendJSONError(w, "Username is taken", http.StatusConflict)
		return
	}

	// Email enumeration mitigation: if the email already exists, return the
	// same success response a fresh registration would, so an attacker
	// can't probe which addresses are signed up. We log it though.
	if existing, err := h.state.Store.GetUserByEmail(email); err == nil && existing != nil {
		log.Printf("registration: email %q already in use (silent-success)", email)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":         true,
			"emailVerifySent": policy.EmailVerifyRequired,
		})
		return
	} else if err != nil && err != sql.ErrNoRows {
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		sendJSONError(w, "Failed to hash password", http.StatusInternalServerError)
		return
	}

	user := &models.User{
		Username: username,
		Email:    email,
		Password: string(hashed),
		IsAdmin:  false,
	}
	if err := h.state.Store.CreateUser(user); err != nil {
		log.Printf("registration: CreateUser failed for username=%q: %v", username, err)
		sendJSONError(w, "Failed to create account", http.StatusInternalServerError)
		return
	}

	// Region access default — self-registered users get whatever the admin
	// configured (default: no access, admin must grant). This is distinct
	// from admin-created users, who default to all-regions.
	if err := h.state.Store.SetUserRegions(user.ID, policy.DefaultNewUserAllRegions, []string{}); err != nil {
		log.Printf("registration: SetUserRegions failed for userID=%s: %v", user.ID, err)
	}

	// Security questions. When required at signup, the user
	// MUST have submitted a valid set. When optional, we accept them if
	// provided but don't require them.
	if policy.SecurityQuestionsEnabled {
		if policy.SecurityQuestionsRequiredAtSignup && len(req.SecurityQuestions) == 0 {
			// Roll back the user since the policy requirement wasn't met.
			// In practice the frontend should never submit without questions
			// when the status endpoint reports required=true.
			if derr := h.state.Store.DeleteUser(user.ID); derr != nil {
				log.Printf("registration: rollback of half-created user %s failed; the row is now orphaned and holds the username: %v", user.ID, derr)
			}
			sendJSONError(w, "Security questions are required for registration", http.StatusBadRequest)
			return
		}
		if len(req.SecurityQuestions) > 0 {
			hashedJSON, err := buildHashedQAJSON(req.SecurityQuestions, LoadSecurityQuestionPool(h.state), policy.SecurityQuestionsCount)
			if err != nil {
				if derr := h.state.Store.DeleteUser(user.ID); derr != nil {
					log.Printf("registration: rollback of half-created user %s failed; the row is now orphaned and holds the username: %v", user.ID, derr)
				}
				sendJSONError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := h.state.Store.SetUserSecurityQuestions(user.ID, hashedJSON); err != nil {
				log.Printf("registration: SetUserSecurityQuestions failed for userID=%s: %v", user.ID, err)
				if policy.SecurityQuestionsRequiredAtSignup {
					// Required by policy — don't leave a half-created account
					// without the questions it must have; roll back instead.
					if derr := h.state.Store.DeleteUser(user.ID); derr != nil {
						log.Printf("registration: rollback of half-created user %s failed; the row is now orphaned and holds the username: %v", user.ID, derr)
					}
					sendJSONError(w, "Failed to save security questions", http.StatusInternalServerError)
					return
				}
				// Optional: account exists, user can set questions in profile.
			}
		}
	}

	if policy.EmailVerifyRequired {
		token, err := randomToken(32)
		if err != nil {
			sendJSONError(w, "Failed to generate verification token", http.StatusInternalServerError)
			return
		}
		if err := h.state.Store.SetEmailVerificationToken(user.ID, token); err != nil {
			log.Printf("registration: SetEmailVerificationToken failed: %v", err)
			sendJSONError(w, "Failed to persist verification token", http.StatusInternalServerError)
			return
		}
		// Send synchronously so the client gets a real result. Mail failures
		// don't roll back the user — they can re-request verification.
		if err := sendVerificationEmail(h.state, user.Email, user.Username, token); err != nil {
			services.ReportOperatorError("registration", "send verification email to %s failed: %v", user.Email, err)
			// Still report success — the account exists, user can resend.
		}
	} else {
		// Verification not required → mark verified immediately so the user
		// can log in right away.
		//
		// Harmless today if it fails - the login gate only checks
		// EmailVerifiedAt when EmailVerifyRequired is on, which it is not on
		// this branch. It becomes a lockout the moment an admin turns the
		// policy ON later: this account would then be treated as unverified and
		// never received a verification mail, because none was sent. Not worth
		// failing the registration over, very much worth being able to find.
		if err := h.state.Store.MarkEmailVerified(user.ID); err != nil {
			log.Printf("registration: could not mark %s (userID=%s) verified while verification is disabled; they will be locked out if the policy is enabled later: %v", user.Username, user.ID, err)
		}
	}

	LogIdentityAudit(h.state, r, "user_registered", "", user.ID, map[string]interface{}{
		"username": user.Username,
		"email":    user.Email,
	})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"emailVerifySent": policy.EmailVerifyRequired,
	})
}

type verifyEmailRequest struct {
	Token string `json:"token"`
}

// VerifyEmail POST /api/auth/verify-email — public.
// Idempotent: re-verifying an already-verified user (or one whose token was
// rotated) still returns success so the user doesn't see a scary error on
// a double-click.
func (h *RegistrationHandler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req verifyEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Token) < 16 {
		sendJSONError(w, "Invalid token", http.StatusBadRequest)
		return
	}

	user, err := h.state.Store.GetUserByEmailVerificationToken(req.Token)
	if err == sql.ErrNoRows || user == nil {
		// Token gone — could be already verified or expired.
		sendJSONError(w, "Verification link is invalid or already used", http.StatusGone)
		return
	}
	if err != nil {
		sendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}

	if err := h.state.Store.MarkEmailVerified(user.ID); err != nil {
		sendJSONError(w, "Failed to verify email", http.StatusInternalServerError)
		return
	}
	LogIdentityAudit(h.state, r, "email_verified", "", user.ID, nil)

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"username": user.Username,
	})
}

type resendVerificationRequest struct {
	Email string `json:"email"`
}

// ResendVerification POST /api/auth/resend-verification — public.
// Same enumeration-safe response: success even if email doesn't exist or
// is already verified. The legitimate user gets the email; an attacker
// gets the same response either way.
func (h *RegistrationHandler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	policy := LoadAuthPolicy(h.state)
	if !policy.EmailVerifyRequired {
		// If verification isn't required, the endpoint is meaningless. Be
		// explicit so the UI can hide the button accordingly.
		sendJSONError(w, "Email verification is not required by this server", http.StatusBadRequest)
		return
	}

	var req resendVerificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !emailRegex.MatchString(email) {
		sendJSONError(w, "Invalid email address", http.StatusBadRequest)
		return
	}

	user, err := h.state.Store.GetUserByEmail(email)
	if err != nil || user == nil || user.EmailVerifiedAt != nil {
		// Silent success in all "no-op" branches.
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}

	// The per-IP limiter on this route bounds one CALLER; it does not bound one
	// MAILBOX, which is what an attacker rotating source addresses aims at. And
	// every send below replaces the verification token, so an unthrottled loop
	// does not just fill the inbox and bill the mail provider - it invalidates
	// the link the user is trying to click, for as long as it runs.
	//
	// email_verification_sent_at has been written on every token issue and
	// loaded onto the user since the column was added. Nothing had ever read it.
	if !verificationResendAllowed(user.EmailVerificationSentAt, time.Now(), resendVerificationCooldown) {
		// Same silent success as the other no-op branches: answering
		// differently here would turn this endpoint into an oracle for which
		// addresses are registered and still unverified.
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}

	token, err := randomToken(32)
	if err != nil {
		sendJSONError(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}
	if err := h.state.Store.SetEmailVerificationToken(user.ID, token); err != nil {
		sendJSONError(w, "Failed to persist token", http.StatusInternalServerError)
		return
	}
	if err := sendVerificationEmail(h.state, user.Email, user.Username, token); err != nil {
		services.ReportOperatorError("registration", "resend verification to %s failed: %v", user.Email, err)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// resendVerificationCooldown is the minimum gap between two verification
// emails to ONE address. It matches the per-IP limiter's window, so a caller
// who is inside the route limit is still inside this one.
const resendVerificationCooldown = 60 * time.Second

// verificationResendAllowed reports whether another verification email may go
// out, given when the last one did. A nil lastSentAt means none was ever sent.
//
// A lastSentAt in the future (clock skew, or a row written by a host running
// ahead) yields a negative difference and so refuses - erring towards not
// sending, which is the safe direction for a mail the user can request again.
func verificationResendAllowed(lastSentAt *time.Time, now time.Time, cooldown time.Duration) bool {
	if lastSentAt == nil {
		return true
	}
	return now.Sub(*lastSentAt) >= cooldown
}

// randomToken returns a hex-encoded cryptographically random token.
// 32 bytes → 64-hex-char string, fits VARCHAR(64) on email_verification_token.
func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sendVerificationEmail builds and sends the verification email.
// Pure helper so the test endpoint and the production flow can be kept in
// sync (both go through mailer.Send).
func sendVerificationEmail(state *AppState, to, username, token string) error {
	link := strings.TrimRight(state.FrontendURL, "/") + "/verify-email?token=" + token
	// Wording lives in the template now, so an operator can change it without a
	// deploy. An install that has never edited it sends the built-in default,
	// which is this text.
	return services.SendMail(state.Store, mailer.KeyVerifyEmail, to, state.FrontendURL, map[string]string{
		"username":    username,
		"verify_link": link,
	})
}
