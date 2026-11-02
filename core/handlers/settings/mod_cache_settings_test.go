package settings

import (
	"context"
	"testing"

	"dylaris-core/handlers/handlertest"
	"dylaris-core/services"
)

func TestCacheConfigConfigured(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want bool
	}{
		{"empty means the Redis Core already has", "", false},
		{"whitespace is still empty", "   ", false},
		{"a host and port is a dedicated endpoint", "cache:6379", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (services.CacheConfig{Addr: tc.addr}).Configured(); got != tc.want {
				t.Errorf("Configured() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The cache password is a credential and must be encrypted at rest like the
// other stored credentials. It is registered in store/settings_secrets.go by
// key name, so a rename there would silently start writing it in the clear;
// this pins the name the handler writes against that registration.
func TestCacheSettingKeysAreTheOnesRegisteredAsSecret(t *testing.T) {
	if settingModCachePassword != "mod_cache_redis_password" {
		t.Errorf("password setting key is %q; store/settings_secrets.go encrypts %q",
			settingModCachePassword, "mod_cache_redis_password")
	}
}

func TestCacheSavedMessageNamesWhereItWent(t *testing.T) {
	// The default is not a failure and must not read like one: an operator who
	// clears the field is choosing the Redis they already run.
	if got := cacheSavedMessage(""); got != "Mod metadata is cached in the Redis this panel already uses." {
		t.Errorf("default message = %q", got)
	}
	if got := cacheSavedMessage("cache:6379"); got != "Mod metadata is now cached in cache:6379." {
		t.Errorf("dedicated message = %q", got)
	}
}

// ApplyCacheSettings used to be an AppState method main called at boot; as a
// function it must still point the cache at the STORED endpoint, and a Core
// without a cache must boot.
func TestApplyCacheSettingsAdoptsTheStoredEndpoint(t *testing.T) {
	if err := ApplyCacheSettings(context.Background(), nil, handlertest.NewSettingsStore()); err != nil {
		t.Fatalf("no cache: %v", err)
	}
	bus, _ := handlertest.Redis(t)
	_, dedicated := handlertest.Redis(t)
	st := handlertest.NewSettingsStore()
	st.KV[settingModCacheAddr] = dedicated.Addr()
	cache := services.NewCache(bus)
	if err := ApplyCacheSettings(context.Background(), cache, st); err != nil {
		t.Fatal(err)
	}
	if s := cache.Status(context.Background()); !s.Dedicated || s.Addr != dedicated.Addr() || !s.Healthy {
		t.Fatalf("status %+v, want the stored endpoint %s", s, dedicated.Addr())
	}
}
