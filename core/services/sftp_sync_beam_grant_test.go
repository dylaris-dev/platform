package services

import (
	"strings"
	"sync"
	"testing"

	"dylaris-core/services/redisacl"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/redis/go-redis/v9"
)

// aclRecorder captures the ACL SETUSER calls a sync tick makes. miniredis has no
// ACL of its own, so the command is registered here purely to observe it - which
// is the point: the assertion is that the grant REACHES Redis with the right
// users in it, not that a pure function can format a string.
type aclRecorder struct {
	mu    sync.Mutex
	calls map[string]string // ACL username -> the joined arguments after it
}

func (r *aclRecorder) argsFor(user string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.calls[user]
	return v, ok
}

func newBeamGrantTest(t *testing.T, fs *sftpPruneFakeStore) (*SFTPSyncService, *aclRecorder) {
	t.Helper()
	mr := miniredis.RunT(t)
	rec := &aclRecorder{calls: map[string]string{}}
	err := mr.Server().Register("ACL", func(c *server.Peer, _ string, args []string) {
		// args: SETUSER <username> <rule>...
		if len(args) >= 2 && strings.EqualFold(args[0], "SETUSER") {
			rec.mu.Lock()
			rec.calls[args[1]] = strings.Join(args[2:], " ")
			rec.mu.Unlock()
		}
		c.WriteOK()
	})
	if err != nil {
		t.Fatalf("register ACL: %v", err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewSFTPSyncService(fs, rdb, nil), rec
}

// A node's beam-quota grant must name its OWN users and no others. This is the
// wiring half of the fix: the selector is only worth anything if the sync
// actually issues it, and only correct if the set it issues is per node.
func TestSyncGrantsEachNodeOnlyItsOwnUploadCounters(t *testing.T) {
	svc, rec := newBeamGrantTest(t, pruneFixture())
	svc.sync()

	gotA, ok := rec.argsFor(redisacl.NodeUsername(pruneNodeA))
	if !ok {
		t.Fatal("no beam quota grant was issued for node-a; its uploads go uncounted, silently, because the quota package fails open")
	}
	if !strings.Contains(gotA, "dylaris:beam:daily:alice:*") {
		t.Errorf("node-a's grant does not cover alice, the one user on it: %s", gotA)
	}
	if strings.Contains(gotA, "bob") {
		t.Errorf("node-a's grant reaches bob, who is on node-b: %s", gotA)
	}

	gotB, _ := rec.argsFor(redisacl.NodeUsername(pruneNodeB))
	if strings.Contains(gotB, "alice") {
		t.Errorf("node-b's grant reaches alice, who is on node-a: %s", gotB)
	}
}

// Losing access has to take the grant with it. A revocation that leaves the key
// reachable is the failure this whole change is about, one scope further in.
func TestARevokedUsersUploadCounterGrantIsWithdrawn(t *testing.T) {
	fs := pruneFixture()
	svc, rec := newBeamGrantTest(t, fs)

	svc.sync()
	if got, _ := rec.argsFor(redisacl.NodeUsername(pruneNodeA)); !strings.Contains(got, "alice") {
		t.Fatalf("setup: node-a never had a grant for alice: %s", got)
	}

	fs.access[1] = nil // alice's access to node-a is revoked
	svc.sync()

	got, ok := rec.argsFor(redisacl.NodeUsername(pruneNodeA))
	if !ok {
		t.Fatal("node-a got no ACL call at all on the revoking tick")
	}
	if strings.Contains(got, "alice") {
		t.Errorf("node-a still holds alice's counter after her access was revoked: %s", got)
	}
	if !strings.Contains(got, "clearselectors") {
		t.Errorf("a node left with no users must have its selector CLEARED, not skipped: %s", got)
	}
}

// A customer's machine holds this credential, and who is on it is the
// customer's to decide: naming a stranger was enough to reach that stranger's
// platform-wide counter. Such a node gets no counter at all.
func TestACustomerNodeGetsNoUploadCounter(t *testing.T) {
	fs := pruneFixture()
	owner := "u-carol"
	fs.nodes[0].OwnerID = &owner
	svc, rec := newBeamGrantTest(t, fs)
	svc.sync()

	got, ok := rec.argsFor(redisacl.NodeUsername(pruneNodeA))
	if !ok || !strings.Contains(got, "clearselectors") {
		t.Fatalf("the customer node's old grant was not cleared: %q", got)
	}
	if strings.Contains(got, "alice") {
		t.Errorf("a customer node reaches alice's upload counter: %s", got)
	}
	if b, _ := rec.argsFor(redisacl.NodeUsername(pruneNodeB)); !strings.Contains(b, "bob") {
		t.Errorf("a platform node lost its user's counter: %s", b)
	}
}
