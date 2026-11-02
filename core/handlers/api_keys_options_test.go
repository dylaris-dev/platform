package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"dylaris-core/models"
)

// The whitelist is a NARROWING filter, so an entry that no key could carry
// narrows nothing. Dropping such an entry keeps the field editable after a
// capability is renamed; refusing the save would make it impossible to fix.
func TestSanitizeKeyCapList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trims and keeps order", " console.send , rcon.exec ", "console.send,rcon.exec"},
		{"drops duplicates", "rcon.exec,rcon.exec", "rcon.exec"},
		{"drops unknown ids", "rcon.exec,not.a.cap", "rcon.exec"},
		// A PANEL capability is rejected on a key by authz.ValidKeyCap, so
		// listing one here could only ever mislead the operator into thinking it
		// was permitted.
		{"drops panel capabilities", "rcon.exec,users.write", "rcon.exec"},
		{"empty stays empty, which means no restriction", "  ", ""},
		{"a list of only invalid ids collapses to no restriction", "nope,users.write", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeKeyCapList(c.in); got != c.want {
				t.Errorf("sanitizeKeyCapList(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The panel asks the backend what it may mint rather than deciding locally, so
// the page never offers a choice Create would refuse.
func TestAPIKeysList_ReportsWhatTheCallerMayMint(t *testing.T) {
	listOptions := func(t *testing.T, enabled, caps string, isAdmin bool) apiKeyOptions {
		t.Helper()
		fs := &apiKeysAuthFakeStore{settings: map[string]string{"apikeys_user_enabled": enabled}}
		if caps != "" {
			fs.settings["apikeys_user_allowed_caps"] = caps
		}
		h := newAPIKeysAuthHandler(fs)

		ctx := context.WithValue(context.Background(), "userID", "u1")
		ctx = context.WithValue(ctx, "isAdmin", isAdmin)
		rec := httptest.NewRecorder()
		h.List(rec, httptest.NewRequest("GET", "/api/me/api-keys", nil).WithContext(ctx))

		var body struct {
			Options apiKeyOptions `json:"options"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
		}
		return body.Options
	}

	t.Run("off for a user when the operator has not enabled it", func(t *testing.T) {
		if got := listOptions(t, "false", "", false); got.Enabled {
			t.Error("enabled = true, want false: the page would offer a create that Create refuses")
		}
	})

	t.Run("on for an admin regardless of the switch", func(t *testing.T) {
		if got := listOptions(t, "false", "", true); !got.Enabled {
			t.Error("enabled = false for an admin: nobody could turn the feature on for anyone")
		}
	})

	// Null and empty are different answers: null is "the operator set no
	// whitelist", which the panel must render as the full picker. An empty array
	// would read as "nothing allowed" and show an empty one on every default
	// install.
	t.Run("an unset whitelist is reported as null, not as an empty list", func(t *testing.T) {
		if got := listOptions(t, "true", "", false); got.AllowedCaps != nil {
			t.Errorf("allowedCaps = %v, want nil", got.AllowedCaps)
		}
	})

	t.Run("a set whitelist is reported verbatim", func(t *testing.T) {
		got := listOptions(t, "true", "rcon.exec,console.read", false)
		if len(got.AllowedCaps) != 2 || got.AllowedCaps[0] != "rcon.exec" {
			t.Errorf("allowedCaps = %v, want the operator list", got.AllowedCaps)
		}
	})

	// An admin is not subject to the whitelist, so reporting one would hide
	// capabilities they are entitled to mint.
	t.Run("an admin is not narrowed by the whitelist", func(t *testing.T) {
		if got := listOptions(t, "true", "rcon.exec", true); got.AllowedCaps != nil {
			t.Errorf("allowedCaps = %v, want nil for an admin", got.AllowedCaps)
		}
	})
}

func (f *apiKeysAuthFakeStore) ListAPIKeysByUser(userID string) ([]models.APIKey, error) {
	return f.listedKeys, nil
}
