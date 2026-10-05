package handlers

import (
	"strings"
	"testing"
)

// Regenerating the backup codes wrote back the secret it had just read. Once
// secrets are sealed, one that reads as "" (a key mismatch) would have been
// erased while 2FA stayed on, with nothing left to validate a code against.
func TestRegeneratingBackupCodesLeavesTheSecretAlone(t *testing.T) {
	body := funcBody(t, "totp.go", "RegenerateBackupCodesHandler")
	if strings.Contains(body, "SetUserTOTP(") {
		t.Error("RegenerateBackupCodesHandler writes the TOTP secret")
	}
	if !strings.Contains(body, "SetUserTOTPBackupCodes(") {
		t.Error("RegenerateBackupCodesHandler no longer stores the new codes")
	}
}
