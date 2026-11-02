package handlers

import (
	"errors"
	"testing"

	"dylaris-core/handlers/access"
	"dylaris-core/store"
)

type modeFakeStore struct {
	store.Store
	value  string
	getErr error
	setKey string
	setVal string
	setErr error
}

func (f *modeFakeStore) GetSetting(string) (string, error) { return f.value, f.getErr }
func (f *modeFakeStore) SetSetting(key, value string) error {
	f.setKey, f.setVal = key, value
	return f.setErr
}

func TestPermissionsMode_DefaultsToSimple(t *testing.T) {
	if got := access.PermissionsMode(&modeFakeStore{value: "", getErr: errors.New("no row")}); got != "simple" {
		t.Fatalf("PermissionsMode = %q, want simple on error", got)
	}
	if got := access.PermissionsMode(&modeFakeStore{value: "advanced"}); got != "advanced" {
		t.Fatalf("PermissionsMode = %q, want advanced", got)
	}
	if got := access.PermissionsMode(&modeFakeStore{value: "bogus"}); got != "simple" {
		t.Fatalf("PermissionsMode = %q, want simple on unknown value", got)
	}
}

func TestSetPermissionsMode_ValidatesEnum(t *testing.T) {
	fs := &modeFakeStore{}
	if err := SetPermissionsMode(fs, "advanced"); err != nil {
		t.Fatalf("SetPermissionsMode(advanced): %v", err)
	}
	if fs.setKey != "permissions_mode" || fs.setVal != "advanced" {
		t.Fatalf("persisted (%q=%q), want permissions_mode=advanced", fs.setKey, fs.setVal)
	}
	if err := SetPermissionsMode(&modeFakeStore{}, "bogus"); err == nil {
		t.Fatal("SetPermissionsMode(bogus) = nil, want error")
	}
}
