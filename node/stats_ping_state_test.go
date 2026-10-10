package main

import (
	"errors"
	"testing"
)

func TestNextPingStateForgetsAfterRepeatedFailures(t *testing.T) {
	good := &SLPResponse{}
	fail := errors.New("timeout")

	last, fails := nextPingState(nil, 0, good, nil)
	if last != good || fails != 0 {
		t.Fatalf("success: got %v/%d", last, fails)
	}
	for i := 1; i < pingFailsToForget; i++ {
		last, fails = nextPingState(last, fails, nil, fail)
		if last != good {
			t.Fatalf("failure %d dropped the last answer too early", i)
		}
	}
	last, fails = nextPingState(last, fails, nil, fail)
	if last != nil {
		t.Fatalf("after %d failures the stale answer must be gone", fails)
	}
	if last, fails = nextPingState(last, fails, good, nil); last != good || fails != 0 {
		t.Fatalf("a later success must restore: got %v/%d", last, fails)
	}
}
