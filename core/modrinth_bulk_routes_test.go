package main

import (
	"os"
	"strings"
	"testing"
)

// Each bulk Modrinth call stands for up to a thousand mods, so these must stay
// authed and on their own per-IP bucket rather than the browse bucket, which
// Update all used to exhaust (and be exhausted by).
func TestModrinthBulkRoutesAreAuthedAndLimited(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/modrinth/versions", "/modrinth/projects", "/modrinth/version-files/update"} {
		found := false
		for _, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, `HandleFunc("`+path+`"`) {
				continue
			}
			found = true
			if !strings.Contains(line, "modrinthBulkLimiter.Limit(") || !strings.Contains(line, "authHandler.AuthMiddleware(") {
				t.Errorf("%s must be wrapped in modrinthBulkLimiter.Limit and AuthMiddleware: %s", path, strings.TrimSpace(line))
			}
		}
		if !found {
			t.Errorf("no %s registration found; the matcher is broken, not the code", path)
		}
	}
}
