package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dylaris-core/models"
	"dylaris-core/store"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

type resetAnswerStore struct {
	store.Store
	stored  string
	cleared int
	resets  int
}

func (f *resetAnswerStore) GetSetting(k string) (string, error) {
	switch k {
	case "auth.security_questions_enabled", "auth.security_questions_required_at_reset":
		return "true", nil
	}
	return "", nil
}
func (f *resetAnswerStore) GetUserByPasswordResetToken(string) (*models.User, error) {
	return &models.User{ID: "u1", Username: "alice"}, nil
}
func (f *resetAnswerStore) GetUserSecurityQuestionsRaw(string) (string, error) {
	return f.stored, nil
}
func (f *resetAnswerStore) ClearPasswordResetToken(string) error { f.cleared++; return nil }
func (f *resetAnswerStore) ResetPasswordWithToken(string, string, string) (bool, error) {
	f.resets++
	return true, nil
}
func (f *resetAnswerStore) GetUserByID(string) (*models.User, error) {
	return &models.User{ID: "u1"}, nil
}
func (f *resetAnswerStore) ListServersForUser(string, bool) ([]models.Server, error) { return nil, nil }
func (f *resetAnswerStore) ListAPIKeysByUser(string) ([]models.APIKey, error)        { return nil, nil }
func (f *resetAnswerStore) DeleteAllSSHKeys(string) (int, error)                     { return 0, nil }
func (f *resetAnswerStore) InsertAuditIdentity(*models.AuditEventIdentity) error {
	return nil
}

func resetWithAnswer(h *PasswordResetHandler, answer string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ResetPassword(rec, httptest.NewRequest("POST", "/api/auth/reset-password",
		strings.NewReader(`{"token":"abcdefghijklmnopqrstuvwxyz","password":"a-long-new-password","securityAnswers":["`+answer+`"]}`)))
	return rec
}

// The per-IP limiter was the only bound on guessing security answers, so a
// reset-link holder with many addresses could try them for the link's whole
// life. Past the per-account limit the link is burnt, and a right answer then
// does not help either.
func TestResetSecurityAnswersAreLimitedPerAccount(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("paris"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cases := []struct {
		name       string
		wrongFirst int
		wantCode   int
		wantClear  int
		wantResets int
	}{
		{"right answer after a few misses still resets", resetAnswerLimit - 1, http.StatusOK, 0, 1},
		{"right answer after the limit is refused and the link burnt", resetAnswerLimit, http.StatusGone, 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mr.FlushAll()
			fs := &resetAnswerStore{stored: `[{"question":"City?","answer_hash":"` + string(hash) + `"}]`}
			h := NewPasswordResetHandler(Deps{Store: fs, Redis: rdb})
			for i := 0; i < tc.wrongFirst; i++ {
				if rec := resetWithAnswer(h, "london"); rec.Code != http.StatusUnauthorized {
					t.Fatalf("wrong answer %d: status %d, want 401", i+1, rec.Code)
				}
			}
			rec := resetWithAnswer(h, "paris")
			if rec.Code != tc.wantCode {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if fs.cleared != tc.wantClear || fs.resets != tc.wantResets {
				t.Fatalf("cleared=%d resets=%d, want cleared=%d resets=%d", fs.cleared, fs.resets, tc.wantClear, tc.wantResets)
			}
			// A burnt link takes its counter with it, and a right answer
			// clears it, so neither leaves the next link short of tries.
			if mr.Exists(resetAnswerKey("u1")) {
				t.Fatal("the attempt counter outlived the outcome")
			}
		})
	}
}
