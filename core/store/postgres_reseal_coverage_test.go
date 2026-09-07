package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The rotation path's one real failure mode is silence: somebody adds a sixth
// at-rest purpose, ResealAtRest never learns about it, and a rotation leaves
// that column sealed under a secret nothing holds any more. Nothing fails at
// build time, nothing fails in a test, and it is discovered by a customer whose
// node cannot authenticate.
//
// So this test does not check the file, it checks the SITES: every place in the
// module that derives an at-rest key must name a purpose this package has
// classified, either as resealed or as deliberately not resealable.

// resealedPurposes is what ResealAtRest actually moves. Kept next to the test
// rather than read out of the implementation on purpose - a list derived from
// the code under test cannot disagree with it.
var resealedPurposes = map[string]bool{
	nodeSecretPurpose:          true,
	backupStorageSecretPurpose: true,
	storageConnSecretPurpose:   true,
	modrinthPATPurpose:         true,
	settingsSecretPurpose:      true,
}

// unresealablePurposes are the derivations that are NOT stored ciphertext, with
// the reason in postgres_reseal.go. Listing them is what makes the test able to
// tell "considered and excluded" from "nobody noticed".
var unresealablePurposes = map[string]string{
	mrpackPathPurpose: "an HMAC over a storage path, not a stored value: rotating moves the OBJECTS",
}

// knownPurposeIdents maps the constant NAMES a call site may use to their
// values, because a purpose reaches DeriveKey as a constant about as often as
// it reaches it as a literal.
var knownPurposeIdents = map[string]string{
	"nodeSecretPurpose":          nodeSecretPurpose,
	"modrinthPATPurpose":         modrinthPATPurpose,
	"mrpackPathPurpose":          mrpackPathPurpose,
	"backupStorageSecretPurpose": backupStorageSecretPurpose,
	"storageConnSecretPurpose":   storageConnSecretPurpose,
	"settingsSecretPurpose":      settingsSecretPurpose,
}

var deriveKeyCall = regexp.MustCompile(`crypto\.DeriveKey\(\s*[^,()]+,\s*([^)]+)\)`)

func TestEveryAtRestPurposeIsClassified(t *testing.T) {
	root := ".."

	found := map[string][]string{} // purpose -> sites
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "node_modules" || info.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The dispatcher derives every purpose from a loop variable. It is the
		// thing being checked, not a site that introduces a purpose.
		if filepath.Base(path) == "postgres_reseal.go" {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range deriveKeyCall.FindAllSubmatch(src, -1) {
			arg := strings.TrimSpace(string(m[1]))
			purpose, ok := resolvePurposeArg(arg)
			if !ok {
				t.Errorf("%s derives an at-rest key with %s, which this test cannot resolve.\n"+
					"Add it to knownPurposeIdents and classify it in resealedPurposes or unresealablePurposes.",
					filepath.ToSlash(path), arg)
				continue
			}
			found[purpose] = append(found[purpose], filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(found) == 0 {
		// The regex silently matching nothing would make this test pass
		// forever while checking nothing at all.
		t.Fatal("found no DeriveKey call sites; the scan is broken, not the code")
	}

	for purpose, sites := range found {
		if resealedPurposes[purpose] {
			continue
		}
		if why, ok := unresealablePurposes[purpose]; ok {
			t.Logf("%q is deliberately not resealed (%s), used at %v", purpose, why, sites)
			continue
		}
		t.Errorf("at-rest purpose %q (used at %v) is neither resealed nor listed as unresealable.\n"+
			"A rotation would leave it sealed under the old secret and nothing would report it.",
			purpose, sites)
	}

	// The other direction: a purpose ResealAtRest moves but nothing derives is
	// a column being rewritten for no reason, or a renamed tag whose real call
	// site is now unresealed under a different name.
	for purpose := range resealedPurposes {
		if len(found[purpose]) == 0 {
			t.Errorf("ResealAtRest moves %q but no call site derives it", purpose)
		}
	}
}

func resolvePurposeArg(arg string) (string, bool) {
	if strings.HasPrefix(arg, `"`) && strings.HasSuffix(arg, `"`) {
		return strings.Trim(arg, `"`), true
	}
	v, ok := knownPurposeIdents[arg]
	return v, ok
}
