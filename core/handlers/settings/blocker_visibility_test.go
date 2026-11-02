package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"
)

// A settings screen must not be able to disagree with what would really happen.
//
// Both cases here are a switch that promises something the platform cannot
// currently deliver, and in both the operator's own screen was the last place
// that could tell them. Mail is the worse of the two: the password-reset
// endpoint answers success whether or not a message went out - deliberately, so
// it cannot be used to test whether an address has an account - so the person
// asking learns nothing and the operator learns nothing either.

type blockerFakeStore struct {
	store.Store
	settings   map[string]string
	categories []models.TicketCategory
	catErr     error
}

func (f *blockerFakeStore) GetSetting(key string) (string, error) {
	return f.settings[key], nil
}

func (f *blockerFakeStore) ListTicketCategories(includeDisabled bool) ([]models.TicketCategory, error) {
	return f.categories, f.catErr
}

func (f *blockerFakeStore) CountUsersMissingSecurityQuestions() (int, int, error) {
	return 0, 0, nil
}

func authPolicyBody(t *testing.T, st store.Store) map[string]interface{} {
	t.Helper()
	h := &AuthSettingsHandler{d: withCoreStorage(Deps{Store: st})}
	w := httptest.NewRecorder()
	h.GetAuthPolicy(w, httptest.NewRequest(http.MethodGet, "/api/admin/settings/auth", nil))
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// The answer comes from loading the transport a real send loads, so a
// configuration that would not deliver cannot report itself as configured.
// Production on 2026-09-03 held zero smtp.* settings, which is the first case.
func TestAuthPolicySaysWhetherMailCanActuallyBeSent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]string
		want     bool
	}{
		{"nothing configured at all", map[string]string{}, false},
		{
			// A host and no sender is the half-finished state an operator
			// leaves behind mid-setup, and it cannot send.
			name:     "host but no from address",
			settings: map[string]string{"smtp.default.host": "smtp.example.com"},
			want:     false,
		},
		{
			name: "complete SMTP profile",
			settings: map[string]string{
				"smtp.default.host":       "smtp.example.com",
				"smtp.default.from_email": "noreply@example.com",
			},
			want: true,
		},
		{
			// The auth profile is what password reset and verification use, so
			// a deployment that configured only that one is configured for the
			// switches on this card.
			name: "only the auth profile",
			settings: map[string]string{
				"smtp.auth.host":       "smtp.example.com",
				"smtp.auth.from_email": "noreply@example.com",
			},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := authPolicyBody(t, &blockerFakeStore{settings: tc.settings})
			got, ok := out["mailConfigured"].(bool)
			if !ok {
				t.Fatalf("mailConfigured missing from the reply: %v", out)
			}
			if got != tc.want {
				t.Errorf("mailConfigured = %v, want %v", got, tc.want)
			}
		})
	}
}

func featuresBody(t *testing.T, st store.Store) map[string]interface{} {
	t.Helper()
	h := &FeatureSettingsHandler{d: withCoreStorage(Deps{Store: st, FeatureFlags: services.NewFeatureFlags(st)})}
	w := httptest.NewRecorder()
	h.Get(w, httptest.NewRequest(http.MethodGet, "/api/admin/settings/features", nil))
	var out map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// Zero enabled categories means the ticket module accepts nothing: a ticket
// must name one. Production held zero while the flag was off, so turning it on
// would have shipped a support system that rejects every customer.
func TestFeaturesCountsTheEnabledTicketCategories(t *testing.T) {
	out := featuresBody(t, &blockerFakeStore{})
	if n, ok := out["enabledTicketCategories"].(float64); !ok || n != 0 {
		t.Fatalf("enabledTicketCategories = %v, want 0", out["enabledTicketCategories"])
	}

	out = featuresBody(t, &blockerFakeStore{categories: []models.TicketCategory{{ID: 1}, {ID: 2}}})
	if n, _ := out["enabledTicketCategories"].(float64); n != 2 {
		t.Fatalf("enabledTicketCategories = %v, want 2", out["enabledTicketCategories"])
	}
}

// A count that cannot be read must be ABSENT rather than zero. Zero is the
// warning state, and inventing it from a failed read would put a warning on the
// screen that describes nothing.
func TestAnUnreadableCategoryCountIsOmittedRatherThanZero(t *testing.T) {
	out := featuresBody(t, &blockerFakeStore{catErr: errBlockerTest})
	if _, present := out["enabledTicketCategories"]; present {
		t.Fatalf("enabledTicketCategories present on a failed read: %v", out["enabledTicketCategories"])
	}
}

type blockerErr struct{}

func (blockerErr) Error() string { return "boom" }

var errBlockerTest = blockerErr{}
