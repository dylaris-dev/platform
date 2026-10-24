package tabs

import (
	"testing"
	"time"
)

// The whole point of parseShareExpiry is telling three states apart on a PATCH,
// which a plain string cannot do: absent means keep, "" means clear, an instant
// means set. Getting keep/clear the wrong way round is silent - the request
// still succeeds and the link just quietly stops (or never stops) expiring.
func TestParseShareExpiry(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	str := func(s string) *string { return &s }

	cases := []struct {
		name     string
		raw      *string
		wantSet  bool
		wantNil  bool
		wantErr  bool
		wantTime time.Time
	}{
		{name: "absent keeps whatever is stored", raw: nil, wantSet: false, wantNil: true},
		{name: "empty string clears the expiry", raw: str(""), wantSet: true, wantNil: true},
		{name: "blanks are an empty string", raw: str("   "), wantSet: true, wantNil: true},
		{
			name: "future instant is stored", raw: str("2026-09-01T08:30:00Z"),
			wantSet: true, wantTime: time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC),
		},
		{
			// A non-UTC offset must survive as the same instant, or a link set
			// from a browser in Berlin expires an hour off.
			name: "offset is normalised, not truncated", raw: str("2026-09-01T10:30:00+02:00"),
			wantSet: true, wantTime: time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC),
		},
		{name: "past is refused", raw: str("2026-08-23T11:59:59Z"), wantErr: true},
		{name: "now is refused - it is already used up", raw: str("2026-08-23T12:00:00Z"), wantErr: true},
		{name: "garbage is refused", raw: str("next tuesday"), wantErr: true},
		{name: "a date alone is not RFC3339", raw: str("2026-09-01"), wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			val, set, err := parseShareExpiry(c.raw, now)
			if c.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want an error (value %v, set %v)", val, set)
				}
				if set {
					t.Error("set = true on a refused value; the write must not run")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if set != c.wantSet {
				t.Errorf("set = %v, want %v", set, c.wantSet)
			}
			if c.wantNil {
				if val != nil {
					t.Errorf("value = %v, want nil", val)
				}
				return
			}
			got, ok := val.(time.Time)
			if !ok {
				t.Fatalf("value = %T, want time.Time", val)
			}
			if !got.Equal(c.wantTime) {
				t.Errorf("value = %s, want %s", got, c.wantTime)
			}
		})
	}
}
