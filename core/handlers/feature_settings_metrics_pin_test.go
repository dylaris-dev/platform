package handlers

import (
	"os"
	"strings"
	"testing"
)

// One setting, one writer.
//
// The recording switch lived in the feature bundle while its database lived
// here, so two endpoints wrote `feature_metrics_enabled` and the last save won:
// the feature card would have reverted a change made on the statistics card
// from its own stale copy of the bundle.
//
// Checked at the source rather than by driving Set, and the site is named
// exactly - the writes table in that file is the only way the bundle could
// touch this setting, so its absence there IS the invariant. Driving the
// handler would have proven the same thing through the modpack module sync,
// which is a different subsystem and would need a fake of its own here.
func TestTheFeatureBundleNoLongerWritesTheMetricsSwitch(t *testing.T) {
	src, err := os.ReadFile("feature_settings.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "MetricsEnabledSetting") {
		t.Fatal("the feature bundle references the statistics switch again; " +
			"two writers of one setting means the last save wins and the other card reverts it")
	}
	// And the payload field is gone, so an old body cannot smuggle a false in
	// through Go's zero value for an absent bool.
	if strings.Contains(string(src), "Metrics bool") {
		t.Fatal("featureSettingsPayload still has a Metrics field")
	}
}
