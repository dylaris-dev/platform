package servers

import (
	"strings"
	"testing"
)

// The wipe vocabulary is a FROZEN cross-repo contract with platform/node
// (installer_wipe.go), which keeps its own copy and refuses anything it does not
// know. Both sides validate: this one keeps a bad request out of the queue, the
// node's keeps a bad queue entry from deleting anything.
//
// The list is pinned here so adding a token is a deliberate two-repo change with
// the node shipping first. A token accepted here and unknown there fails the
// install at the node, after the caller was told it was queued.
func TestInstallWipeVocabularyIsFrozen(t *testing.T) {
	want := []string{"config", "jars", "libraries", "mods", "versions"}
	got := make([]string, 0, len(installWipeTokens))
	for k := range installWipeTokens {
		got = append(got, k)
	}
	if len(got) != len(want) {
		t.Fatalf("vocabulary is %v, want %v - add it to platform/node first", sorted(got), want)
	}
	for _, w := range want {
		if !installWipeTokens[w] {
			t.Errorf("%q is missing from the vocabulary", w)
		}
	}
}

// An unknown target is refused rather than dropped.
//
// Dropping it would hand back a dirty update reported as a success, which is the
// exact failure the feature exists to end - now with a confirmation dialog in
// front of it to make it look deliberate.
func TestValidateWipePaths(t *testing.T) {
	if err := validateWipePaths(nil); err != nil {
		t.Errorf("no targets means install on top, which is not an error: %v", err)
	}
	if err := validateWipePaths([]string{"mods", "config", " jars "}); err != nil {
		t.Errorf("known targets should pass, whitespace included: %v", err)
	}

	// "world" is the one an operator would most plausibly try, and the one that
	// must never be in the vocabulary.
	for _, bad := range []string{"world", "logs", "MODS", "mods/", "../etc", ""} {
		err := validateWipePaths([]string{bad})
		if err == nil {
			t.Errorf("%q was accepted as a wipe target", bad)
			continue
		}
		if !strings.Contains(err.Error(), "unknown wipe target") {
			t.Errorf("%q: error = %v, want it to name the unknown target", bad, err)
		}
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
