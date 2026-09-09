package main

import (
	"fmt"
	"strings"
	"testing"
)

// The node said one thing about isolation, once, into stdout at boot. The panel
// showed nothing at all, and a node's stdout does not survive its container.
//
// The sentence it now reports has to carry three distinct states, and the third
// is the one a boot-time answer cannot express: isolation ON, servers on the
// shared network anyway.
func TestIsolationNotice(t *testing.T) {
	tests := []struct {
		name      string
		enabled   bool
		fallbacks int
		reason    string
		want      func(string) bool
		why       string
	}{
		{
			name: "on and holding", enabled: true, fallbacks: 0,
			want: func(s string) bool { return s == "" },
			why:  "the one state that needs no words; a badge already says it",
		},
		{
			name: "off", enabled: false,
			want: func(s string) bool {
				return strings.Contains(s, "SIDECAR_REDIS_ADDR") && strings.HasPrefix(s, "Off")
			},
			why: "names the setting an operator would edit, and says which state it is in",
		},
		{
			name: "on but one server fell back", enabled: true, fallbacks: 1, reason: "subnet full",
			want: func(s string) bool {
				return strings.Contains(s, "1 server ") && strings.Contains(s, "subnet full")
			},
			why: "the count and the cause are both needed: one says how bad, the other what to do",
		},
		{
			name: "on but several fell back", enabled: true, fallbacks: 4, reason: "docker: endpoint refused",
			want: func(s string) bool {
				return strings.Contains(s, "4 servers ") && strings.Contains(s, "docker: endpoint refused")
			},
			why: "plural, because a sentence that reads wrong reads as a bug in the panel",
		},
		{
			// The fallback count is what separates "isolation is switched off"
			// from "isolation is on and not working", and those need different
			// answers from the operator. They must never produce one sentence.
			name: "off is not the same sentence as not holding", enabled: false, fallbacks: 3, reason: "subnet full",
			want: func(s string) bool { return strings.HasPrefix(s, "Off") },
			why:  "isolation being off outranks a stale fallback count from before it was turned off",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isolationNotice(tt.enabled, tt.fallbacks, tt.reason)
			if !tt.want(got) {
				t.Errorf("notice = %q (%s)", got, tt.why)
			}
		})
	}
}

// The counter is written from container creation and read from the heartbeat,
// so it keeps the LAST reason rather than the first: the first is what the
// operator already reacted to, the current one is what they need next.
func TestIsolationStateRecordsCountAndLastReason(t *testing.T) {
	var s isolationState

	if n, r := s.snapshot(); n != 0 || r != "" {
		t.Fatalf("fresh state = (%d, %q), want (0, \"\")", n, r)
	}

	s.recordFallback(fmt.Errorf("subnet full"))
	s.recordFallback(fmt.Errorf("docker refused the endpoint"))

	n, r := s.snapshot()
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	if r != "docker refused the endpoint" {
		t.Errorf("reason = %q, want the most recent one", r)
	}
}

// A nil error is not a fallback. tenantEndpoints reaches the recorder only on
// the error path today, but a counter that can be incremented by success is a
// counter nobody can trust.
func TestIsolationStateIgnoresANilError(t *testing.T) {
	var s isolationState
	s.recordFallback(nil)
	if n, _ := s.snapshot(); n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}
