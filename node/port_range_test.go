package main

import "testing"

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantStart int
		wantEnd   int
		wantErr   bool
	}{
		{name: "plain range", raw: "25600-25699", wantStart: 25600, wantEnd: 25699},
		{name: "surrounding spaces", raw: " 25600 - 25699 ", wantStart: 25600, wantEnd: 25699},
		{name: "single port", raw: "25600-25600", wantStart: 25600, wantEnd: 25600},
		{name: "no separator", raw: "25600", wantErr: true},
		{name: "start not numeric", raw: "abc-25699", wantErr: true},
		{name: "end not numeric", raw: "25600-abc", wantErr: true},
		{name: "end below start", raw: "25699-25600", wantErr: true},
		{name: "port zero", raw: "0-25699", wantErr: true},
		{name: "port above 65535", raw: "25600-70000", wantErr: true},
		{name: "negative start parses as separator split", raw: "-1-25699", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := parsePortRange(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePortRange(%q) = %d-%d, want error", tt.raw, start, end)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortRange(%q) returned error: %v", tt.raw, err)
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("parsePortRange(%q) = %d-%d, want %d-%d", tt.raw, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestResolvePortRange(t *testing.T) {
	tests := []struct {
		name       string
		portRange  string
		legacyVars bool
		wantStart  int
		wantEnd    int
		wantNotice bool
	}{
		{
			name:      "valid range is used as-is with no notice",
			portRange: "26000-26099",
			wantStart: 26000,
			wantEnd:   26099,
		},
		{
			// The default is a valid choice; a notice for it showed as a
			// warning on every node in Infrastructure.
			name:      "unset uses the default with no notice",
			wantStart: defaultPortRangeStart,
			wantEnd:   defaultPortRangeEnd,
		},
		{
			name:      "blank value counts as unset",
			portRange: "   ",
			wantStart: defaultPortRangeStart,
			wantEnd:   defaultPortRangeEnd,
		},
		{
			name:       "invalid value falls back instead of half-applying",
			portRange:  "26000-oops",
			wantStart:  defaultPortRangeStart,
			wantEnd:    defaultPortRangeEnd,
			wantNotice: true,
		},
		{
			// An inverted range used to reach the port manager and panic there
			// on a negative allocation-table length.
			name:       "inverted range falls back",
			portRange:  "26099-26000",
			wantStart:  defaultPortRangeStart,
			wantEnd:    defaultPortRangeEnd,
			wantNotice: true,
		},
		{
			// The retired vars must be called out, or an upgrade silently moves
			// the range away from the operator's firewall rules.
			name:       "retired START/END vars are reported",
			legacyVars: true,
			wantStart:  defaultPortRangeStart,
			wantEnd:    defaultPortRangeEnd,
			wantNotice: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PORT_RANGE", tt.portRange)
			t.Setenv("PORT_RANGE_START", "")
			t.Setenv("PORT_RANGE_END", "")
			if tt.legacyVars {
				t.Setenv("PORT_RANGE_START", "26000")
				t.Setenv("PORT_RANGE_END", "26099")
			}

			start, end, notice := resolvePortRange()
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("resolvePortRange() = %d-%d, want %d-%d", start, end, tt.wantStart, tt.wantEnd)
			}
			if tt.wantNotice && notice == "" {
				t.Error("resolvePortRange() notice is empty, want an explanation")
			}
			if !tt.wantNotice && notice != "" {
				t.Errorf("resolvePortRange() notice = %q, want empty", notice)
			}
		})
	}
}

func TestResolvePortRangeLegacyVarsIgnoredWhenPortRangeSet(t *testing.T) {
	t.Setenv("PORT_RANGE", "26000-26099")
	t.Setenv("PORT_RANGE_START", "27000")
	t.Setenv("PORT_RANGE_END", "27099")

	start, end, notice := resolvePortRange()
	if start != 26000 || end != 26099 {
		t.Errorf("resolvePortRange() = %d-%d, want 26000-26099 (PORT_RANGE wins)", start, end)
	}
	if notice != "" {
		t.Errorf("resolvePortRange() notice = %q, want empty", notice)
	}
}
