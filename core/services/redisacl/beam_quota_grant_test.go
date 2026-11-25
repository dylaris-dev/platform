package redisacl

import (
	"strings"
	"testing"
	"time"

	"dylaris-pkg/beam/quota"
)

// TestNodeHoldsNoFleetWideBeamCounterGrant is the regression guard for the
// defect this selector replaced: "~dylaris:beam:daily:*" in the node's root
// permission was a read+write grant on a key named after a USER, held by every
// node in the fleet including a machine a customer owns.
//
// Written against the namespace rather than the one literal string, because the
// value of the grant is what makes it dangerous: any pattern reaching another
// user's counter is the same defect however it is spelled.
func TestNodeHoldsNoFleetWideBeamCounterGrant(t *testing.T) {
	rules := BuildNodeACLRules("n1", "pw", []string{"uuid-a"})
	for _, r := range rules {
		s, ok := r.(string)
		if !ok || !strings.Contains(s, "dylaris:beam:daily:") {
			continue
		}
		// A grant is only acceptable here if it cannot name a stranger: the
		// selector spells out one prefix per user, so a "*" standing where the
		// username belongs is the failure.
		if strings.Contains(s, "dylaris:beam:daily:*") {
			t.Errorf("the node holds a fleet-wide grant on the per-user upload counter: %q\n"+
				"any node could then lock a stranger out of uploads for the day, or reset its own users' counter", s)
		}
	}
}

func TestBeamQuotaGrantIsPerUser(t *testing.T) {
	sel := BeamQuotaSelector(names("carol", "alice"))

	// Sorted, so an unchanged tick sends an unchanged string.
	if want := "(~dylaris:beam:daily:alice:* ~dylaris:beam:daily:carol:* +get +incrby +expire)"; sel != want {
		t.Fatalf("selector\n got: %s\nwant: %s", sel, want)
	}

	// The three commands the shared quota package uses, and nothing that could
	// reset a counter outright. EXPIRE is granted because RecordDailyUsage arms
	// the TTL, and a short TTL IS a reset - which is precisely why the key
	// pattern, not the command list, is what has to be narrow.
	for _, forbidden := range []string{"+del", "+set", "+decrby", "+getdel", "+@write", "+@all"} {
		if strings.Contains(sel, forbidden) {
			t.Errorf("selector grants %s, which can reset a counter: %s", forbidden, sel)
		}
	}
}

// TestBeamQuotaGrantMatchesTheRealKey is the contract between this grant and the
// package that writes the key. A pattern that no longer covers the key throws no
// error anywhere: the node gets NOPERM, the quota package fails OPEN, and the
// daily limit silently stops being enforced.
func TestBeamQuotaGrantMatchesTheRealKey(t *testing.T) {
	sel := BeamQuotaSelector(names("alice"))
	prefix := "~" + quota.DailyKeyPrefix("alice")
	if !strings.Contains(sel, prefix) {
		t.Fatalf("selector %s does not carry the key prefix %s", sel, prefix)
	}

	key := quota.DailyKey(quota.BucketFor("", "alice"), time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC))
	if !strings.HasPrefix(key, strings.TrimPrefix(prefix, "~")) {
		t.Errorf("the granted pattern %s does not match the key the node writes, %s", prefix, key)
	}
	// And it must not reach the next user along. "alice" and "alice2" are both
	// valid usernames, and a prefix without the trailing ':' would cover both.
	if other := quota.DailyKey(quota.BucketFor("", "alice2"), time.Now()); strings.HasPrefix(other, strings.TrimPrefix(prefix, "~")) {
		t.Errorf("the grant for alice also covers %s", other)
	}
}

// A node hosting nobody must produce no selector at all, so the caller clears
// the grant instead of leaving a revoked user's counter reachable.
func TestBeamQuotaGrantIsEmptyForANodeWithNoUsers(t *testing.T) {
	if sel := BeamQuotaSelector(nil); sel != "" {
		t.Errorf("expected no selector for a node with no users, got %q", sel)
	}
}

// A username outside validate.Username (a space, a '~', a '+') would split the
// selector, and ACL SETUSER rejects the whole call - freezing the node's grant for
// every user - or, crafted, widens it. One bad row must cost only that row.
func TestBeamQuotaSelectorSkipsInvalidNames(t *testing.T) {
	cases := []struct {
		in   []BeamQuotaAccount
		want string
	}{
		{names("alice", "john doe", "x +@all ~*"), "(~dylaris:beam:daily:alice:* +get +incrby +expire)"},
		{names("john doe"), ""},
		{[]BeamQuotaAccount{{UserID: "x:* ~*", Username: "alice"}}, "(~dylaris:beam:daily:alice:* +get +incrby +expire)"},
	}
	for _, c := range cases {
		if got := BeamQuotaSelector(c.in); got != c.want {
			t.Errorf("BeamQuotaSelector(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func names(usernames ...string) []BeamQuotaAccount {
	out := make([]BeamQuotaAccount, len(usernames))
	for i, u := range usernames {
		out[i] = BeamQuotaAccount{Username: u}
	}
	return out
}

// R34-09: the counter is booked by user id, so the node must be granted the id
// counter - else it reads NOPERM, which the quota package reads as nothing
// used. The username counter stays granted for the rollout: a node or a Core
// older than the id still books there.
func TestBeamQuotaGrantCoversTheIDCounter(t *testing.T) {
	const id = "3f1c2a9e-0b7d-4e21-9c5a-6d8e7f901234"
	sel := BeamQuotaSelector([]BeamQuotaAccount{{UserID: id, Username: "alice"}})
	want := "(~dylaris:beam:daily-id:" + id + ":* ~dylaris:beam:daily:alice:* +get +incrby +expire)"
	if sel != want {
		t.Fatalf("selector\n got: %s\nwant: %s", sel, want)
	}
	for _, b := range []quota.Bucket{quota.BucketFor(id, "alice"), quota.BucketFor(id, "alice2"), quota.BucketFor("", "alice")} {
		key := quota.DailyKey(b, time.Now())
		covered := false
		for _, p := range strings.Fields(strings.Trim(sel, "()")) {
			if strings.HasPrefix(p, "~") && strings.HasPrefix(key, strings.TrimSuffix(p[1:], "*")) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("the grant %s does not cover the key the node writes, %s", sel, key)
		}
	}
}
