package handlers

import (
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/services/redisacl"
	"dylaris-core/store"
	"dylaris-pkg/validate"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"golang.org/x/crypto/bcrypt"
)

type UserHandler struct {
	state *AppState
}

func NewUserHandler(state *AppState) *UserHandler {
	return &UserHandler{state: state}
}

// GetAllUsers GET /api/users - every account.
func (h *UserHandler) GetAllUsers(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	users, err := h.state.Store.ListUsers()
	if err != nil {
		sendJSONError(w, "Failed to fetch users", 500)
		return
	}

	for i := range users {
		users[i].Password = ""
	}

	if users == nil {
		users = []models.User{}
	}

	// FIX: Return as object
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"users":   users,
	})
}

// createUserRequest is the wire-level payload — superset of models.User with
// optional region-access fields. Kept separate from models.User so we can
// add registration-specific fields in 0a.2+ without polluting the model.
type createUserRequest struct {
	models.User
	// Password shadows models.User.Password, which is json:"-" so that a user
	// row can never be serialised back to a client with its bcrypt hash on it.
	// That tag also applies on the way IN, which silently made this the one
	// field an admin could not send: every create arrived with an empty
	// password and was refused with "Password is required". Declaring it here
	// keeps the hash off every response and still lets the create payload
	// carry one. Plaintext on the way in, hashed into req.User.Password below
	// - never assign this field to the model directly.
	Password string `json:"password"`
	adminReauthRequest
	// Region access. If the caller omits both fields, the new
	// user defaults to all-regions access — matches the grandfather behavior
	// applied to existing users at migration time, and avoids creating users
	// who can see nothing.
	AllRegions      *bool    `json:"allRegions,omitempty"`
	RegionsExplicit []string `json:"regionsExplicit,omitempty"`
}

// createsPrivilegedAccount reports whether this create hands out power rather
// than just making an account.
//
// Every field that would need re-authentication to set AFTERWARDS counts here,
// or creation becomes the way around those checks: is_admin and the admin or
// support role are the two that reach the panel, and the flags are the ones
// SetUserPermissions gates. A panel role cannot be assigned at creation, so it
// has nothing to check for.
func createsPrivilegedAccount(req createUserRequest) bool {
	return req.User.IsAdmin ||
		req.User.Role == "admin" || req.User.Role == "support" ||
		req.User.CanDeleteServers || req.User.CanChangeResources
}

// CreateUser POST /api/users - creates an account and assigns its regions. A
// region assignment that fails is logged but does not fail the creation, so
// the account still exists afterwards.
func (h *UserHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	// The admin create-user path historically validated neither the username nor
	// the password. A username is interpolated into Redis keys (the beam daily
	// counter dylaris:beam:daily:<username>), so a ':'/space must be rejected here
	// exactly like the register + rename paths.
	req.Username = strings.TrimSpace(req.Username)
	if !validate.IsUsername(req.Username) {
		sendJSONError(w, "Invalid username: 3-32 characters, must start with a letter or digit, then letters, digits, '.', '_' or '-'", 400)
		return
	}
	// The fourth door onto a username, and the only one that never checked at
	// all - it leaned entirely on the DB constraint, which was case-SENSITIVE,
	// so an admin could create `NewComer` beside an existing `newcomer`. The
	// unique index on LOWER(username) is the real guard now; this is the
	// message that says which name the collision is with.
	if taken, _ := h.state.Store.UsernameTaken(req.Username, ""); taken {
		sendJSONError(w, "Username is taken", 409)
		return
	}
	if req.Password == "" {
		sendJSONError(w, "Password is required", 400)
		return
	}
	// There is no unique index on users.email, and a second account on an
	// existing address makes that address's password reset pick either row.
	if email := strings.TrimSpace(req.Email); email != "" {
		if existing, eerr := h.state.Store.GetUserByEmail(email); eerr == nil && existing != nil {
			sendJSONError(w, "Email is already in use", http.StatusConflict)
			return
		}
	}
	// The body decodes into models.User, whose isAdmin is written as sent.
	if req.User.IsAdmin && !IsAdmin(r) {
		sendJSONError(w, "Only an admin can create an admin", http.StatusForbidden)
		return
	}
	// Enforce the same password-length policy the register + reset paths apply.
	if min := LoadAuthPolicy(h.state).PasswordMinLength; len(req.Password) < min {
		sendJSONError(w, fmt.Sprintf("Password must be at least %d characters", min), 400)
		return
	}
	// A PRIVILEGED account is durable access that outlives the session creating
	// it, so the administrator proves who they are. An ordinary account is not:
	// it holds nothing until somebody grants it something, and every route that
	// grants is gated the same way. Gating routine user creation as well would
	// train operators to type their password without reading the dialog.
	if createsPrivilegedAccount(req) && !requireAdminReauth(w, r, h.state, req.adminReauthRequest) {
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("CreateUser: bcrypt hashing failed for username=%q: %v", req.Username, err)
		sendJSONError(w, "Could not create user", 500)
		return
	}
	// req.User, not req: CreateUser persists the embedded model, and the
	// shadowing field above is only the wire's plaintext.
	req.User.Password = string(hashed)
	// The admin vouches for an account they create, and no verification mail is
	// ever sent for it. Overwritten rather than trusted, since the body decodes
	// into the model and could carry any timestamp.
	verifiedAt := time.Now()
	req.User.EmailVerifiedAt = &verifiedAt

	if err := h.state.Store.CreateUser(&req.User); err != nil {
		log.Printf("CreateUser failed for username=%q: %v", req.Username, err)
		sendJSONError(w, "Could not create user", 409)
		return
	}

	// Default to all-regions when caller hasn't specified — preserves
	// previous behavior where every user had implicit global access.
	allRegions := true
	regionsExplicit := []string{}
	if req.AllRegions != nil {
		allRegions = *req.AllRegions
	}
	if req.RegionsExplicit != nil {
		regionsExplicit = req.RegionsExplicit
	}
	if err := h.state.Store.SetUserRegions(req.User.ID, allRegions, regionsExplicit); err != nil {
		// Region setup failed but the user already exists — log but don't
		// roll back; admin can fix the assignment from the user settings panel.
		log.Printf("CreateUser: SetUserRegions failed for userID=%s: %v", req.User.ID, err)
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "User created",
		"userId":  req.User.ID,
	})
}

// CancelUserDeletion POST /api/admin/users/{id}/cancel-deletion
// Admin override: clears the pending_deletion stamps and returns the user
// to active state. Useful when an admin wants to save an account before
// the user logs in themselves. Idempotent on already-active users.
func (h *UserHandler) CancelUserDeletion(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}
	id, ok := parseUserID(w, r)
	if !ok {
		return
	}
	if target, terr := h.state.Store.GetUserByID(id); terr != nil || target == nil || !mayManageAccount(h.state, r, target) {
		sendJSONError(w, "You cannot change an account with more rights than yours", http.StatusForbidden)
		return
	}
	if err := h.state.Store.CancelUserDeletion(id); err != nil {
		sendJSONError(w, "Failed to cancel deletion", 500)
		return
	}
	actorID, _ := r.Context().Value("userID").(string)
	LogIdentityAudit(h.state, r, AuditEventDeletionCancelledByAdmin, actorID, id, nil)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// otherAdminExists reports whether any admin other than excludeID exists. The
// error is kept rather than collapsed: "we could not count them" and "there are
// none" call for opposite decisions at the one place this is asked.
func (h *UserHandler) otherAdminExists(excludeID string) (bool, error) {
	users, err := h.state.Store.ListUsers()
	if err != nil {
		return false, err
	}
	for _, u := range users {
		if u.IsAdmin && u.ID != excludeID {
			return true, nil
		}
	}
	return false, nil
}

// DeleteUser DELETE /api/users/{id} - deletes an account. A user still
// referenced elsewhere, for instance by server invites they issued, answers
// 409 with that reason rather than a bare 500.
func (h *UserHandler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	if h.state.Store == nil {
		sendJSONError(w, "Database not connected", 503)
		return
	}

	id, ok := parseUserID(w, r)
	if !ok {
		return
	}

	currentUser := r.Context().Value("username").(string)
	userToDelete, err := h.state.Store.GetUserByID(id)

	if err == nil && userToDelete.Username == currentUser {
		sendJSONError(w, "You cannot delete yourself", 403)
		return
	}
	if !IsAdmin(r) && (err != nil || userToDelete == nil || !mayManageAccount(h.state, r, userToDelete)) {
		sendJSONError(w, "You cannot delete an account with more rights than yours", 403)
		return
	}

	// Never let the platform reach zero admins. "You cannot delete yourself"
	// does not cover it: users.delete is a delegatable capability, so a
	// non-admin holding it could remove every admin and lock the owner out of
	// their own panel with no way back in short of the database.
	if err == nil && userToDelete != nil && userToDelete.IsAdmin {
		others, cerr := h.otherAdminExists(userToDelete.ID)
		if cerr != nil {
			// Refuse when the answer is unknown. Guessing "yes" here is the one
			// direction that is unrecoverable.
			sendJSONError(w, "Could not verify how many admins remain", 503)
			return
		}
		if !others {
			sendJSONError(w, "This is the last admin. Make someone else an admin first.", 409)
			return
		}
	}

	// Everything the account HOLDS, before the row that owns it goes: its
	// route-only link kits (durable revoke, tunnel key) and its
	// protected addresses.
	//
	// This used to be an inline loop over the addresses only, and the auto-delete
	// sweep - which is what removes an account in practice - did none of it. The
	// shared function is the point: three paths remove an account and they were
	// each answering "how much of this is my problem" differently. See
	// services.TeardownTenantInfrastructure for what each part leaves behind when
	// it is skipped.
	//
	// Done BEFORE the user row goes, so a failure here leaves the account intact
	// and the operator can retry: deleting the user first would strand all of it
	// with no owner to look it up by.
	if err := services.TeardownTenantInfrastructure(r.Context(), h.state.Store, h.state.Gateway,
		h.state.Redis, redisacl.NewProvisioner(h.state.Redis), h.state.WarpPeers, id); err != nil {
		log.Printf("delete user %s: teardown: %v", id, err)
		// A precondition the operator can fix is not a server error, and the
		// sentence naming what is in the way is the whole value of the refusal.
		var owns *services.TenantStillOwnsServersError
		if errors.As(err, &owns) {
			sendJSONError(w, owns.Error()+" Nothing was deleted.", http.StatusConflict)
			return
		}
		sendJSONError(w, "Could not remove what this account still holds. Nothing was deleted.", 500)
		return
	}

	if err := h.state.Store.DeleteUser(id); err != nil {
		// These two are not faults, they are the current state of the data, so
		// they answer 409 and say what to do about it. Postgres already produces
		// a precise reason ("still referenced from table servers"); collapsing it
		// into "Delete failed" left an admin with a button that does nothing and
		// no way to find out why.
		switch {
		case errors.Is(err, store.ErrUserOwnsServers):
			sendJSONError(w, "This user still owns servers. Transfer or delete their servers first.", 409)
		case errors.Is(err, store.ErrUserStillReferenced):
			sendJSONError(w, "This user is still referenced by other records (for example server invites they issued) and cannot be deleted yet.", 409)
		default:
			sendJSONError(w, "Delete failed", 500)
		}
		return
	}

	// The only record that this account ever existed.
	//
	// This path wrote nothing at all, while the auto-delete sweep beside it has
	// always written one - so the removals an OPERATOR performs, which is most
	// of them, left no trace. Measured the hard way: an account deleted here by
	// mistake could only be reconstructed from its own registration row, and
	// only because registration happens to record the username and address.
	//
	// Written with NO target id, and that is the whole difficulty.
	// audit_events_identity.target_user_id is a foreign key onto users: the
	// account is gone by this line, so a row pointing at it is refused outright
	// (23503), and LogIdentityAudit swallows the error - the row would simply
	// never appear. ON DELETE SET NULL blanks the target of rows that already
	// EXIST; it does nothing for an insert that arrives afterwards. Measured on
	// production: the first version of this logged
	// "violates foreign key constraint audit_events_identity_target_user_id_fkey"
	// and wrote nothing, and the unit test could not see it because a fake
	// store has no foreign keys.
	//
	// So the identity lives in the metadata, which is the only place it can:
	// the id, the username and the address. The identity log already carries an
	// address on user_registered, so this discloses nothing it does not hold.
	actorID, _ := r.Context().Value("userID").(string)
	deleted := map[string]interface{}{"userId": id}
	if userToDelete != nil {
		deleted["username"] = userToDelete.Username
		deleted["email"] = userToDelete.Email
	}
	LogIdentityAudit(h.state, r, AuditEventUserHardDeleted, actorID, "", deleted)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// ResetUserPassword PUT /api/users/{id}/password - sets a new password for an
// account, hashed before it is stored. No current password is asked for; this
// is the admin path, not the self-service one.
func (h *UserHandler) ResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUserID(w, r)
	if !ok {
		return
	}

	var req struct {
		Password string `json:"password"`
		adminReauthRequest
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		sendJSONError(w, "Password is required", 400)
		return
	}
	target, terr := h.state.Store.GetUserByID(id)
	if terr != nil || target == nil {
		sendJSONError(w, "User not found", http.StatusNotFound)
		return
	}
	if !mayManageAccount(h.state, r, target) {
		sendJSONError(w, "You cannot change the password of an account with more rights than yours", http.StatusForbidden)
		return
	}
	if min := LoadAuthPolicy(h.state).PasswordMinLength; len(req.Password) < min {
		sendJSONError(w, fmt.Sprintf("Password must be at least %d characters", min), 400)
		return
	}

	// Setting somebody else's password is taking their account over, so the
	// question is not what this session may do but whether the administrator is
	// the one asking. Last, after every cheap check and before the hash.
	if !requireAdminReauth(w, r, h.state, req.adminReauthRequest) {
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		sendJSONError(w, "Failed to hash password", 500)
		return
	}

	if err := h.state.Store.UpdateUserPassword(id, string(hashed)); err != nil {
		sendJSONError(w, "Failed to update password", 500)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "Password updated"})
}

// GetUserRouteLimit GET /api/users/{id}/route-limit - one user's gateway route
// allowance: default when no override exists, otherwise custom or disabled.
func (h *UserHandler) GetUserRouteLimit(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	scope := fmt.Sprintf("user:%s", id)

	limit, err := h.state.Store.GetGatewayRouteLimit(scope)
	if err != nil || limit == nil {
		// No row at all: this user defers to the platform default.
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"mode":      "default",
			"maxRoutes": nil,
		})
		return
	}

	// Four states, and every one of them is now expressible. A NULL cap is
	// "unlimited, decided for this user" - different from "default", which is
	// deciding nothing and letting the platform answer.
	mode := "custom"
	switch {
	case limit.MaxRoutes == nil:
		mode = "unlimited"
	case *limit.MaxRoutes == 0:
		mode = "disabled"
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"mode":      mode,
		"maxRoutes": limit.MaxRoutes,
	})
}

// SetUserRouteLimit PUT /api/users/{id}/route-limit - sets a user's gateway
// route allowance to default, custom with a count, or disabled.
func (h *UserHandler) SetUserRouteLimit(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	id := vars["id"]
	scope := fmt.Sprintf("user:%s", id)
	// A route limit is a quota. Nobody but an admin sets their own, and staff
	// set it only on accounts they may manage.
	if !IsAdmin(r) {
		actorID, _ := r.Context().Value("userID").(string)
		target, terr := h.state.Store.GetUserByID(id)
		if actorID == id || terr != nil || target == nil || !mayManageAccount(h.state, r, target) {
			sendJSONError(w, "You cannot change this account's route limit", http.StatusForbidden)
			return
		}
	}

	var req struct {
		Mode      string `json:"mode"`
		MaxRoutes int    `json:"maxRoutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, "Invalid JSON", 400)
		return
	}

	zero := 0
	switch req.Mode {
	case "default":
		// No row: defer to user_default, then global.
		h.state.Store.DeleteGatewayRouteLimit(scope)
	case "unlimited":
		// A row with a NULL cap. Deliberately NOT the same as "default": this
		// says "no cap, for this user", and keeps saying it if an operator later
		// tightens the platform default.
		h.state.Store.SetGatewayRouteLimit(scope, nil)
	case "custom":
		if req.MaxRoutes < 1 {
			sendJSONError(w, "Custom limit must be at least 1 (use mode \"disabled\" for none)", 400)
			return
		}
		h.state.Store.SetGatewayRouteLimit(scope, &req.MaxRoutes)
	case "disabled":
		h.state.Store.SetGatewayRouteLimit(scope, &zero)
	default:
		sendJSONError(w, "Invalid mode (use: default, unlimited, custom, disabled)", 400)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "Route limit updated"})
}
