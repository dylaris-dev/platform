package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// memRevoked is the durable sign-out list in memory.
type memRevoked struct {
	ids map[string]time.Time
	err error
}

func (m *memRevoked) RevokeSession(id string, exp time.Time) error {
	if m.err != nil {
		return m.err
	}
	m.ids[id] = exp
	return nil
}

func (m *memRevoked) IsSessionRevoked(id string) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	exp, ok := m.ids[id]
	return ok && exp.After(time.Now()), nil
}

// Redis runs without persistence, so a restart used to bring every signed-out
// session back for the rest of its 90 days. The durable list keeps it out.
func TestASignedOutSessionStaysOutAfterARedisRestart(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	st := &memRevoked{ids: map[string]time.Time{}}
	c := newSessionClaims("alice", false, "hash")
	ctx := context.Background()

	if err := revokeSession(ctx, st, rdb, c); err != nil {
		t.Fatal(err)
	}
	if !sessionRevoked(ctx, st, rdb, c.ID) {
		t.Fatal("not revoked right after sign-out")
	}
	mr.FlushAll() // the restart
	if !sessionRevoked(ctx, st, rdb, c.ID) {
		t.Fatal("a Redis restart brought the signed-out session back")
	}
	mr.Close() // Redis gone entirely: Postgres still answers
	if !sessionRevoked(ctx, st, rdb, c.ID) {
		t.Fatal("with Redis down the signed-out session came back")
	}
	if sessionRevoked(ctx, st, rdb, newSessionClaims("bob", false, "h").ID) {
		t.Fatal("a session that was never signed out reads as revoked")
	}
}

// Only when neither side can answer does the check let a request through: both
// stores down must not sign the whole platform out.
func TestSessionRevokedFailsOpenOnlyWhenBothAreDown(t *testing.T) {
	st := &memRevoked{ids: map[string]time.Time{}, err: context.DeadlineExceeded}
	if sessionRevoked(context.Background(), st, nil, "some-id") {
		t.Fatal("an unanswerable check refused the request")
	}
}
