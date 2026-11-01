package services

import (
	"context"
	"database/sql"
	"testing"

	"dylaris-core/models"
)

type ownStorageUsers map[string]*models.User

func (u ownStorageUsers) GetUserByID(id string) (*models.User, error) {
	if x, ok := u[id]; ok {
		return x, nil
	}
	return nil, sql.ErrNoRows
}

// Unset and anything unrecognised read as all: the switch must not take own
// storage away from an operator who never touched it.
func TestOwnStorageAudience(t *testing.T) {
	ctx := context.Background()
	users := ownStorageUsers{"adm": {ID: "adm", IsAdmin: true}, "usr": {ID: "usr"}}
	tests := []struct {
		name                   string
		settings               map[string]string
		want                   string
		wantAdmin, wantUser    bool
		wantForAdm, wantForUsr bool
	}{
		{"unset", map[string]string{}, "all", true, true, true, true},
		{"garbage", map[string]string{OwnStorageSettingKey: "banana"}, "all", true, true, true, true},
		{"all", map[string]string{OwnStorageSettingKey: "all"}, "all", true, true, true, true},
		{"admin", map[string]string{OwnStorageSettingKey: "admin"}, "admin", true, false, true, false},
		{"off", map[string]string{OwnStorageSettingKey: "off"}, "off", false, false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ff := NewFeatureFlags(stubSettings{m: tc.settings})
			if got := ff.OwnStorageAudience(ctx); got != tc.want {
				t.Errorf("audience = %q, want %q", got, tc.want)
			}
			if got := ff.OwnStorageAllowed(ctx, true); got != tc.wantAdmin {
				t.Errorf("allowed(admin) = %v, want %v", got, tc.wantAdmin)
			}
			if got := ff.OwnStorageAllowed(ctx, false); got != tc.wantUser {
				t.Errorf("allowed(user) = %v, want %v", got, tc.wantUser)
			}
			if got := OwnStorageAllowedFor(ctx, ff, users, "adm"); got != tc.wantForAdm {
				t.Errorf("allowedFor(adm) = %v, want %v", got, tc.wantForAdm)
			}
			if got := OwnStorageAllowedFor(ctx, ff, users, "usr"); got != tc.wantForUsr {
				t.Errorf("allowedFor(usr) = %v, want %v", got, tc.wantForUsr)
			}
		})
	}
	var nilFlags *FeatureFlags
	if !OwnStorageAllowedFor(ctx, nilFlags, users, "missing") {
		t.Error("nil flags must keep the pre-switch behaviour (allowed)")
	}
	if OwnStorageAllowedFor(ctx, NewFeatureFlags(stubSettings{m: map[string]string{OwnStorageSettingKey: "admin"}}), users, "missing") {
		t.Error("an owner that cannot be looked up must not count as admin")
	}
}
