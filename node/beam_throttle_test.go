package main

import (
	"context"
	"testing"
	"time"
)

// A request larger than the limiter's burst must be paced, not refused.
// x/time's WaitN fails outright when n > burst, and the burst is capped at
// 256 KiB while the panel uploads 512 KiB chunks - so any upload cap made
// every Beam upload fail on its first chunk.
func TestBeamThrottleWaitNLargerThanBurst(t *testing.T) {
	cases := []struct {
		name string
		bps  int64
		dir  Direction
		n    int
	}{
		{"upload chunk over the 256 KiB burst cap", 8 << 20, DirectionUp, 512 * 1024},
		{"download read over a small cap's burst", 100 * 1024, DirectionDown, 256 * 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bt := &BeamThrottle{}
			bt.setLimits(tc.bps, tc.bps)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := bt.WaitN(ctx, tc.dir, tc.n); err != nil {
				t.Fatalf("WaitN(%d) with a %d B/s cap: %v", tc.n, tc.bps, err)
			}
		})
	}
}
