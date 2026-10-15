package handlers

import (
	"encoding/json"
	"testing"
)

func TestParseAdminCeiling(t *testing.T) {
	for raw, want := range map[string]string{
		``:            "",
		`null`:        "",
		`"unlimited"`: "unlimited",
		`0`:           "0",
		`4`:           "4",
	} {
		got, err := parseAdminCeiling(json.RawMessage(raw))
		if err != nil || got != want {
			t.Errorf("%q -> %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{`-1`, `"lots"`, `1.5`, `true`} {
		if _, err := parseAdminCeiling(json.RawMessage(raw)); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
}
