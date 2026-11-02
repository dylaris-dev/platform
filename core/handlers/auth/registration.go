package auth

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

	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"
	"dylaris-core/mailer"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
	"dylaris-pkg/validate"

	"golang.org/x/crypto/bcrypt"
)

type RegistrationHandler struct {
	d Deps
}

func NewRegistrationHandler(d Deps) *RegistrationHandler {
	return &RegistrationHandler{d: d}
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
	p := services.LoadAuthPolicy(h.d.Store)
	out := map[string]interface{}{
		"success":             true,
		"registrationEnabled": p.RegistrationEnabled,
		"emailVerifyRequired": p.EmailVerifyRequired,
		"passwordMinLength":   p.PasswordMinLength,
	}
	// Where the panel links its imprint, privacy policy and terms: the hosted
	// panel is the one connected to the dylaris.com store, and those pages are
	// the operator's. A self-hosted panel gets none - our imprint is not theirs.
	if h.d.StoreEnabled {
		out["legalBaseUrl"] = strings.TrimRight(h.d.StoreURL, "/")
	}
	if p.SecurityQuestionsEnabled && p.SecurityQuestionsRequiredAtSignup {
		out["securityQuestionsRequired"] = true
		out["securityQuestionsCount"] = p.SecurityQuestionsCount
		out["securityQuestionsPool"] = LoadSecurityQuestionPool(h.d.Store)
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
	policy := services.LoadAuthPolicy(h.d.Store)
	if !policy.RegistrationEnabled {
		httpx.SendJSONError(w, "Registration is disabled", http.StatusForbidden)
		return
	}

	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(req.Username)
	email := strings.ToLower(strings.TrimSpace(req.Email))
	password := req.Password

	if !validate.IsUsername(username) {
		httpx.SendJSONError(w, "Invalid username (3–32 chars, letters/digits/._- only)", http.StatusBadRequest)
		return
	}
	if !emailRegex.MatchString(email) {
		httpx.SendJSONError(w, "Invalid email address", http.StatusBadRequest)
		return
	}
	if len(password) < policy.PasswordMinLength {
		httpx.SendJSONError(w, fmt.Sprintf("Password must be at least %d characters", policy.PasswordMinLength), http.StatusBadRequest)
		return
	}

	// Username uniqueness is hard-checked (the DB UNIQUE constraint would
	// kick in anyway — surface it as a clean error).
	// Case-INSENSITIVE, so `NewComer` cannot be claimed beside an existing
	// `newcomer`. The database has the last word (unique index on
	// LOWER(username)); this is the friendly message.
	if taken, _ := h.d.Store.UsernameTaken(username, ""); taken {
		httpx.SendJSONError(w, "Username is taken", http.StatusConflict)
		return
	}

	// Email enumeration mitigation: if the email already exists, return the
	// same success response a fresh registration would, so an attacker
	// can't probe which addresses are signed up. We log it though.
	if existing, err := h.d.Store.GetUserByEmail(email); err == nil && existing != nil {
		log.Printf("registration: email %q already in use (silent-success)", email)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":         true,
			"emailVerifySent": policy.EmailVerifyRequired,
		})
		return
	} else if err != nil && err != sql.ErrNoRows {
		httpx.SendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		httpx.SendJSONError(w, "Failed to hash password", http.StatusInternalServerError)
		return
	}

	user := &models.User{
		Username: username,
		Email:    email,
		Password: string(hashed),
		IsAdmin:  false,
	}
	if err := h.d.Store.CreateUser(user); err != nil {
		log.Printf("registration: CreateUser failed for username=%q: %v", username, err)
		httpx.SendJSONError(w, "Failed to create account", http.StatusInternalServerError)
		return
	}

	// Security questions. When required at signup, the user
	// MUST have submitted a valid set. When optional, we accept them if
	// provided but don't require them.
	if policy.SecurityQuestionsEnabled {
		if policy.SecurityQuestionsRequiredAtSignup && len(req.SecurityQuestions) == 0 {
			// Roll back the user since the policy requirement wasn't met.
			// In practice the frontend should never submit without questions
			// when the status endpoint reports required=true.
			if derr := h.d.Store.DeleteUser(user.ID); derr != nil {
				log.Printf("registration: rollback of half-created user %s failed; the row is now orphaned and holds the username: %v", user.ID, derr)
			}
			httpx.SendJSONError(w, "Security questions are required for registration", http.StatusBadRequest)
			return
		}
		if len(req.SecurityQuestions) > 0 {
			hashedJSON, err := buildHashedQAJSON(req.SecurityQuestions, LoadSecurityQuestionPool(h.d.Store), policy.SecurityQuestionsCount)
			if err != nil {
				if derr := h.d.Store.DeleteUser(user.ID); derr != nil {
					log.Printf("registration: rollback of half-created user %s failed; the row is now orphaned and holds the username: %v", user.ID, derr)
				}
				httpx.SendJSONError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := h.d.Store.SetUserSecurityQuestions(user.ID, hashedJSON); err != nil {
				log.Printf("registration: SetUserSecurityQuestions failed for userID=%s: %v", user.ID, err)
				if policy.SecurityQuestionsRequiredAtSignup {
					// Required by policy — don't leave a half-created account
					// without the questions it must have; roll back instead.
					if derr := h.d.Store.DeleteUser(user.ID); derr != nil {
						log.Printf("registration: rollback of half-created user %s failed; the row is now orphaned and holds the username: %v", user.ID, derr)
					}
					httpx.SendJSONError(w, "Failed to save security questions", http.StatusInternalServerError)
					return
				}
				// Optional: account exists, user can set questions in profile.
			}
		}
	}

	if policy.EmailVerifyRequired {
		token, err := randomToken(32)
		if err != nil {
			httpx.SendJSONError(w, "Failed to generate verification token", http.StatusInternalServerError)
			return
		}
		if err := h.d.Store.SetEmailVerificationToken(user.ID, token); err != nil {
			log.Printf("registration: SetEmailVerificationToken failed: %v", err)
			httpx.SendJSONError(w, "Failed to persist verification token", http.StatusInternalServerError)
			return
		}
		// In the background, like the other anonymous senders: the result was
		// never shown (the account exists either way and the user can resend),
		// and waiting for the relay only made the request hang with it.
		go func(email, username string) {
			if err := sendVerificationEmail(h.d.Store, h.d.FrontendURL, email, username, token); err != nil {
				services.ReportOperatorError("registration", "send verification email to %s failed: %v", email, err)
			}
		}(user.Email, user.Username)
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
		if err := h.d.Store.MarkEmailVerified(user.ID); err != nil {
			log.Printf("registration: could not mark %s (userID=%s) verified while verification is disabled; they will be locked out if the policy is enabled later: %v", user.Username, user.ID, err)
		}
	}

	audit.LogIdentity(h.d.Store, r, "user_registered", "", user.ID, map[string]interface{}{
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
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Token) < 16 {
		httpx.SendJSONError(w, "Invalid token", http.StatusBadRequest)
		return
	}

	user, err := h.d.Store.GetUserByEmailVerificationToken(req.Token)
	if err == sql.ErrNoRows || user == nil {
		// Token gone — could be already verified or expired.
		httpx.SendJSONError(w, "Verification link is invalid or already used", http.StatusGone)
		return
	}
	if err != nil {
		httpx.SendJSONError(w, "Database error", http.StatusInternalServerError)
		return
	}

	// A link for an address change the user asked for: it becomes the
	// account's address now, unless another account took it in the meantime.
	if user.PendingEmail != "" {
		if other, oerr := h.d.Store.GetUserByEmail(user.PendingEmail); oerr == nil && other != nil && other.ID != user.ID {
			httpx.SendJSONError(w, "That email address is already in use by another account", http.StatusConflict)
			return
		}
		oldEmail, newEmail, ok, cerr := h.d.Store.ConfirmPendingEmail(user.ID)
		if cerr != nil {
			httpx.SendJSONError(w, "Failed to verify email", http.StatusInternalServerError)
			return
		}
		if ok {
			audit.LogIdentity(h.d.Store, r, audit.AuditEventUserEmailChanged, user.ID, user.ID, nil)
			notifyAddressChanged(h.d.Store, h.d.FrontendURL, oldEmail, newEmail, user.Username)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "username": user.Username})
			return
		}
	}
	if err := h.d.Store.MarkEmailVerified(user.ID); err != nil {
		httpx.SendJSONError(w, "Failed to verify email", http.StatusInternalServerError)
		return
	}
	audit.LogIdentity(h.d.Store, r, "email_verified", "", user.ID, nil)

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
	policy := services.LoadAuthPolicy(h.d.Store)
	if !policy.EmailVerifyRequired {
		// If verification isn't required, the endpoint is meaningless. Be
		// explicit so the UI can hide the button accordingly.
		httpx.SendJSONError(w, "Email verification is not required by this server", http.StatusBadRequest)
		return
	}

	var req resendVerificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !emailRegex.MatchString(email) {
		httpx.SendJSONError(w, "Invalid email address", http.StatusBadRequest)
		return
	}

	user, err := h.d.Store.GetUserByEmail(email)
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
		httpx.SendJSONError(w, "Failed to generate token", http.StatusInternalServerError)
		return
	}
	if err := h.d.Store.SetEmailVerificationToken(user.ID, token); err != nil {
		httpx.SendJSONError(w, "Failed to persist token", http.StatusInternalServerError)
		return
	}
	// In the background: an unverified account answered after a mail round
	// trip and every other case at once, which told a caller measuring time
	// which addresses are registered and still unverified.
	go func(email, username string) {
		if err := sendVerificationEmail(h.d.Store, h.d.FrontendURL, email, username, token); err != nil {
			services.ReportOperatorError("registration", "resend verification to %s failed: %v", email, err)
		}
	}(user.Email, user.Username)
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
// sendEmailChangeConfirmation mails the link that makes a pending address the
// account's. Not the registration mail: that one welcomes a new account, and a
// stranger receiving it would read a sign-up instead of somebody asking to put
// their address on an existing one.
func sendEmailChangeConfirmation(st store.Store, frontendURL string, to, username, token string) error {
	link := strings.TrimRight(frontendURL, "/") + "/verify-email?token=" + token
	return sendAccountMail(st, mailer.KeyConfirmEmailChange, to, frontendURL, map[string]string{
		"username":     username,
		"confirm_link": link,
	})
}

func sendVerificationEmail(st store.Store, frontendURL string, to, username, token string) error {
	link := strings.TrimRight(frontendURL, "/") + "/verify-email?token=" + token
	// Wording lives in the template now, so an operator can change it without a
	// deploy. An install that has never edited it sends the built-in default,
	// which is this text.
	return sendAccountMail(st, mailer.KeyVerifyEmail, to, frontendURL, map[string]string{
		"username":    username,
		"verify_link": link,
	})
}
