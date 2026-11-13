package store

import "testing"

func TestProviderKeySettingIsEncrypted(t *testing.T) {
	if !isSecretSettingKey("gateway.provider_key") {
		t.Fatal("gateway.provider_key must be sealed at rest")
	}
	for _, k := range []string{"gateway.provider", "gateway.provider_url"} {
		if isSecretSettingKey(k) {
			t.Errorf("%s is not a secret", k)
		}
	}
}
