package beam

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The download path must keep BOTH halves of its verification.
//
// GetBeamDownload is unauthenticated and browser-facing, and it streams an
// executable over Core's own trusted TLS. Two things keep that safe, and they
// are separate functions:
//
//   - resolveDownloadCandidates consults the SIGNED manifest even when the
//     beam.download_link override is set, so the override moves only WHERE the
//     bytes come from - it used to return before the manifest was read at all,
//     which meant a settings.write holder (a delegatable panel capability, not
//     admin) could have Core hand every visitor an arbitrary binary;
//   - verifiedBeamBody hashes what actually arrived and refuses it unless it
//     matches the digest the manifest carried.
//
// Both are unit-tested in isolation, and that is the gap this closes: removing
// either CALL from the handler leaves every one of those tests green while Core
// streams an unverified executable. It is the same shape as a gate whose only
// tests build their input by hand.
//
// Asserted at the source rather than by driving the handler, because the
// signing key is a const: a behavioural test would need it to become a var, and
// loosening a security constant to make it testable is a worse trade than this.
func TestTheBeamDownloadHandlerStillVerifiesWhatItStreams(t *testing.T) {
	src, err := os.ReadFile("beam.go")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fn := regexp.MustCompile(`(?s)func \(h \*BeamHandler\) GetBeamDownload\(w http\.ResponseWriter, r \*http\.Request\) \{(.*?)\n\}`)
	m := fn.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("GetBeamDownload not found - the extraction stopped matching, the handler did not disappear")
	}
	body := m[1]

	for _, call := range []struct{ name, why string }{
		{"resolveDownloadCandidates(", "the signed manifest is what says these bytes are allowed to exist; without this the beam.download_link override picks the binary"},
		{"verifiedBeamBody(", "nothing would hash the bytes that actually arrived, so a mirror could serve anything"},
	} {
		if !strings.Contains(body, call.name) {
			t.Errorf("GetBeamDownload no longer calls %s - %s", call.name, call.why)
		}
	}

	// The digest has to be the one the manifest carried. verifiedBeamBody
	// refuses an empty string, so a literal cannot open a hole - but a second
	// variable could, and reading it back here is cheaper than finding out.
	if !regexp.MustCompile(`verifiedBeamBody\([^,]+, expectedSHA\)`).MatchString(body) {
		t.Error("verifiedBeamBody is no longer given expectedSHA, the digest resolveDownloadCandidates returned")
	}
}
