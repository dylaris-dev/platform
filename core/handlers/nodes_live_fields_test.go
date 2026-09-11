package handlers

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"dylaris-core/models"
)

// A node's live fields have no column. ListNodes answers from the nodes TABLE,
// so on any endpoint that does not enrich, every one of them is the zero value
// for every node - and nothing errors, because a zero is a legal answer.
//
// It stayed invisible while no screen rendered one. The first badge that did
// shipped reading `false` on every node in the fleet: Settings -> Nodes calls
// GET /api/nodes, which returned rows straight from the database, so the badge
// could only ever show the zero value - including about nodes that reported
// the opposite.
//
// The test is on the SOURCE because the defect is a missing call, not a wrong
// value: there is nothing to assert on the output of a handler that never asked
// Redis anything. It names the sites rather than scanning the package, so a
// THIRD endpoint returning a node list has to be added here deliberately.
func TestEveryNodeListEndpointEnrichesFromTheHeartbeat(t *testing.T) {
	sites := []struct {
		file string
		fn   string
	}{
		{"nodes.go", "GetNodes"},
		{"infrastructure.go", "GetOverview"},
	}

	for _, site := range sites {
		t.Run(site.fn, func(t *testing.T) {
			body := funcBody(t, site.file, site.fn)
			if !strings.Contains(body, `"nodes"`) {
				t.Fatalf("%s no longer returns a node list - move or drop this site", site.fn)
			}
			if !strings.Contains(body, "EnrichNodesWithLiveStats") {
				t.Errorf("%s returns nodes without enriching them: every heartbeat field "+
					"(netPolicy, cpuUsage, ramTotal, linkCount, portRange, sharedStorage) "+
					"reaches the panel as its zero value", site.fn)
			}
		})
	}
}

// funcBody returns the text of one top-level func, from its signature to the
// closing brace in column 0.
func funcBody(t *testing.T, file, fn string) string {
	t.Helper()
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	re := regexp.MustCompile(`(?ms)^func (?:\([^)]*\) )?` + regexp.QuoteMeta(fn) + `\(.*?^}`)
	body := re.FindString(string(src))
	if body == "" {
		t.Fatalf("%s not found in %s", fn, file)
	}
	return body
}

// nil is a third answer and has to stay absent on the wire.
//
// The panel guards on `node.netPolicy === undefined` so a node nobody has heard
// from renders no badge at all. A non-pointer bool defeats that guard by
// construction: encoding/json writes `"netPolicy": false` for every node
// without a heartbeat, and the panel would then state something about a
// machine that had reported nothing.
func TestUnmeasuredNetPolicyIsAbsentFromTheJSON(t *testing.T) {
	silent, err := json.Marshal(models.Node{ID: 1, Name: "no-heartbeat"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(silent), "netPolicy") {
		t.Errorf("a node with no heartbeat carries a netPolicy field: %s", silent)
	}

	for _, measured := range []bool{true, false} {
		b, err := json.Marshal(models.Node{ID: 1, NetPolicy: &measured})
		if err != nil {
			t.Fatal(err)
		}
		var back struct {
			NetPolicy *bool `json:"netPolicy"`
		}
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.NetPolicy == nil || *back.NetPolicy != measured {
			t.Errorf("measured %v did not survive the round trip: %s", measured, b)
		}
	}
}
