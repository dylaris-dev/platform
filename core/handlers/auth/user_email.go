package auth

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"dylaris-core/handlers/access"
	"dylaris-core/handlers/audit"
	"dylaris-core/handlers/httpx"
	"dylaris-core/handlers/reauth"
	"dylaris-core/mailer"
	"dylaris-core/services"
	"dylaris-core/store"
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
	d Deps
}

func NewUserEmailHandler(d Deps) *UserEmailHandler {
	return &UserEmailHandler{d: d}
}

type setUserEmailRequest struct {
	Email string `json:"email"`
	reauth.AdminReauthRequest
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
	if h.d.Store == nil {
		httpx.SendJSONError(w, "Database not connected", 503)
		return
	}
	id, ok := httpx.ParseUserID(w, r)
	if !ok {
		return
	}
	var req setUserEmailRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if email == "" || !emailRegex.MatchString(email) {
		httpx.SendJSONError(w, "Invalid email address", http.StatusBadRequest)
		return
	}

	target, err := h.d.Store.GetUserByID(id)
	if err != nil || target == nil {
		httpx.SendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	// The address is where a password reset goes, so this is a password change
	// by other means.
	if !access.MayManageAccount(h.d.Store, h.d.Authz, r, target) {
		httpx.SendJSONError(w, "You cannot change the email of an account with more rights than yours", http.StatusForbidden)
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
	if existing, eerr := h.d.Store.GetUserByEmail(email); eerr == nil && existing != nil && existing.ID != id {
		httpx.SendJSONError(w, "That email address is already in use", http.StatusConflict)
		return
	}

	// Re-authenticate last, after every cheap check and before the write.
	// Nothing above this line changes anything, and a bcrypt comparison in
	// front of the validation would let a malformed body buy CPU time - the
	// same order the API key mint gives its own reasons for. A save that
	// changes nothing returned above without asking at all.
	if !reauth.RequireAdminReauth(w, r, h.d.Store, h.d.ReauthCodes, req.AdminReauthRequest) {
		return
	}

	// Read before the write: the notice goes to the address being replaced.
	oldEmail := target.Email
	if err := h.d.Store.SetUserEmail(id, email); err != nil {
		httpx.SendJSONError(w, "Failed to update the email address", http.StatusInternalServerError)
		return
	}

	actorID, _ := r.Context().Value("userID").(string)
	// The addresses themselves are deliberately absent from the audit metadata:
	// the identity log is readable by anyone with audit access, and the row
	// already names WHO was changed and by WHOM, which is what an investigation
	// needs.
	audit.LogIdentity(h.d.Store, r, audit.AuditEventUserEmailChanged, actorID, id, nil)
	NotifyAddressChanged(h.d.Store, h.d.FrontendURL, oldEmail, email, target.Username)

	// Send the verification the account now needs. Only when the policy
	// requires one: without it the account is usable immediately and an
	// unexpected "confirm your account" mail would be noise.
	verifySent := SendChangedEmailVerification(h.d.Store, h.d.FrontendURL, id, email, target.Username, "admin-email-change")

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"email":           email,
		"emailVerifySent": verifySent,
	})
}

// sendAccountMail is services.SendMail; a variable so a test can see what the
// notices send without an SMTP server.
var sendAccountMail = services.SendMail

// SetAccountMailSender replaces the account notice sender for a test and
// returns the restore func. Production never calls it.
func SetAccountMailSender(fn func(services.MailStore, string, string, string, map[string]string) error) (restore func()) {
	prev := sendAccountMail
	sendAccountMail = fn
	return func() { sendAccountMail = prev }
}

// NotifyAddressChanged tells the PREVIOUS address that the account's address
// changed. It used to hear nothing: whoever changed it - an attacker holding
// the session and the password, or a compromised operator account - also
// redirected every future password reset, and the owner found out when they
// could no longer get in. Sent in the background, like every account mail, so
// the answer does not wait on SMTP; a failure is logged, the change stands.
func NotifyAddressChanged(st store.Store, frontendURL string, oldEmail, newEmail, username string) {
	if strings.TrimSpace(oldEmail) == "" {
		return
	}
	// Read here, not in the goroutine: a test swapping it must not race a
	// notice still in flight.
	send := sendAccountMail
	go func() {
		if err := send(st, mailer.KeyEmailChanged, oldEmail, frontendURL, map[string]string{
			"username": username, "new_email": newEmail,
		}); err != nil {
			log.Printf("account mail: address-change notice for %s not sent: %v", username, err)
		}
	}()
}

// NotifyPasswordChanged tells the account's address that its password changed.
func NotifyPasswordChanged(st store.Store, frontendURL string, email, username string) {
	if strings.TrimSpace(email) == "" {
		return
	}
	// Read here, not in the goroutine: a test swapping it must not race a
	// notice still in flight.
	send := sendAccountMail
	go func() {
		if err := send(st, mailer.KeyPasswordChanged, email, frontendURL, map[string]string{
			"username": username,
		}); err != nil {
			log.Printf("account mail: password-change notice for %s not sent: %v", username, err)
		}
	}()
}

// SendChangedEmailVerification sends the verification an account needs after its
// address changed, and reports whether one went out.
//
// Shared by the two doors that change an address - the admin screen and the
// account's own profile - because they have to agree. Only when the policy
// requires verification: without it the account is usable immediately and an
// unexpected "confirm your account" mail would be noise.
func SendChangedEmailVerification(st store.Store, frontendURL string, userID, email, username, source string) bool {
	if email == "" || !services.LoadAuthPolicy(st).EmailVerifyRequired {
		return false
	}
	token, err := RandomToken(32)
	if err != nil {
		return false
	}
	if err := st.SetEmailVerificationToken(userID, token); err != nil {
		return false
	}
	if err := sendVerificationEmail(st, frontendURL, email, username, token); err != nil {
		// The address is already stored, so this is not a failed change - it is
		// a user who cannot get in until a mail arrives, which is exactly the
		// kind of failure that used to reach nobody.
		services.ReportOperatorError(source, "verification mail to %s failed: %v", email, err)
		return false
	}
	return true
}
