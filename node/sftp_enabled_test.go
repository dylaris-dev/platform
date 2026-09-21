package main

import "testing"

// The mirror of beamAdvertiseEnabled, and it was missing: the SFTP listener
// started unconditionally and authenticated against the hashes Core publishes,
// so file_access_mode="beam" switched SFTP off in the PANEL - which refuses to
// hand out credentials and answers "beam_only" - while the transport went on
// accepting logins.
//
// Measured on production: with the platform on beam-only, a delegate logged in
// over SFTP with their panel password and listed their server's files.
func TestSFTPEnabled(t *testing.T) {
	origExternal := nodeExternal
	origRouting, origFile, origPort, origCPort, origIO, origPids := getModes()
	t.Cleanup(func() {
		nodeExternal = origExternal
		setModes(origRouting, origFile, origPort, origCPort, origIO, origPids)
	})

	cases := []struct {
		name     string
		external bool
		fileMode string
		want     bool
	}{
		{"sftp mode serves it", false, "sftp", true},
		{"both serves it", false, "both", true},
		{"beam does NOT", false, "beam", false},
		// The node's own default before the first Redis read is "sftp", and a
		// platform that never configured the mode must behave as it always has.
		{"an unknown value is not beam", false, "", true},
		// An external node forces beam locally whatever the platform says, the
		// same rule the panel's credentials route applies.
		{"an external node never serves it", true, "sftp", false},
		{"an external node in beam mode either", true, "beam", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodeExternal = c.external
			setModes(origRouting, c.fileMode, origPort, origCPort, origIO, origPids)
			if got := sftpEnabled(); got != c.want {
				t.Errorf("sftpEnabled(external=%v, mode=%q) = %v, want %v", c.external, c.fileMode, got, c.want)
			}
		})
	}
}

// The two are opposites on the file mode and agree on an external node: beam is
// what an external node has instead of SFTP, so both must not be off at once.
func TestSFTPAndBeamNeverBothOff(t *testing.T) {
	origExternal := nodeExternal
	origRouting, origFile, origPort, origCPort, origIO, origPids := getModes()
	t.Cleanup(func() {
		nodeExternal = origExternal
		setModes(origRouting, origFile, origPort, origCPort, origIO, origPids)
	})

	for _, external := range []bool{false, true} {
		for _, mode := range []string{"sftp", "both", "beam", ""} {
			nodeExternal = external
			setModes(origRouting, mode, origPort, origCPort, origIO, origPids)
			if !sftpEnabled() && !beamAdvertiseEnabled() {
				t.Errorf("external=%v mode=%q leaves the node with no file transport at all", external, mode)
			}
		}
	}
}
