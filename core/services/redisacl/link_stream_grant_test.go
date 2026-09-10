package redisacl

import (
	"strings"
	"testing"
)

// TestALinkIsGrantedBothOfItsStreams closes a hole neither key-coverage test
// could see.
//
// A Link writes TWO per-instance streams and names them with the SAME instance:
// dylaris:errors:link:<i> through errlog, and dylaris:link:<i>:stats through its
// stats publisher (gateway link/link.go, errLogInstance feeds both). Core
// granted the first and not the second, so every stats write answered NOPERM,
// go-redis returned an ordinary error, and the publisher logged to its own
// stdout. Core's gateway bandwidth consumer scans dylaris:link:*:stats and
// found nothing, so the panel's Link bandwidth had no data rather than an
// error.
//
// Measured in production on 2026-09-10 before the fix: nine
// dylaris:errors:link:* streams existed, holding 4 to 365 entries each, and
// dylaris:link:*:stats did not exist AT ALL - for any link, of any shape, since
// the feature shipped.
//
// Why it was invisible: there are two ACL builders for one consumer. The
// gateway repo's key sweep checks the HUB's rules, which do grant the stream;
// this repo's twin scans the NODE's source, and the key is written by the Link,
// which lives in the other repository. Each test was looking at the builder the
// other one was not.
//
// The assertion is therefore not "the pattern is present" - that is what was
// missed once already - but "both streams are named by the SAME instance". A
// grant that names a different instance is the identical silent failure with a
// pattern present to reassure the reader.
func TestALinkIsGrantedBothOfItsStreams(t *testing.T) {
	cases := []struct {
		name  string
		rules []interface{}
	}{
		// nodeToken is what the Link sees as NODE_ID, so errLogInstance returns
		// it and both streams are named by it.
		{"a node's link sidecar", BuildLinkACLRules("pw", "node-a", "tunnel-token")},
		// A route-only link has no NodeID, so errLogInstance falls back to the
		// ACL username - which IS the instance id Core passes here.
		{"a route-only link", BuildRouteOnlyLinkACLRules("pw", "tunnel-token", "link-abc")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errInstance, statsInstance string
			for _, r := range tc.rules {
				s, ok := r.(string)
				if !ok {
					continue
				}
				key := strings.TrimPrefix(strings.TrimPrefix(s, "%R~"), "~")
				if rest, found := strings.CutPrefix(key, "dylaris:errors:link:"); found {
					errInstance = rest
				}
				if rest, found := strings.CutPrefix(key, "dylaris:link:"); found {
					statsInstance = strings.TrimSuffix(rest, ":stats")
				}
			}

			if errInstance == "" {
				t.Fatal("no dylaris:errors:link: grant at all; the extraction is broken, not the rules")
			}
			if statsInstance == "" {
				t.Fatalf("the link is granted its error stream (instance %q) but no dylaris:link:<instance>:stats; "+
					"its telemetry answers NOPERM on every tick and the panel's Link bandwidth stays empty forever",
					errInstance)
			}
			if statsInstance != errInstance {
				t.Errorf("the two streams are named by different instances: errors by %q, stats by %q. "+
					"The Link names both with the same value, so one of these grants covers a key nothing writes "+
					"and the key it does write is refused",
					errInstance, statsInstance)
			}
		})
	}
}
