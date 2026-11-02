package settings

import "testing"

// The rule this decides: what a save writes into beam.relay_address, which is
// the manual OVERRIDE and not the effective relay.
//
// The read returns the resolved relay - discovered from the registry when no
// override is set - in the same struct's RelayAddress. Echoing that back on save
// pinned whichever relay happened to be discovered as a permanent override, and
// the operator saw nothing: the field kept showing the address it had always
// shown, while discovery, failover and multi-region selection stopped for good.
// Nothing in the panel or the logs said so.
func TestBeamManualOverride(t *testing.T) {
	s := func(v string) *string { return &v }

	cases := []struct {
		name string
		req  BeamSettings
		want string
	}{
		{
			// The defect, stated as a test: a client that knows the field sends
			// an empty override alongside the discovered relay it was shown, and
			// the discovered relay must NOT be stored.
			name: "an explicit empty override beats the effective relay",
			req:  BeamSettings{RelayAddress: "relay-fra-1.example.com:25550", ManualOverride: s("")},
			want: "",
		},
		{
			name: "an operator-set override is stored",
			req:  BeamSettings{RelayAddress: "beam.example.com:25550", ManualOverride: s("beam.example.com:25550")},
			want: "beam.example.com:25550",
		},
		{
			name: "the override wins over a differing effective relay",
			req:  BeamSettings{RelayAddress: "relay-fra-1.example.com:25550", ManualOverride: s("beam.example.com:25550")},
			want: "beam.example.com:25550",
		},
		{
			// An older panel does not send the field at all. Treating nil as ""
			// would clear an override it never knew about, which is the same
			// class of silent change in the other direction.
			name: "an absent field keeps the legacy behaviour",
			req:  BeamSettings{RelayAddress: "beam.example.com:25550"},
			want: "beam.example.com:25550",
		},
		{
			name: "whitespace is not an address",
			req:  BeamSettings{ManualOverride: s("   ")},
			want: "",
		},
		{
			name: "surrounding whitespace is trimmed",
			req:  BeamSettings{ManualOverride: s("  beam.example.com:25550  ")},
			want: "beam.example.com:25550",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := beamManualOverride(c.req); got != c.want {
				t.Errorf("beamManualOverride = %q, want %q", got, c.want)
			}
		})
	}
}
