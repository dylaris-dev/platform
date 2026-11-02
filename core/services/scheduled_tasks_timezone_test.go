package services

import (
	"testing"
	"time"
)

// A schedule is read in the task's own zone, across a DST change, and a zone
// that is not a real IANA name never reaches the cron parser.
func TestComputeNextRunInReadsTheScheduleInTheTasksZone(t *testing.T) {
	cases := []struct {
		name, cron, tz string
		from, want     time.Time
		wantErr        bool
	}{
		// CEST is UTC+2: 04:00 Berlin is 02:00 UTC.
		{"summer Berlin", "0 4 * * *", "Europe/Berlin",
			time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC), time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC), false},
		// After the switch to CET (25 Oct 2026) it is UTC+1: 03:00 UTC.
		{"winter Berlin", "0 4 * * *", "Europe/Berlin",
			time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC), false},
		{"New York", "30 9 * * *", "America/New_York",
			time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC), time.Date(2026, 7, 15, 13, 30, 0, 0, time.UTC), false},
		{"empty is UTC", "0 4 * * *", "",
			time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC), time.Date(2026, 7, 16, 4, 0, 0, 0, time.UTC), false},
		{"unknown zone", "0 4 * * *", "Mars/Olympus", time.Time{}, time.Time{}, true},
		{"host zone refused", "0 4 * * *", "Local", time.Time{}, time.Time{}, true},
		{"path refused", "0 4 * * *", "../../etc/passwd", time.Time{}, time.Time{}, true},
		{"zone smuggled with a space", "0 4 * * *", "UTC 0 0 * * *", time.Time{}, time.Time{}, true},
		{"prefix in the spec still refused", "CRON_TZ=Asia/Tokyo 0 4 * * *", "UTC", time.Time{}, time.Time{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ComputeNextRunIn(c.cron, c.tz, c.from)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ComputeNextRunIn: %v", err)
			}
			if !got.Equal(c.want) {
				t.Fatalf("got %v, want %v", got.UTC(), c.want)
			}
		})
	}
}

// Berlin 2026-10-25, 03:00 CEST -> 02:00 CET: a task every five minutes keeps
// running through both occurrences of 02:xx, with no hour skipped.
func TestComputeNextRunInFallBackEveryFiveMinutes(t *testing.T) {
	from := time.Date(2026, 10, 24, 23, 50, 0, 0, time.UTC) // 01:50 CEST
	end := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)    // 04:00 CET
	prev := from
	n := 0
	for {
		next, err := ComputeNextRunIn("*/5 * * * *", "Europe/Berlin", prev)
		if err != nil {
			t.Fatal(err)
		}
		if !next.After(end) && next.Sub(prev) != 5*time.Minute {
			t.Fatalf("gap %s after %v", next.Sub(prev), prev.UTC())
		}
		if next.After(end) {
			break
		}
		prev = next
		n++
	}
	if n != 38 { // 3h10m of absolute time in 5-minute steps
		t.Fatalf("%d runs, want 38", n)
	}
	// Hourly on the hour: 02:00 CEST, then 02:00 CET one hour later.
	next, _ := ComputeNextRunIn("0 * * * *", "Europe/Berlin", time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	if !next.Equal(time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("hourly after 02:00 CEST = %v, want 01:00 UTC (02:00 CET)", next.UTC())
	}
}
