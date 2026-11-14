package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The Core binary must not link test code of its own: a fake provider or a
// testing.Testing() switch in a production package is reachable at runtime.
// Third-party packages that import testing (hcloud, libdns/namecheap) are not
// ours to change, so only first-party importers are checked.
func TestProductionBinaryDoesNotLinkTesting(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go is not on PATH: cannot list the binary's dependencies")
	}
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{join .Imports \" \"}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "dylaris-") {
			continue
		}
		for _, imp := range fields[1:] {
			if imp == "testing" || imp == "net/http/httptest" {
				t.Errorf("%s imports %s into the production binary", fields[0], imp)
			}
		}
	}
}
