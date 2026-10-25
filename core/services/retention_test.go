package services

import (
	"testing"
	"time"
)

func TestAddRetention(t *testing.T) {
	base := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		spec   string
		want   time.Time
		wantOK bool
	}{
		{"3d", time.Date(2026, 1, 18, 12, 0, 0, 0, time.UTC), true},
		{"2w", time.Date(2026, 1, 29, 12, 0, 0, 0, time.UTC), true},
		{"3m", time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC), true},
		{"0d", base, true},
		{"", base, false},
		{"x", base, false},
		{"3", base, false},
		{"-1d", base, false},
		{"d", base, false},
	}
	for _, c := range cases {
		got, ok := AddRetention(base, c.spec)
		if ok != c.wantOK {
			t.Errorf("AddRetention(%q) ok=%v, want %v", c.spec, ok, c.wantOK)
			continue
		}
		if ok && !got.Equal(c.want) {
			t.Errorf("AddRetention(%q) = %v, want %v", c.spec, got, c.want)
		}
	}
}

// A day is 24 hours on the day a local zone leaves DST, too: in Europe/Berlin
// that day is 25 hours long, and a retention measured on the wall clock ran an
// hour past its deadline (seen at 02:08 CEST on 2026-10-25).
func TestAddRetentionIgnoresTheLocalDSTChange(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	for _, at := range []time.Time{
		time.Date(2026, 10, 24, 12, 0, 0, 0, berlin), // the next day is 25h long
		time.Date(2026, 3, 28, 12, 0, 0, 0, berlin),  // the next day is 23h long
	} {
		got, ok := AddRetention(at, "1d")
		if !ok || got.Sub(at) != 24*time.Hour {
			t.Errorf("AddRetention(%v, 1d) = %v, %v later; want 24h", at, got, got.Sub(at))
		}
	}
}

func TestValidRetentionSpec(t *testing.T) {
	for _, s := range []string{"3d", "2w", "3m", "10d"} {
		if !ValidRetentionSpec(s) {
			t.Errorf("ValidRetentionSpec(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "x", "3", "3y", "-1d"} {
		if ValidRetentionSpec(s) {
			t.Errorf("ValidRetentionSpec(%q) = true, want false", s)
		}
	}
}
