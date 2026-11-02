package handlers

import (
	"net/http"

	"dylaris-core/handlers/reauth"
	"dylaris-core/services"
	"dylaris-core/store"
)

// Exported for the handlers/<domain> subpackages (packs, tickets, ...), which
// live in their own packages so that a change there does not recompile all of
// handlers. Thin wrappers rather than renames: the unexported names stay what
// the rest of this package calls.

// MayReceive reports whether the caller of r may see a system event. Exported
// so the packs tests can prove their events reach only the pack owner.
func (h *SystemEventsHandler) MayReceive(r *http.Request, payload string) bool {
	return h.mayReceive(r, payload)
}

func SendConnTestFailure(w http.ResponseWriter, check services.ConnCheck) {
	sendConnTestFailure(w, check)
}

func SettingOrUnset(st store.Store, key string) (string, error) { return settingOrUnset(st, key) }

// --- gateway -----------------------------------------------------------------

func (s *AppState) GatewayEnabled() bool { return s.gatewayEnabled() }

func GatewayDisabledResponse(w http.ResponseWriter) { gatewayDisabledResponse(w) }

// --- users -------------------------------------------------------------------

// AdminReauthRequest is embedded in the admin account-action bodies; the alias
// keeps the JSON shape ({"reauth": {...}}) exactly as it was.
type ()

func NotifyPasswordChanged(state *AppState, email, username string) {
	notifyPasswordChanged(state, email, username)
}

// SetAccountMailSender replaces the account notice sender for a test and
// returns the restore func. Production never calls it.
func SetAccountMailSender(fn func(services.MailStore, string, string, string, map[string]string) error) (restore func()) {
	prev := sendAccountMail
	sendAccountMail = fn
	return func() { sendAccountMail = prev }
}

var DefaultPlacementSettings = defaultPlacementSettings

// ReauthCodeVerifier is the second-factor check re-authentication hands to
// reauth.RequireAdminReauth / RequireReauth, for the subpackages.
func ReauthCodeVerifier(state *AppState) reauth.CodeVerifier { return reauthCodeVerifier(state) }
