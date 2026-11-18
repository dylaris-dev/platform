package storagemigrate

import (
	"errors"
	"fmt"
)

// ErrSwitchNotAuthorized is returned when the preconditions for pointing the
// live config at the migration target are not met.
var ErrSwitchNotAuthorized = errors.New("storagemigrate: switching the active config is not authorized")

// AuthorizeConfigSwitch gates the switching_config phase: the active storage
// config may be repointed at the target ONLY after a verification that passed.
//
// Note what is deliberately NOT required here: VerifyModeFull. A switch is
// reversible - the operator points the config back - and destroys nothing, so
// insisting on a full verification would only push operators into skipping
// verification altogether on large data sets. The irreversible step is the
// delete, and that one does insist on full (AuthorizeSourceDelete).
func AuthorizeConfigSwitch(report *StorageVerifyReport) error {
	if report == nil {
		return fmt.Errorf("%w: no verification report", ErrSwitchNotAuthorized)
	}
	if !report.OK {
		return fmt.Errorf("%w: verification found %d problem(s)", ErrSwitchNotAuthorized, report.ProblemsTotal)
	}
	return nil
}

// SwitchFailureReport is the message a failed switching_config phase reports.
//
// This is the ONE outcome where the operator must not be left guessing: the
// copy succeeded, so the data now exists in BOTH places, and the old config is
// still the live one. Saying only "switch failed" would invite someone to start
// deleting. The phrasing names both locations, states that nothing was deleted,
// and says which config is active.
//
// verifyMode is taken as an argument rather than assumed, because
// AuthorizeConfigSwitch deliberately does NOT require VerifyModeFull: a switch
// is reversible, so a passing SAMPLE verification is enough to reach this
// phase. A flat "and verified" would therefore overstate what actually ran, on
// the very message whose job is to stop someone deleting data.
func SwitchFailureReport(sourceLabel, targetLabel, verifyMode string) string {
	verified := "a FULL verification passed (every object in the manifest was hashed)"
	if verifyMode == VerifyModeSample {
		verified = "a SAMPLE verification passed (presence checked for every object in the manifest, contents hashed for a bounded subset only, so some objects were never content-checked)"
	}
	return fmt.Sprintf(
		"The copy completed and %s, but switching the active config failed. "+
			"The data now exists in both places: %s (source) and %s (target). "+
			"Nothing was deleted. The SOURCE config is still active, so the system is "+
			"serving from %s. Fix the cause and re-run: the copy resumes and skips "+
			"everything already identical.",
		verified, sourceLabel, targetLabel, sourceLabel)
}

// SwitchPartialFailureReport is the message for a switch that failed partway
// and could not restore the earlier settings. Unlike SwitchFailureReport it
// cannot name an active config, because the saved one is a mix of both.
func SwitchPartialFailureReport(sourceLabel, targetLabel string) string {
	return fmt.Sprintf(
		"The copy completed and verified, but switching the active config failed partway and the previous settings could not be restored. "+
			"The data exists in both places: %s (source) and %s (target). Nothing was deleted. "+
			"The saved config is now a MIX of both, so open the storage settings, save the config you want "+
			"(either location holds every object), and only then re-run.",
		sourceLabel, targetLabel)
}
