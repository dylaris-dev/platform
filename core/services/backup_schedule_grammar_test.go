package services

import (
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func rfc(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The forms beside the original one: each is refused with a reason a user can
// act on, and each valid one is accepted in any zone Go can load.
func TestCheckBackupScheduleGrammar(t *testing.T) {
	valid := []string{
		"every 6h from 12:00", "every 1h from 00:15", "every 24h from 03:00", "every 8h from 23:59",
		"daily 00:00", "daily 3:30", "weekly sun 23:59", "weekly mon 04:00",
		"cron 0 */6 * * *", "cron 30 2 * * 1-5", "cron 0 4 1,15 * *", "cron 15 3 * * sun",
		"  daily   03:00  ",
	}
	for _, s := range valid {
		for _, tz := range []string{"", "UTC", "Europe/Berlin", "America/New_York"} {
			if err := CheckBackupSchedule(s, tz); err != nil {
				t.Errorf("CheckBackupSchedule(%q, %q) = %v, want nil", s, tz, err)
			}
		}
	}
	invalid := []struct{ schedule, tz, why string }{
		{"every 5h from 12:00", "", "1, 2, 3, 4, 6, 8, 12 or 24"},
		{"every 48h from 12:00", "", "1, 2, 3, 4, 6, 8, 12 or 24"},
		{"every 1d from 12:00", "", "1, 2, 3, 4, 6, 8, 12 or 24"},
		{"every 0h from 12:00", "", "1, 2, 3, 4, 6, 8, 12 or 24"},
		{"every 6h from 25:00", "", "invalid time"},
		{"every 6h from noon", "", "invalid time"},
		{"every 6h at 12:00", "", "Schedule must be"},
		{"daily", "", "Schedule must be"},
		{"daily 24:00", "", "invalid time"},
		{"daily 03:60", "", "invalid time"},
		{"daily 3pm", "", "invalid time"},
		{"weekly funday 03:00", "", "unknown weekday"},
		{"weekly Mon 03:00", "", "unknown weekday"},
		{"weekly mon", "", "Schedule must be"},
		{"cron * * * * *", "", "at most once an hour"},
		{"cron */15 * * * *", "", "at most once an hour"},
		{"cron 0,30 * * * *", "", "at most once an hour"},
		{"cron 0 * * *", "", "exactly five fields"},
		{"cron @daily", "", "exactly five fields"},
		{"cron TZ=UTC 0 * * *", "", "time zone in its own field"},
		{"cron 0 0 * * 8", "", "invalid cron"},
		{"cron 0 0 30 2 *", "", "never runs"},
		{"0 3 * * *", "", "Schedule must be"},
		{"daily 03:00", "Mars/Base", "invalid time zone"},
		{"daily 03:00", "Local", "invalid time zone"},
		{"every 6h", "../../etc/passwd", "invalid time zone"},
		{"cron 0 3 * * * " + strings.Repeat("x", 130), "", "longer than"},
	}
	for _, tc := range invalid {
		err := CheckBackupSchedule(tc.schedule, tc.tz)
		if err == nil || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("CheckBackupSchedule(%q, %q) = %v, want an error saying %q", tc.schedule, tc.tz, err, tc.why)
		}
	}
}

// A schedule stored before strict validation existed keeps the reading the
// original parser gave it, so no job changes behaviour on upgrade.
func TestStoredLooseScheduleKeepsItsOldReading(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := ComputeBackupNextRun("every 6h at 12:00", "", from)
	if got == nil || !got.Equal(from.Add(6*time.Hour)) {
		t.Fatalf("got %v, want from + 6h as before", got)
	}
	// And the relative form never reads the zone.
	got = ComputeBackupNextRun("every 6h", "Asia/Tokyo", from)
	if got == nil || !got.Equal(from.Add(6*time.Hour)) {
		t.Fatalf("got %v, want from + 6h whatever the zone", got)
	}
}

func TestComputeBackupNextRunWallClock(t *testing.T) {
	cases := []struct {
		name, schedule, tz, from, want string
	}{
		{"anchor later today", "every 6h from 12:00", "UTC", "2026-01-01T07:00:00Z", "2026-01-01T12:00:00Z"},
		{"strictly after the anchor", "every 6h from 12:00", "UTC", "2026-01-01T12:00:00Z", "2026-01-01T18:00:00Z"},
		{"wraps to midnight", "every 6h from 12:00", "UTC", "2026-01-01T19:00:00Z", "2026-01-02T00:00:00Z"},
		{"anchor minutes", "every 6h from 14:30", "UTC", "2026-01-01T03:00:00Z", "2026-01-01T08:30:00Z"},
		{"anchor in a zone", "every 8h from 01:00", "Europe/Berlin", "2026-01-01T00:00:00Z", "2026-01-01T08:00:00Z"},
		{"daily in summer time", "daily 03:00", "Europe/Berlin", "2026-07-01T00:00:00Z", "2026-07-01T01:00:00Z"},
		{"daily already passed", "daily 03:00", "Europe/Berlin", "2026-07-01T02:00:00Z", "2026-07-02T01:00:00Z"},
		{"weekly next monday", "weekly mon 04:15", "America/New_York", "2026-11-04T12:00:00Z", "2026-11-09T09:15:00Z"},
		{"weekly today, not yet", "weekly wed 23:00", "UTC", "2026-11-04T12:00:00Z", "2026-11-04T23:00:00Z"},
		{"cron first of the month", "cron 0 3 1 * *", "UTC", "2026-01-15T00:00:00Z", "2026-02-01T03:00:00Z"},
		{"cron weekdays skips the weekend", "cron 0 4 * * 1-5", "UTC", "2026-01-02T05:00:00Z", "2026-01-05T04:00:00Z"},
		// Both day fields restricted: either may match (the classic cron OR).
		{"cron dom OR dow", "cron 0 0 13 * 5", "UTC", "2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z"},
		{"cron leap day", "cron 0 0 29 2 *", "UTC", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeBackupNextRun(tc.schedule, tc.tz, rfc(t, tc.from))
			if got == nil || !got.Equal(rfc(t, tc.want)) {
				t.Errorf("ComputeBackupNextRun(%q, %q, %s) = %v, want %s", tc.schedule, tc.tz, tc.from, got, tc.want)
			}
		})
	}
}

// The DST rule: a wall time that happens twice runs at the first, one that does
// not exist runs shifted forward by the gap. Either way a daily run happens
// exactly once on the day of the change.
func TestBackupScheduleDST(t *testing.T) {
	cases := []struct {
		name, schedule, tz, from string
		want                     []string
	}{
		// Berlin 2026-03-29: 02:00 CET -> 03:00 CEST. 02:30 does not exist and runs at 03:30 CEST.
		{"Berlin spring", "daily 02:30", "Europe/Berlin", "2026-03-28T12:00:00Z",
			[]string{"2026-03-29T01:30:00Z", "2026-03-30T00:30:00Z", "2026-03-31T00:30:00Z"}},
		// Berlin 2026-10-25: 03:00 CEST -> 02:00 CET. 02:30 happens twice; only the first (CEST) runs.
		{"Berlin autumn", "daily 02:30", "Europe/Berlin", "2026-10-24T12:00:00Z",
			[]string{"2026-10-25T00:30:00Z", "2026-10-26T01:30:00Z", "2026-10-27T01:30:00Z"}},
		// New York 2026-03-08: 02:00 EST -> 03:00 EDT.
		{"New York spring", "daily 02:30", "America/New_York", "2026-03-07T12:00:00Z",
			[]string{"2026-03-08T07:30:00Z", "2026-03-09T06:30:00Z"}},
		// New York 2026-11-01: 02:00 EDT -> 01:00 EST. 01:30 happens twice; the EDT one runs.
		{"New York autumn", "daily 01:30", "America/New_York", "2026-10-31T12:00:00Z",
			[]string{"2026-11-01T05:30:00Z", "2026-11-02T06:30:00Z"}},
		{"New York autumn weekly", "weekly sun 01:30", "America/New_York", "2026-10-31T12:00:00Z",
			[]string{"2026-11-01T05:30:00Z", "2026-11-08T06:30:00Z"}},
		{"New York spring cron", "cron 30 2 * * *", "America/New_York", "2026-03-07T12:00:00Z",
			[]string{"2026-03-08T07:30:00Z", "2026-03-09T06:30:00Z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BackupNextRuns(tc.schedule, tc.tz, rfc(t, tc.from), len(tc.want))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d runs %v, want %v", len(got), got, tc.want)
			}
			for i, w := range tc.want {
				if !got[i].Equal(rfc(t, w)) {
					t.Errorf("run %d = %s, want %s", i, got[i].UTC().Format(time.RFC3339), w)
				}
			}
		})
	}

	// Hourly on the wall clock: the short day has 23 runs, the long day 25 (an
	// hourly job runs in both occurrences of the repeated hour, so there is no
	// two-hour hole), and no two runs share an instant.
	berlin := mustLoc(t, "Europe/Berlin")
	for _, day := range []struct {
		date string
		want int
	}{{"2026-03-29", 23}, {"2026-10-25", 25}, {"2026-06-01", 24}} {
		start, err := time.ParseInLocation("2006-01-02", day.date, berlin)
		if err != nil {
			t.Fatal(err)
		}
		for _, sched := range []string{"every 1h from 00:00", "cron 0 * * * *"} {
			runs := BackupNextRuns(sched, "Europe/Berlin", start.Add(-time.Minute), 30)
			n := 0
			for i, r := range runs {
				if i > 0 && !r.After(runs[i-1]) {
					t.Fatalf("%s: runs not strictly increasing at %d: %v", day.date, i, runs)
				}
				if r.In(berlin).Format("2006-01-02") == day.date {
					n++
				}
			}
			if n != day.want {
				t.Errorf("%s %q: %d hourly runs, want %d", day.date, sched, n, day.want)
			}
		}
	}
}

// Berlin 2026-10-25, 03:00 CEST -> 02:00 CET: an hourly backup runs at 02:00
// CEST and again at 02:00 CET, one hour apart, while a daily 02:00 runs once.
func TestBackupScheduleFallBackHourlyRunsBothOccurrences(t *testing.T) {
	from := rfc(t, "2026-10-24T23:30:00Z") // 01:30 CEST
	got := BackupNextRuns("cron 0 * * * *", "Europe/Berlin", from, 3)
	want := []string{"2026-10-25T00:00:00Z", "2026-10-25T01:00:00Z", "2026-10-25T02:00:00Z"}
	for i, w := range want {
		if i >= len(got) || !got[i].Equal(rfc(t, w)) {
			t.Fatalf("hourly runs = %v, want %v", got, want)
		}
	}
	daily := BackupNextRuns("daily 02:00", "Europe/Berlin", from, 2)
	if len(daily) != 2 || !daily[0].Equal(rfc(t, "2026-10-25T00:00:00Z")) || !daily[1].Equal(rfc(t, "2026-10-26T01:00:00Z")) {
		t.Fatalf("daily runs = %v, want only the first 02:00", daily)
	}
	// A fixed hour stays first-occurrence-only for cron as well.
	if got := ComputeBackupNextRun("cron 0 2 * * *", "Europe/Berlin", rfc(t, "2026-10-25T00:00:00Z")); got == nil || !got.Equal(rfc(t, "2026-10-26T01:00:00Z")) {
		t.Fatalf("cron 0 2 after the first 02:00 = %v, want the next day", got)
	}
}

// "every 5h from 12:00" is refused when saved, but a row stored before the
// anchored form existed was read as plain "every 5h" and keeps running so.
func TestStoredAnchorThatDoesNotDivideTheDayKeepsTheRelativeReading(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range []string{"every 5h from 12:00", "every 7h from 25:00", "every 1d from 03:00"} {
		every, _, err := parseBackupSchedule(s, false)
		if err != nil {
			t.Fatalf("%q non-strict: %v", s, err)
		}
		got := ComputeBackupNextRun(s, "Europe/Berlin", from)
		if got == nil || !got.Equal(from.Add(every)) {
			t.Fatalf("%q: next = %v, want from + %s", s, got, every)
		}
	}
	if got := ComputeBackupNextRun("every 5h from 12:00", "", from); got == nil || !got.Equal(from.Add(5*time.Hour)) {
		t.Fatalf("every 5h from 12:00 = %v, want from + 5h", got)
	}
}

// The wall-clock walk used to resolve all 1440 minutes of a day through the
// zone database for "* * * * *". 1000 next-run computations must stay cheap.
func TestNextWallClockRunIsCheap(t *testing.T) {
	from := rfc(t, "2026-07-01T10:17:30Z")
	start := time.Now()
	for i := 0; i < 1000; i++ {
		next, err := ComputeNextRunIn("* * * * *", "Europe/Berlin", from)
		if err != nil || !next.Equal(rfc(t, "2026-07-01T10:18:00Z")) {
			t.Fatalf("next = %v, %v", next, err)
		}
		if _, err := ComputeNextRunIn("0 3 1 1 *", "America/New_York", from); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("1000 next-run computations took %s", d)
	}
}
