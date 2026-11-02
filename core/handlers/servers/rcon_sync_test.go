package servers

import "testing"

// rconNeedsStamping decides whether activating a sub-server writes the stored
// RCON config into its server.properties. Both directions matter: skipping when
// it is needed is the bug this exists for (Core reports RCON enabled, the file
// says otherwise, the Players tabs unlock onto a closed port); stamping when it
// is NOT needed writes three keys into the properties of every server on the
// platform at every single start.
func TestRconNeedsStamping(t *testing.T) {
	cases := []struct {
		name      string
		subServer string
		enabled   bool
		password  string
		want      bool
	}{
		{
			name:      "never configured - leave the file alone",
			subServer: "survival", enabled: false, password: "", want: false,
		},
		{
			name:      "enabled - the whole point",
			subServer: "survival", enabled: true, password: "secret", want: true,
		},
		{
			// Turning RCON off has to reach the sub-server too, or switching to
			// one that still carries an old enable-rcon=true leaves a port open
			// that the panel believes is closed.
			name:      "disabled but configured before - the off must travel too",
			subServer: "creative", enabled: false, password: "secret", want: true,
		},
		{
			// A password with no enable is what SetConfig leaves behind when it
			// mints one and the write is later disabled; the file still needs it.
			name:      "password without enabled still counts as configured",
			subServer: "creative", enabled: false, password: "leftover", want: true,
		},
		{
			name:      "no sub-server - there is no file to write",
			subServer: "", enabled: true, password: "secret", want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rconNeedsStamping(c.subServer, c.enabled, c.password); got != c.want {
				t.Errorf("rconNeedsStamping(%q, enabled=%v, hasPassword=%v) = %v, want %v",
					c.subServer, c.enabled, c.password != "", got, c.want)
			}
		})
	}
}
