package main

import (
	"strings"
	"testing"
)

func TestTzEnvOnlyNamesAZoneTheContainerCanUse(t *testing.T) {
	cases := map[string]string{
		"Europe/Berlin":     "TZ=Europe/Berlin",
		"UTC":               "TZ=UTC",
		"":                  "",
		"Local":             "",
		"Mars/Olympus":      "",
		"../../etc/passwd":  "",
		"UTC\nREDIS_PASS=x": "",
		"A=B":               "",
	}
	for in, want := range cases {
		if got := strings.Join(tzEnv(in), ","); got != want {
			t.Errorf("tzEnv(%q) = %q, want %q", in, got, want)
		}
	}
}
