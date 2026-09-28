package auth

import (
	"context"
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

// The stamp is kept only as long as a ticket could still be young enough for it
// to matter. Anything older is refused by its own expiry.
func TestTheStampExpiresWithTheTicketsItCouldRefuse(t *testing.T) {
	rdb, mr := epochFixture(t)
	if err := BumpAccessEpoch(context.Background(), rdb, "srv-1"); err != nil {
		t.Fatalf("bump: %v", err)
	}
	ttl := mr.TTL(AccessEpochKey("srv-1"))
	if ttl <= BeamTicketTTL {
		t.Errorf("stamp TTL %v does not outlive a ticket (%v); a ticket could survive the stamp that refuses it", ttl, BeamTicketTTL)
	}
}
