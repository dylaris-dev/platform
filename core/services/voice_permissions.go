package services

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"dylaris-core/models"
	"dylaris-core/store"
)

// ResolveVoiceAllowed answers whether srv may have voice chat turned on, and
// which scope decided it: server:<id> > user:<owner> > role:<owner's panel
// role> > global (the setting VoiceChatSettingKey, default false). A scope
// without a row defers to the next one down.
//
// A read error is returned with allowed=false, and the caller picks the side:
// an enable fails closed on it, the reconcile does NOT revoke on it - a
// database blip must not switch voice off on every server at once.
func ResolveVoiceAllowed(st store.Store, srv *models.Server) (allowed bool, decidedBy string, err error) {
	scopes := []string{"server:" + strconv.Itoa(srv.ID)}
	if srv.OwnerID != "" {
		scopes = append(scopes, "user:"+srv.OwnerID)
		roleID, _, err := st.GetUserPanelAuthz(srv.OwnerID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, "role", fmt.Errorf("read panel role of %s: %w", srv.OwnerID, err)
		}
		if roleID != nil {
			scopes = append(scopes, "role:"+strconv.Itoa(*roleID))
		}
	}
	for _, scope := range scopes {
		v, err := st.GetVoicePermission(scope)
		if err != nil {
			return false, scope, fmt.Errorf("read voice permission %s: %w", scope, err)
		}
		if v != nil {
			return *v, scope, nil
		}
	}
	raw, err := st.GetSetting(VoiceChatSettingKey)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, "global", fmt.Errorf("read %s: %w", VoiceChatSettingKey, err)
	}
	on, _ := strconv.ParseBool(strings.TrimSpace(raw))
	return on, "global", nil
}

// ValidVoicePermissionScope accepts server:<id>, user:<id> and role:<id>.
// "global" is not a row: it is the feature setting.
func ValidVoicePermissionScope(scope string) bool {
	kind, id, ok := strings.Cut(scope, ":")
	if !ok || id == "" || strings.TrimSpace(id) != id {
		return false
	}
	switch kind {
	case "server", "role":
		n, err := strconv.Atoi(id)
		return err == nil && n > 0 && strconv.Itoa(n) == id
	case "user":
		return len(id) <= 64 && !strings.ContainsAny(id, ": ")
	}
	return false
}
