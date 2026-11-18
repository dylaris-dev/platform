package storagemigrate

import (
	"errors"
	"strings"
	"testing"
)

func TestAuthorizeConfigSwitch(t *testing.T) {
	cases := []struct {
		name    string
		report  *StorageVerifyReport
		wantErr bool
	}{
		{"authorized: a passing full verification", &StorageVerifyReport{OK: true, Mode: VerifyModeFull}, false},
		// A switch is reversible (point the config back) and destroys nothing,
		// so a passing SAMPLE verification is enough for it. Only the delete,
		// which is irreversible, insists on a full run.
		{"authorized: a passing sample verification", &StorageVerifyReport{OK: true, Mode: VerifyModeSample, CheckedFraction: 0.1}, false},
		{"blocked: verification failed", &StorageVerifyReport{OK: false, Mode: VerifyModeFull, ProblemsTotal: 2}, true},
		{"blocked: no report at all", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := AuthorizeConfigSwitch(c.report)
			if (err != nil) != c.wantErr {
				t.Fatalf("AuthorizeConfigSwitch(%+v) err = %v, wantErr %v", c.report, err, c.wantErr)
			}
			if c.wantErr && !errors.Is(err, ErrSwitchNotAuthorized) {
				t.Errorf("err = %v, want it to wrap ErrSwitchNotAuthorized", err)
			}
		})
	}
}

func TestSwitchFailureReport_NamesBothPlacesAndTheActiveConfig(t *testing.T) {
	// A failed switch is the one outcome where the operator MUST be told the
	// system is duplicated rather than broken, and which side is live. A vague
	// "switch failed" would invite them to delete something.
	msg := SwitchFailureReport("path:/mnt/old/library", "s3:https://new.example.com/bucket/library", VerifyModeFull)
	for _, want := range []string{"path:/mnt/old/library", "s3:https://new.example.com/bucket/library"} {
		if !strings.Contains(msg, want) {
			t.Errorf("report %q does not name %q", msg, want)
		}
	}
	for _, want := range []string{"both", "still active", "Nothing was deleted"} {
		if !strings.Contains(msg, want) {
			t.Errorf("report %q is missing the phrase %q", msg, want)
		}
	}
}

// TestSwitchFailureReport_StatesTheVerificationScope pins the claim this
// message is allowed to make about verification. AuthorizeConfigSwitch
// deliberately accepts a SAMPLE report (a switch is reversible), so this phase
// is reachable after a run that never content-checked most objects. Saying a
// flat "and verified" there overstates what happened - on the one message whose
// whole purpose is to stop an operator deleting data.
func TestSwitchFailureReport_StatesTheVerificationScope(t *testing.T) {
	cases := []struct {
		name       string
		verifyMode string
		wantSubstr []string
		notSubstr  []string
	}{
		{
			name:       "full",
			verifyMode: VerifyModeFull,
			wantSubstr: []string{"FULL verification", "every object in the manifest was hashed"},
			notSubstr:  []string{"SAMPLE"},
		},
		{
			name:       "sample",
			verifyMode: VerifyModeSample,
			wantSubstr: []string{"SAMPLE verification", "bounded subset", "never content-checked"},
			notSubstr:  []string{"FULL verification"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := SwitchFailureReport("path:/old", "path:/new", c.verifyMode)
			for _, want := range c.wantSubstr {
				if !strings.Contains(msg, want) {
					t.Errorf("report %q is missing %q", msg, want)
				}
			}
			for _, bad := range c.notSubstr {
				if strings.Contains(msg, bad) {
					t.Errorf("report %q must not contain %q for mode %q", msg, bad, c.verifyMode)
				}
			}
			// Invariant across both modes: the operator must still be told
			// nothing was deleted and which side is live.
			for _, want := range []string{"Nothing was deleted", "still active"} {
				if !strings.Contains(msg, want) {
					t.Errorf("report %q is missing %q", msg, want)
				}
			}
		})
	}
}

func TestSwitchPartialFailureReport_ClaimsNoActiveConfig(t *testing.T) {
	msg := SwitchPartialFailureReport("path:/mnt/old", "s3:https://new.example.com/b")
	for _, want := range []string{"path:/mnt/old", "s3:https://new.example.com/b", "Nothing was deleted", "MIX"} {
		if !strings.Contains(msg, want) {
			t.Errorf("report %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "still active") {
		t.Errorf("report %q claims a config is still active, which a half-written switch cannot", msg)
	}
}
