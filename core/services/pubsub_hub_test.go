package services

import (
	"context"
	"testing"
	"time"
)

func recvWithin(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no message, want %q", want)
	}
}

func recvSkippingWarm(t *testing.T, ch <-chan string, want string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == "warm" {
				continue
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			return
		case <-deadline:
			t.Fatalf("no message, want %q", want)
		}
	}
}

// publishUntilSeen republishes until the first subscriber sees it: SUBSCRIBE
// is not acknowledged synchronously, so the first publish can race it.
func publishUntilSeen(t *testing.T, pub func(), ch <-chan string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pub()
		select {
		case got := <-ch:
			return got
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("subscription never became live")
	return ""
}

func TestPubSubHubSharesOneConnectionAndFansOut(t *testing.T) {
	_, rdb := newTestRedis(t)
	ctx := context.Background()
	hub := NewPubSubHub(rdb)

	a, stopA := hub.Subscribe("chan:a")
	b, stopB := hub.Subscribe("chan:a")
	other, stopOther := hub.Subscribe("chan:other")

	publishUntilSeen(t, func() { rdb.Publish(ctx, "chan:a", "warm") }, a)
	publishUntilSeen(t, func() { rdb.Publish(ctx, "chan:other", "warm") }, other)

	// Warm-up copies can still be in flight to a and b (and other), so skip
	// them rather than draining what happens to be buffered right now.
	rdb.Publish(ctx, "chan:a", "x")
	recvSkippingWarm(t, a, "x")
	recvSkippingWarm(t, b, "x")
	quiet := time.After(100 * time.Millisecond)
	for done := false; !done; {
		select {
		case got := <-other:
			if got != "warm" {
				t.Fatalf("other channel got %q from chan:a", got)
			}
		case <-quiet:
			done = true
		}
	}

	// One leaving keeps the channel for the rest.
	stopA()
	stopA() // idempotent
	rdb.Publish(ctx, "chan:a", "y")
	recvWithin(t, b, "y")
	if n, conn := hub.listening(); n != 2 || !conn {
		t.Fatalf("listening = %d,%v; want 2,true", n, conn)
	}

	stopB()
	if n, conn := hub.listening(); n != 1 || !conn {
		t.Fatalf("after last chan:a subscriber: %d,%v; want 1,true", n, conn)
	}
	stopOther()
	if n, conn := hub.listening(); n != 0 || conn {
		t.Fatalf("after all left: %d,%v; want 0,false (connection released)", n, conn)
	}

	// The hub reconnects for the next stream.
	c, stopC := hub.Subscribe("chan:a")
	defer stopC()
	if got := publishUntilSeen(t, func() { rdb.Publish(ctx, "chan:a", "z") }, c); got != "z" {
		t.Fatalf("got %q after reconnect", got)
	}
}

func TestPubSubHubDropsForASlowSubscriberWithoutBlockingOthers(t *testing.T) {
	_, rdb := newTestRedis(t)
	ctx := context.Background()
	hub := NewPubSubHub(rdb)

	slow, stopSlow := hub.Subscribe("chan:s")
	defer stopSlow()
	fast, stopFast := hub.Subscribe("chan:s")
	defer stopFast()
	publishUntilSeen(t, func() { rdb.Publish(ctx, "chan:s", "warm") }, fast)

	for i := 0; i < hubSubscriberBuffer+50; i++ {
		rdb.Publish(ctx, "chan:s", "m")
		<-fast // the fast one keeps up although slow never reads
	}
	if len(slow) != cap(slow) {
		t.Fatalf("slow buffer = %d, want full at %d", len(slow), cap(slow))
	}
}
