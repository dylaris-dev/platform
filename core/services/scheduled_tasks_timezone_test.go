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
