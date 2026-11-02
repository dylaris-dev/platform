package health

import (
	"context"
	"testing"

	"dylaris-pkg/release"
)

// adminCtx builds the request context AuthMiddleware would have installed.
// Shared by the handler tests in this package.
func adminCtx(userID string, isAdmin bool) context.Context {
	ctx := context.WithValue(context.Background(), "userID", userID)
	return context.WithValue(ctx, "isAdmin", isAdmin)
}

func mustParse(t *testing.T, src string) []release.Release {
	t.Helper()
	rs, err := release.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return rs
}

const notesForTest = "## 2026.08.28\n" +
	"### Features\n- A node change. `node`\n" +
	"### Breaking\n- Nothing.\n" +
	"### Security\n- Nothing.\n" +
	"### Fixes\n- Nothing.\n" +
	"\n## 2026.08.20\n" +
	"### Features\n- An old core change. `core`\n" +
	"### Breaking\n- Nothing.\n" +
	"### Security\n- Nothing.\n" +
	"### Fixes\n- Nothing.\n"

// newestNaming is what an instance has to REACH to be current, and it is per
// service. Reading the newest release outright would tell a Core operator to
// update for a release that only ever touched the node.
func TestNewestNaming(t *testing.T) {
	rs := mustParse(t, notesForTest)
	if got := newestNaming(rs, "node"); got != "2026.08.28" {
		t.Errorf("node = %q, want 2026.08.28", got)
	}
	if got := newestNaming(rs, "core"); got != "2026.08.20" {
		t.Errorf("core = %q, want 2026.08.20 - the newer release never names core", got)
	}
	if got := newestNaming(rs, "log-shipper"); got != "" {
		t.Errorf("log-shipper = %q, want empty: no release ever names it", got)
	}
}

func TestVersioned(t *testing.T) {
	rs := mustParse(t, notesForTest)

	t.Run("behind on a release that names it", func(t *testing.T) {
		got := versioned("node-1", "2026.08.20", rs, "node")
		if !got.Outdated || got.Version != "2026.08.20" {
			t.Errorf("got %+v, want outdated", got)
		}
	})

	// The false positive this whole design exists to avoid: core is older than
	// the newest release, and that release does not name core.
	t.Run("older than the newest release but not named by it", func(t *testing.T) {
		got := versioned("core", "2026.08.20", rs, "core")
		if got.Outdated {
			t.Error("core was flagged by a release that never names it")
		}
	})

	// An unstamped image reports nothing. It must show as "not reporting" and
	// never as behind, or the day this ships flags every deployment.
	t.Run("not reporting", func(t *testing.T) {
		got := versioned("node-2", "", rs, "node")
		if got.Version != "" || got.Outdated {
			t.Errorf("got %+v, want an empty version and not outdated", got)
		}
		if got.Label != "node-2" {
			t.Errorf("label = %q", got.Label)
		}
	})

	t.Run("garbage is treated as not reporting", func(t *testing.T) {
		got := versioned("node-3", "banana", rs, "node")
		if got.Version != "" || got.Outdated {
			t.Errorf("got %+v, want an empty version and not outdated", got)
		}
	})
}
