package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

func epochFixture(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	return redis.NewClient(&redis.Options{Addr: mr.Addr()}), mr
}

func ticketIssuedAt(server string, at time.Time) *BeamClaims {
	c := &BeamClaims{ServerUUID: server}
	c.RegisteredClaims = jwt.RegisteredClaims{
		Issuer:    BeamIssuer,
		IssuedAt:  jwt.NewNumericDate(at),
		ExpiresAt: jwt.NewNumericDate(at.Add(BeamTicketTTL)),
	}
	return c
}

// The window this closes: a ticket is a bearer token nothing re-reads for 30
// minutes, so removing someone's access stopped NEW tickets and left an
// outstanding one working. Core stamps the server; a ticket minted before the
// stamp no longer opens a session.
func TestATicketMintedBeforeAnAccessChangeIsStale(t *testing.T) {
	rdb, _ := epochFixture(t)
	ctx := context.Background()
	const srv = "srv-1"

	older := ticketIssuedAt(srv, time.Now().Add(-2*time.Minute))

	t.Run("with no stamp it is fine", func(t *testing.T) {
		stale, err := TicketPredatesAccessChange(ctx, rdb, older)
		if err != nil || stale {
			t.Fatalf("stale=%v err=%v, want a clean pass", stale, err)
		}
	})

	if err := BumpAccessEpoch(ctx, rdb, srv); err != nil {
		t.Fatalf("bump: %v", err)
	}

	t.Run("the older ticket is refused", func(t *testing.T) {
		stale, err := TicketPredatesAccessChange(ctx, rdb, older)
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if !stale {
			t.Error("a ticket issued before the access change is still accepted")
		}
	})

	t.Run("a freshly minted one is not", func(t *testing.T) {
		newer := ticketIssuedAt(srv, time.Now().Add(time.Second))
		stale, err := TicketPredatesAccessChange(ctx, rdb, newer)
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		if stale {
			t.Error("the stamp refused a ticket minted after it - nobody could reconnect")
		}
	})

	t.Run("another server is untouched", func(t *testing.T) {
		other := ticketIssuedAt("srv-2", time.Now().Add(-2*time.Minute))
		stale, err := TicketPredatesAccessChange(ctx, rdb, other)
		if err != nil || stale {
			t.Errorf("stamping one server invalidated another: stale=%v err=%v", stale, err)
		}
	})
}

// Fails OPEN, deliberately: an outage must not stand between a customer and
// their own files, and what it reopens is the window that existed before this
// existed at all. The error is RETURNED so the caller can log it rather than
// treat "could not ask" as "fine".
func TestAnUnreachableRedisDoesNotLockAnyoneOut(t *testing.T) {
	rdb, mr := epochFixture(t)
	mr.Close()
	stale, err := TicketPredatesAccessChange(context.Background(), rdb, ticketIssuedAt("srv-1", time.Now()))
	if stale {
		t.Error("a Redis outage refused a valid ticket")
	}
	if err == nil {
		t.Error("the failure was swallowed; the node cannot say it could not check")
	}
}

// Open sessions re-check the stamp, and the node caps a session at a day. A
// stamp that expired while a session sat idle - it used to live 35 minutes -
// answered "nothing changed" and gave a revoked session its rights back.
func TestTheStampOutlivesTheLongestSession(t *testing.T) {
	rdb, mr := epochFixture(t)
	if err := BumpAccessEpoch(context.Background(), rdb, "srv-1"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if ttl := mr.TTL(AccessEpochKey("srv-1")); ttl <= 24*time.Hour {
		t.Errorf("stamp TTL %v does not outlive a day-long session", ttl)
	}
}

// A plain SET let a delayed earlier stamp (or a Core replica with a slower
// clock) land over a later one and move the epoch BACK, reviving tickets the
// later change had made stale.
func TestTheStampNeverMovesBackwards(t *testing.T) {
	rdb, mr := epochFixture(t)
	key := AccessEpochKey("srv-1")
	cases := []struct {
		name        string
		offset      time.Duration // stored stamp relative to now
		wantForward bool          // true: the bump writes now; false: stored stays
	}{
		{"a later stamp already there stays", time.Hour, false},
		{"an earlier stamp is moved forward", -time.Hour, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stored := time.Now().Add(c.offset).Unix()
			mr.Set(key, strconv.FormatInt(stored, 10))
			mr.SetTTL(key, time.Minute)
			before := time.Now().Unix()
			if err := BumpAccessEpoch(context.Background(), rdb, "srv-1"); err != nil {
				t.Fatalf("bump: %v", err)
			}
			got, err := rdb.Get(context.Background(), key).Int64()
			if err != nil {
				t.Fatalf("read stamp: %v", err)
			}
			if !c.wantForward && got != stored {
				t.Errorf("stamp moved from %d back to %d", stored, got)
			}
			if c.wantForward && (got < before || got > time.Now().Unix()) {
				t.Errorf("stamp %d was not moved forward to now (%d)", got, before)
			}
			if ttl := mr.TTL(key); ttl <= 24*time.Hour {
				t.Errorf("TTL %v was not re-armed", ttl)
			}
		})
	}
}

// A ticket minted in the same second as the change cannot be told apart from
// one minted just before it, and is refused.
func TestATicketFromTheSameSecondAsTheChangeIsStale(t *testing.T) {
	rdb, mr := epochFixture(t)
	now := time.Now()
	mr.Set(AccessEpochKey("srv-1"), strconv.FormatInt(now.Unix(), 10))
	c := &BeamClaims{ServerUUID: "srv-1"}
	c.IssuedAt = jwt.NewNumericDate(now)
	if stale, err := TicketPredatesAccessChange(context.Background(), rdb, c); err != nil || !stale {
		t.Fatalf("same-second ticket: stale=%v err=%v", stale, err)
	}
}

// Stamping every server goes through batched pipelines; every server across a
// batch boundary must be stamped, an empty uuid skipped, and a stamp still only
// moves forward.
func TestBumpAccessEpochsStampsEveryServerAcrossBatches(t *testing.T) {
	rdb, mr := epochFixture(t)
	ctx := context.Background()
	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	if err := mr.Set(AccessEpochKey("srv-0"), future); err != nil {
		t.Fatal(err)
	}
	uuids := []string{""}
	for i := 0; i < bumpBatch+3; i++ {
		uuids = append(uuids, "srv-"+strconv.Itoa(i))
	}
	if err := BumpAccessEpochs(ctx, rdb, uuids); err != nil {
		t.Fatalf("bump: %v", err)
	}
	for _, u := range uuids[1:] {
		if !mr.Exists(AccessEpochKey(u)) {
			t.Fatalf("%s not stamped", u)
		}
	}
	if mr.Exists(AccessEpochKey("")) {
		t.Error("an empty uuid was stamped")
	}
	if got, _ := mr.Get(AccessEpochKey("srv-0")); got != future {
		t.Errorf("srv-0 stamp = %s, want the newer %s kept", got, future)
	}
	if ttl := mr.TTL(AccessEpochKey("srv-1")); ttl <= 0 {
		t.Errorf("srv-1 has no TTL")
	}
}
