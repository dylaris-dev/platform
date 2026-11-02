package services

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Backup schedules: one string per job, read in the job's IANA time zone.
//
//	manual                       no automatic runs
//	every <n>h | every <n>d      relative: n hours or days after the previous slot
//	every <n>h from HH:MM        on the wall clock: HH:MM and every n hours around
//	                             the day from it; n divides 24 ("every 6h from
//	                             12:00" runs at 00:00, 06:00, 12:00 and 18:00)
//	daily HH:MM
//	weekly <mon..sun> HH:MM
//	cron <min> <hour> <dom> <month> <dow>
//
// The relative form is the original one and is read exactly as before, with no
// zone involved. Every other form follows the wall clock in the job's zone, and
// a daylight-saving change is resolved like Temporal's "compatible" rule: a wall
// time that happens twice (autumn) runs at the first of the two, one that does
// not exist (spring) runs shifted forward by the gap, 02:30 -> 03:30. Either
// way a daily 02:30 runs exactly once on the day of the change. robfig/cron's
// own Next is not used for this: it skips such a run in spring and runs it
// twice in autumn.

// BackupScheduleMaxLen bounds what is stored; the column is TEXT, so without
// this an arbitrarily long string would be parsed and kept.
const BackupScheduleMaxLen = 128

// BackupScheduleHelp is the grammar in one sentence, for error messages.
const BackupScheduleHelp = `Schedule must be "manual", "every <n>h", "every <n>d", "every <n>h from HH:MM", "daily HH:MM", "weekly <mon-sun> HH:MM" or "cron <minute> <hour> <day> <month> <weekday>"`

// No Descriptor: "@every 1m" would be a backup a minute, and "@daily" has its
// own form here.
var backupCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// cronStarBit is robfig's marker for a field written as "*" (unexported there).
const cronStarBit = 1 << 63

var backupWeekdays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// parseBackupSchedule returns a relative interval, or a wall-clock spec, or
// neither for manual. strict refuses the loose input the original parser let
// through ("every 6h at 12:00" read as plain "every 6h"); stored rows are read
// non-strict so a job saved under the old rules keeps running as it did.
func parseBackupSchedule(schedule string, strict bool) (time.Duration, *cron.SpecSchedule, error) {
	s := strings.TrimSpace(schedule)
	if s == "" || s == "manual" {
		return 0, nil, nil
	}
	if len(s) > BackupScheduleMaxLen {
		return 0, nil, fmt.Errorf("the schedule is longer than %d characters", BackupScheduleMaxLen)
	}
	f := strings.Fields(s)
	var spec string
	switch {
	case f[0] == "every" && len(f) == 4 && f[2] == "from":
		n, err := strconv.Atoi(strings.TrimSuffix(f[1], "h"))
		if !strings.HasSuffix(f[1], "h") || err != nil || n <= 0 || 24%n != 0 {
			return 0, nil, errors.New(`"every <n>h from HH:MM" needs n to be 1, 2, 3, 4, 6, 8, 12 or 24`)
		}
		hh, mm, err := parseBackupClock(f[3])
		if err != nil {
			return 0, nil, err
		}
		hours := make([]string, 0, 24/n)
		for h := hh % n; h < 24; h += n {
			hours = append(hours, strconv.Itoa(h))
		}
		spec = fmt.Sprintf("%d %s * * *", mm, strings.Join(hours, ","))
	case f[0] == "daily" && len(f) == 2:
		hh, mm, err := parseBackupClock(f[1])
		if err != nil {
			return 0, nil, err
		}
		spec = fmt.Sprintf("%d %d * * *", mm, hh)
	case f[0] == "weekly" && len(f) == 3:
		day, ok := backupWeekdays[f[1]]
		if !ok {
			return 0, nil, fmt.Errorf("unknown weekday %q: use mon, tue, wed, thu, fri, sat or sun", f[1])
		}
		hh, mm, err := parseBackupClock(f[2])
		if err != nil {
			return 0, nil, err
		}
		spec = fmt.Sprintf("%d %d * * %d", mm, hh, day)
	case f[0] == "cron":
		if len(f) != 6 {
			return 0, nil, errors.New("a cron schedule has exactly five fields: minute hour day-of-month month day-of-week")
		}
		spec = strings.Join(f[1:], " ")
		// robfig reads a TZ= / CRON_TZ= prefix itself whatever the options, and
		// "TZ=x" without a space panics it. The zone is the job's own field.
		if strings.Contains(spec, "=") {
			return 0, nil, errors.New("set the time zone in its own field, not in the schedule")
		}
	case f[0] == "every":
		if strict && len(f) != 2 {
			return 0, nil, errors.New(BackupScheduleHelp)
		}
		// The original parser, unchanged, so stored relative schedules read as
		// they always have.
		var n int
		var unit string
		if _, err := fmt.Sscanf(s, "every %d%s", &n, &unit); err != nil || n <= 0 {
			return 0, nil, errors.New(BackupScheduleHelp)
		}
		switch unit {
		case "h":
			return time.Duration(n) * time.Hour, nil, nil
		case "d":
			return time.Duration(n) * 24 * time.Hour, nil, nil
		}
		return 0, nil, errors.New(BackupScheduleHelp)
	default:
		return 0, nil, errors.New(BackupScheduleHelp)
	}
	parsed, err := backupCronParser.Parse(spec)
	if err != nil {
		return 0, nil, fmt.Errorf("invalid cron: %w", err)
	}
	ss, ok := parsed.(*cron.SpecSchedule)
	if !ok {
		return 0, nil, errors.New("invalid cron")
	}
	// A backup a minute would fill storage and overlap its own runs. One minute
	// value caps it at once an hour, the same floor "every 1h" always had.
	if bits.OnesCount64(ss.Minute&^cronStarBit) != 1 {
		return 0, nil, errors.New("a backup runs at most once an hour: give the minute field a single value, for example 0")
	}
	return 0, ss, nil
}

// parseBackupClock reads a 24-hour HH:MM.
func parseBackupClock(s string) (int, int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid time %q: use 24-hour HH:MM, for example 03:30", s)
	}
	return t.Hour(), t.Minute(), nil
}

func backupLocation(tz string) (*time.Location, error) {
	if tz == "" {
		return time.UTC, nil // a job from before zones existed
	}
	if !ValidTimezone(tz) {
		return nil, fmt.Errorf("invalid time zone %q", tz)
	}
	return time.LoadLocation(tz)
}

// CheckBackupSchedule says why a schedule cannot be stored, or nil. Defined in
// terms of the same parser and next-run search the scheduler uses, so what is
// accepted is exactly what runs - the failure this exists to stop is a job that
// is listed, enabled and never runs.
func CheckBackupSchedule(schedule, tz string) error {
	if _, err := backupLocation(tz); err != nil {
		return err
	}
	every, spec, err := parseBackupSchedule(schedule, true)
	if err != nil {
		return err
	}
	if every == 0 && spec != nil && ComputeBackupNextRun(schedule, tz, time.Now()) == nil {
		return errors.New("this schedule never runs")
	}
	return nil
}

// ComputeBackupNextRun returns the first slot strictly after from, or nil for a
// schedule that does not run (manual, or one nothing can parse) - callers must
// act on nil rather than store the job regardless.
func ComputeBackupNextRun(schedule, tz string, from time.Time) *time.Time {
	every, spec, err := parseBackupSchedule(schedule, false)
	if err != nil {
		return nil
	}
	if every > 0 {
		next := from.Add(every)
		return &next
	}
	if spec == nil {
		return nil
	}
	loc, err := backupLocation(tz)
	if err != nil {
		return nil
	}
	next, ok := nextWallClockRun(spec, loc, from)
	if !ok {
		return nil
	}
	return &next
}

// BackupNextRuns lists the next n slots after from, for the panel's preview.
func BackupNextRuns(schedule, tz string, from time.Time, n int) []time.Time {
	out := make([]time.Time, 0, n)
	for len(out) < n {
		next := ComputeBackupNextRun(schedule, tz, from)
		if next == nil {
			break
		}
		out = append(out, *next)
		from = *next
	}
	return out
}

// nextWallClockRun walks calendar days in loc and resolves each matching wall
// time to an instant, so a DST change is decided per wall time (see the top of
// this file) rather than by stepping hours in absolute time.
func nextWallClockRun(spec *cron.SpecSchedule, loc *time.Location, from time.Time) (time.Time, bool) {
	y, m, d := from.In(loc).Date()
	// Five years, the same search limit robfig uses: "cron 0 0 30 2 *" has to end.
	for i := 0; i <= 5*366; i++ {
		day := time.Date(y, m, d+i, 0, 0, 0, 0, time.UTC)
		if 1<<uint(day.Month())&spec.Month == 0 || !cronDayMatches(spec, day) {
			continue
		}
		var best time.Time
		for h := 0; h < 24; h++ {
			if 1<<uint(h)&spec.Hour == 0 {
				continue
			}
			for mi := 0; mi < 60; mi++ {
				if 1<<uint(mi)&spec.Minute == 0 {
					continue
				}
				// The minimum, not the first: a run shifted out of a spring gap
				// can land after a later wall time of the same day.
				t := wallInstant(day.Year(), day.Month(), day.Day(), h, mi, loc)
				if t.After(from) && (best.IsZero() || t.Before(best)) {
					best = t
				}
			}
		}
		if !best.IsZero() {
			return best, true
		}
	}
	return time.Time{}, false
}

// cronDayMatches is robfig's day rule: with either day field written as "*",
// both must match; otherwise either may (the classic cron OR).
func cronDayMatches(s *cron.SpecSchedule, day time.Time) bool {
	dom := 1<<uint(day.Day())&s.Dom > 0
	dow := 1<<uint(day.Weekday())&s.Dow > 0
	if s.Dom&cronStarBit > 0 || s.Dow&cronStarBit > 0 {
		return dom && dow
	}
	return dom || dow
}

// wallInstant resolves a wall time in loc: the earlier instant when it happens
// twice, and shifted forward by the gap when it does not happen at all. Go's
// time.Date leaves both cases unspecified, so they are decided here.
func wallInstant(y int, mo time.Month, d, h, mi int, loc *time.Location) time.Time {
	naive := time.Date(y, mo, d, h, mi, 0, 0, time.UTC)
	// The offsets on either side of any change on that day.
	_, before := naive.Add(-36 * time.Hour).In(loc).Zone()
	_, after := naive.Add(36 * time.Hour).In(loc).Zone()
	var found time.Time
	for _, off := range []int{before, after} {
		t := naive.Add(-time.Duration(off) * time.Second)
		lt := t.In(loc)
		if time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), 0, 0, time.UTC).Equal(naive) &&
			(found.IsZero() || t.Before(found)) {
			found = t
		}
	}
	if found.IsZero() {
		return naive.Add(-time.Duration(before) * time.Second)
	}
	return found
}
