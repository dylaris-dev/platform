package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"dylaris-core/handlers/httpx"
)

type SecurityQuestionsHandler struct {
	state *AppState
}

func NewSecurityQuestionsHandler(state *AppState) *SecurityQuestionsHandler {
	return &SecurityQuestionsHandler{state: state}
}

// defaultSecurityQuestionPool is what every new install starts with. Admins
// can edit / replace via the admin settings UI. The strings are kept English-
// only on purpose — matches the platform's English-everywhere rule.
var defaultSecurityQuestionPool = []string{
	"What is the name of your first pet?",
	"In what city were you born?",
	"What was the name of your first school?",
	"What is your mother's maiden name?",
	"What is the name of your favorite childhood teacher?",
	"What was the model of your first car?",
	"What was your childhood nickname?",
	"What is the name of the street you grew up on?",
	"What is your favorite book?",
	"What was the name of your first stuffed animal?",
}

// LoadSecurityQuestionPool reads the admin-managed pool, falling back to
// defaults when the setting hasn't been customized yet. Returned slice is
// never nil and never empty (defaults always provide at least 10 entries).
func LoadSecurityQuestionPool(state *AppState) []string {
	raw, _ := state.Store.GetSetting("auth.security_questions_pool")
	if raw == "" {
		return append([]string(nil), defaultSecurityQuestionPool...)
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil || len(out) == 0 {
		return append([]string(nil), defaultSecurityQuestionPool...)
	}
	return out
}

// securityQARequest is the wire shape for setting questions — plaintext
// answers, server hashes them before storage. Question text is sent verbatim
// so the client controls exact wording (matters when admins edit the pool
// after a user has already set up).
type securityQARequest struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// GetPool GET /api/auth/security-questions/pool — public.
// Returns the question strings only. Frontend uses this on /register and
// /reset-password to render the pickers.
func (h *SecurityQuestionsHandler) GetPool(w http.ResponseWriter, r *http.Request) {
	policy := LoadAuthPolicy(h.state)
	if !policy.SecurityQuestionsEnabled {
		// Feature off — return empty pool so the frontend can simply skip
		// the picker without a hard error.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  true,
			"enabled":  false,
			"pool":     []string{},
			"required": 0,
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"enabled":  true,
		"pool":     LoadSecurityQuestionPool(h.state),
		"required": policy.SecurityQuestionsCount,
	})
}

// GetMyQuestions GET /api/me/security-questions — auth required.
// Returns the current user's chosen questions (texts only). Used by the
// profile UI to show "your questions are X, Y, Z" before editing.
func (h *SecurityQuestionsHandler) GetMyQuestions(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	questions, err := h.state.Store.GetUserSecurityQuestions(userID)
	if err != nil {
		httpx.SendJSONError(w, "Failed to load questions", http.StatusInternalServerError)
		return
	}
	if questions == nil {
		questions = []string{}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"questions": questions,
	})
}

type setMyQuestionsRequest struct {
	Items []securityQARequest `json:"items"`
	// These answers ARE the password-reset path, so overwriting them survives
	// a password change and even revoking every API key. See requireReauth.
	reauthFields
}

// SetMyQuestions PUT /api/me/security-questions — auth required.
// Replaces the user's question set in one shot. Validates: count matches
// policy, every question is from the current pool (so users can't slip a
// custom self-known one in), no duplicates.
func (h *SecurityQuestionsHandler) SetMyQuestions(w http.ResponseWriter, r *http.Request) {
	userID, _ := r.Context().Value("userID").(string)
	if userID == "" {
		httpx.SendJSONError(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	policy := LoadAuthPolicy(h.state)
	if !policy.SecurityQuestionsEnabled {
		httpx.SendJSONError(w, "Security questions are disabled by the administrator", http.StatusForbidden)
		return
	}

	var req setMyQuestionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	hashedJSON, err := buildHashedQAJSON(req.Items, LoadSecurityQuestionPool(h.state), policy.SecurityQuestionsCount)
	if err != nil {
		httpx.SendJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Re-authenticate immediately before the write, and after every cheap check.
	// A stolen session that can rewrite these owns the recovery path from then
	// on, whatever the owner does to their password afterwards.
	//
	// Last rather than first for the same reason as APIKeysHandler.Create: a
	// bcrypt comparison ahead of the request validation would let a malformed
	// body buy CPU time, and nothing above this line writes anything.
	if rerr := requireReauth(h.state, userID, req.Password, req.Code); rerr != nil {
		writeReauthError(w, rerr)
		return
	}
	if err := h.state.Store.SetUserSecurityQuestions(userID, hashedJSON); err != nil {
		httpx.SendJSONError(w, "Failed to save", http.StatusInternalServerError)
		return
	}

	LogIdentityAudit(h.state, r, AuditEventSecurityQuestionsSet, userID, userID, map[string]interface{}{
		"count": len(req.Items),
	})

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// GetAdminPool GET /api/admin/settings/security-questions-pool - PANEL settings.read
// (RequireCap at the route).
func (h *SecurityQuestionsHandler) GetAdminPool(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"pool":    LoadSecurityQuestionPool(h.state),
	})
}

type setAdminPoolRequest struct {
	Pool []string `json:"pool"`
}

// SetAdminPool PUT /api/admin/settings/security-questions-pool — admin only.
// Replaces the question pool. Existing users' chosen questions don't change
// — they keep their original wording in the JSON column. Only new picks are
// constrained to the updated pool.
func (h *SecurityQuestionsHandler) SetAdminPool(w http.ResponseWriter, r *http.Request) {
	var req setAdminPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.SendJSONError(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	cleaned := make([]string, 0, len(req.Pool))
	seen := map[string]bool{}
	for _, q := range req.Pool {
		q = strings.TrimSpace(q)
		if q == "" || len(q) > 200 {
			continue
		}
		if seen[strings.ToLower(q)] {
			continue
		}
		seen[strings.ToLower(q)] = true
		cleaned = append(cleaned, q)
	}
	if len(cleaned) < 3 {
		httpx.SendJSONError(w, "Pool must contain at least 3 distinct questions", http.StatusBadRequest)
		return
	}

	payload, _ := json.Marshal(cleaned)
	actorID, _ := r.Context().Value("userID").(string)
	if err := h.state.Store.SetSettingBy("auth.security_questions_pool", string(payload), actorID); err != nil {
		httpx.SendJSONError(w, "Failed to save pool", http.StatusInternalServerError)
		return
	}

	LogIdentityAudit(h.state, r, AuditEventSecurityQuestionsPoolUpdated, actorID, "", map[string]interface{}{
		"count": len(cleaned),
	})

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"pool":    cleaned,
	})
}

// buildHashedQAJSON validates incoming items against pool + count constraints,
// bcrypts the answers, and returns the JSON blob ready for SetUserSecurityQuestions.
// Returns an error message safe to surface to the caller (no internal details).
func buildHashedQAJSON(items []securityQARequest, pool []string, required int) (string, error) {
	if len(items) != required {
		return "", &simpleErr{msg: "Please choose exactly the required number of questions"}
	}
	poolSet := make(map[string]bool, len(pool))
	for _, p := range pool {
		poolSet[p] = true
	}
	seen := map[string]bool{}
	type stored struct {
		Question   string `json:"question"`
		AnswerHash string `json:"answer_hash"`
	}
	out := make([]stored, 0, len(items))
	for _, it := range items {
		q := strings.TrimSpace(it.Question)
		a := strings.TrimSpace(it.Answer)
		if q == "" || a == "" {
			return "", &simpleErr{msg: "Each question must have a non-empty answer"}
		}
		if len(a) > 200 {
			return "", &simpleErr{msg: "Answer is too long (max 200 characters)"}
		}
		if !poolSet[q] {
			return "", &simpleErr{msg: "One of the chosen questions is not in the current pool"}
		}
		if seen[q] {
			return "", &simpleErr{msg: "Each question may be chosen at most once"}
		}
		seen[q] = true
		hash, err := bcrypt.GenerateFromPassword([]byte(strings.ToLower(a)), bcrypt.DefaultCost)
		if err != nil {
			return "", &simpleErr{msg: "Failed to hash answer"}
		}
		out = append(out, stored{Question: q, AnswerHash: string(hash)})
	}
	js, _ := json.Marshal(out)
	return string(js), nil
}

// verifySecurityAnswersAgainstStored compares plaintext answers (in the SAME
// order as the stored questions) against the stored bcrypt hashes. Returns
// true only if every answer matches. Used by the password-reset flow.
func verifySecurityAnswersAgainstStored(storedJSON string, plainAnswers []string) bool {
	var stored []struct {
		Question   string `json:"question"`
		AnswerHash string `json:"answer_hash"`
	}
	if err := json.Unmarshal([]byte(storedJSON), &stored); err != nil {
		return false
	}
	if len(stored) == 0 || len(stored) != len(plainAnswers) {
		return false
	}
	for i, s := range stored {
		a := strings.TrimSpace(strings.ToLower(plainAnswers[i]))
		if a == "" {
			return false
		}
		if bcrypt.CompareHashAndPassword([]byte(s.AnswerHash), []byte(a)) != nil {
			return false
		}
	}
	return true
}

// simpleErr lets the handler surface validation messages without dragging
// in fmt.Errorf for every guard.
type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }
