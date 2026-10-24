package tickets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dylaris-core/handlers"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/pquerna/otp/totp"
)

// restoreFakeStore is the only lookup ExecuteRestore makes before the TOTP
// check. The embedded nil store.Store panics on anything else, which is what
// keeps this test honest about how far the handler got.
type restoreFakeStore struct {
	store.Store
	user *models.User
}

// GetSetting answers the storage-config resolution the handler reaches AFTER
// the 2FA gate. It returns nothing configured, so the provider build fails
// cleanly with a 503 instead of panicking on the embedded nil store - which is
// exactly the outcome this test wants: the gate was passed, the restore was not
// performed, and no ticket table was touched.
func (f *restoreFakeStore) GetSetting(string) (string, error) { return "", nil }

func (f *restoreFakeStore) GetUserByID(string) (*models.User, error) {
	if f.user == nil {
		return nil, errors.New("no user")
	}
	return f.user, nil
}

const restoreTestUser = "aaaaaaaa-1111-4111-8111-111111111111"

func newRestoreTest(t *testing.T) (*TicketMigrationHandler, string, string) {
	t.Helper()
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "dylaris-test", AccountName: "admin"})
	if err != nil {
		t.Fatalf("generate totp: %v", err)
	}
	secret := key.Secret()
	h := NewTicketMigrationHandler(&handlers.AppState{
		Store: &restoreFakeStore{user: &models.User{
			ID: restoreTestUser, Is2FAEnabled: true, TOTPSecret: secret,
		}},
	})
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef"
	now := time.Now()
	h.tokens[token] = &restoreToken{
		BackupName:         "tickets-2026-08-28.json",
		UserID:             restoreTestUser,
		IssuedAt:           now,
		MinExecuteAfter:    now.Add(-time.Second), // cooldown already elapsed
		ConfirmationPhrase: "restore tickets from tickets-2026-08-28.json",
	}
	return h, token, secret
}

func executeRestore(t *testing.T, h *TicketMigrationHandler, token, phrase, code string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(restoreExecuteRequest{Token: token, TOTPCode: code, ConfirmationPhrase: phrase})
	r := httptest.NewRequest(http.MethodPost, "/api/admin/tickets/restore/execute", bytes.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), "userID", restoreTestUser))
	r = adminReq(r)
	w := httptest.NewRecorder()
	h.ExecuteRestore(w, r)
	return w
}

func (h *TicketMigrationHandler) hasToken(token string) bool {
	h.tokensMu.Lock()
	defer h.tokensMu.Unlock()
	_, ok := h.tokens[token]
	return ok
}

// One token must buy exactly one TOTP attempt.
//
// The token used to be deleted only after a SUCCESSFUL restore, so a wrong code
// cost nothing: the token lives five minutes, this route has no rate limiter,
// and totp.Validate keeps no attempt state. A stolen admin session could stand
// there guessing six digits - which is precisely what the 2FA gate on a
// wipe-every-ticket-table action exists to stop.
func TestAWrongTOTPCodeSpendsTheRestoreToken(t *testing.T) {
	h, token, _ := newRestoreTest(t)
	phrase := h.tokens[token].ConfirmationPhrase

	w := executeRestore(t, h, token, phrase, "000000")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if h.hasToken(token) {
		t.Error("the token survived a wrong 2FA code: it can be guessed against until it expires")
	}

	// And the spent token is genuinely unusable, not merely marked.
	if w := executeRestore(t, h, token, phrase, "111111"); w.Code != http.StatusUnauthorized {
		t.Errorf("second attempt status = %d, want 401", w.Code)
	}
}

// The checks BEFORE the credential are typos, not guesses. Burning the token on
// them would only train people to click through the confirmation faster.
func TestATypoBeforeTheCodeDoesNotSpendTheToken(t *testing.T) {
	h, token, secret := newRestoreTest(t)

	if w := executeRestore(t, h, token, "restore tickets from something-else.json", "000000"); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong phrase status = %d, want 400", w.Code)
	}
	if !h.hasToken(token) {
		t.Fatal("a mistyped confirmation phrase spent the token")
	}

	// The cooldown is the same kind of check.
	h.tokens[token].MinExecuteAfter = time.Now().Add(time.Minute)
	phrase := h.tokens[token].ConfirmationPhrase
	if w := executeRestore(t, h, token, phrase, "000000"); w.Code != http.StatusTooEarly {
		t.Fatalf("cooldown status = %d, want 425", w.Code)
	}
	if !h.hasToken(token) {
		t.Fatal("hitting the cooldown spent the token")
	}

	// With the cooldown elapsed and the right code, the token is spent and the
	// handler moves on past the gate (it fails later on storage, which this test
	// deliberately does not provide - reaching that far is the assertion).
	h.tokens[token].MinExecuteAfter = time.Now().Add(-time.Second)
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	w := executeRestore(t, h, token, phrase, code)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("a valid code was rejected: %s", w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (past the gate, stopped at unconfigured storage): %s", w.Code, w.Body.String())
	}
	if h.hasToken(token) {
		t.Error("the token survived a completed attempt")
	}
}
