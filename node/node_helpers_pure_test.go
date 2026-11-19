package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLanCandidates(t *testing.T) {
	t.Run("endpoint with port reuses that port", func(t *testing.T) {
		got := lanCandidates("10.0.0.1:9999", []string{"10.0.0.2", "10.0.0.3"})
		want := []string{"10.0.0.2:9999", "10.0.0.3:9999"}
		if !stringSlicesEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("endpoint without a parseable port falls back to default", func(t *testing.T) {
		got := lanCandidates("no-port-here", []string{"10.0.0.2"})
		want := []string{"10.0.0.2:25522"}
		if !stringSlicesEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("duplicate private IPs collapse", func(t *testing.T) {
		got := lanCandidates("10.0.0.1:9999", []string{"10.0.0.2", "10.0.0.2", "10.0.0.3"})
		want := []string{"10.0.0.2:9999", "10.0.0.3:9999"}
		if !stringSlicesEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("empty private IPs yields empty result", func(t *testing.T) {
		got := lanCandidates("10.0.0.1:9999", nil)
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})

	t.Run("empty endpoint and nil IPs yields empty result", func(t *testing.T) {
		got := lanCandidates("", nil)
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})

	// The list is the SOURCE node's self-report: it goes into that node's own
	// discovery heartbeat, Core stores it verbatim and hands it to this node,
	// and chooseMigrationHost then sends the pull token to every entry. Anything
	// that is not an address the source could honestly own must not be probed.
	t.Run("only private IPv4 addresses are probed", func(t *testing.T) {
		got := lanCandidates("10.0.0.1:9999", []string{
			"127.0.0.1",       // this node's own loopback
			"169.254.169.254", // cloud metadata
			"8.8.8.8",         // arbitrary public host
			"::1",             // IPv6 loopback
			"evil.example.com",
			"10.0.0.2:1", // an address with a port glued on
			"",
			"192.168.1.5", // the one honest entry
		})
		want := []string{"192.168.1.5:9999"}
		if !stringSlicesEqual(got, want) {
			t.Fatalf("got %v want %v", got, want)
		}
	})

	t.Run("the candidate list is capped", func(t *testing.T) {
		var many []string
		for i := range 60 {
			many = append(many, fmt.Sprintf("10.1.%d.%d", i/256, i%256))
		}
		got := lanCandidates("10.0.0.1:9999", many)
		if len(got) != maxLANCandidates {
			t.Fatalf("got %d candidates, want the cap of %d - each one costs a %s probe before the R2 fallback",
				len(got), maxLANCandidates, migrationProbeTimeout)
		}
	})
}

func TestStagedArchivePath(t *testing.T) {
	storagePath := t.TempDir()
	got := stagedArchivePath(storagePath, "abc-123")
	want := filepath.Join(storagePath, migrationStagingDir, "abc-123.zip")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestMatchAny(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		patterns []string
		want     bool
	}{
		{"suffix glob matches basename anywhere", "world/logs/latest.log", []string{"*.log"}, true},
		{"no match", "world/region/r.0.0.mca", []string{"*.log"}, false},
		{"exact basename match", "world.dat", []string{"world.dat"}, true},
		{"multiple patterns, second matches", "cache/x.tmp", []string{"*.log", "cache/**"}, true},
		{"empty patterns list", "anything", nil, false},
		{"all-empty-string patterns are skipped", "anything", []string{"", ""}, false},
		// Real (non-obvious) behavior: a "/**" pattern is anchored at the
		// ROOT of path, not "anywhere in the tree". "logs/**" only matches
		// paths that themselves start with "logs/" from the top; it does
		// NOT match a nested "world/logs/..." path. Flagging this as a
		// documented quirk, not a bug: callers must write the full
		// root-relative prefix (e.g. "world/logs/**") to match nested dirs.
		{"double-star pattern does NOT match a nested dir unless anchored at root", "world/logs/latest.log", []string{"logs/**"}, false},
		{"double-star pattern matches when anchored at the correct root prefix", "world/logs/latest.log", []string{"world/logs/**"}, true},
		{"double-star pattern matches the bare directory itself", "world/logs", []string{"world/logs/**"}, true},
		// A pattern that names a directory covers what is under it, so an
		// include of "world" acts like "world/**" instead of archiving only
		// the empty directory entry.
		{"pattern naming a directory matches its children", "world/region/r.0.0.mca", []string{"world"}, true},
		{"pattern naming a nested directory matches its children", "plugins/Essentials/config.yml", []string{"plugins/Essentials"}, true},
		{"glob naming a directory matches its children", "world_nether/DIM-1/r.0.0.mca", []string{"world_*"}, true},
		{"sibling with a shared prefix is not a child", "worldedit/config.yml", []string{"world"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchAny(c.path, c.patterns); got != c.want {
				t.Fatalf("matchAny(%q, %v) = %v, want %v", c.path, c.patterns, got, c.want)
			}
		})
	}
}

func TestNodeLocalArchiveName(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"job-1/server-uuid/archive.tar.gz", "archive.tar.gz"},
		{"archive.tar.gz", "archive.tar.gz"},
		{"a/b/c", "c"},
		{"", ""},
	}
	for _, c := range cases {
		if got := nodeLocalArchiveName(c.key); got != c.want {
			t.Errorf("nodeLocalArchiveName(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

func TestDepthBelow(t *testing.T) {
	root := filepath.Join(t.TempDir(), "servers", "uuid1")

	t.Run("root equals path is depth 0", func(t *testing.T) {
		if got := depthBelow(root, root); got != 0 {
			t.Fatalf("got %d want 0", got)
		}
	})

	t.Run("one level below root is depth 1", func(t *testing.T) {
		p := filepath.Join(root, "world")
		if got := depthBelow(root, p); got != 1 {
			t.Fatalf("got %d want 1", got)
		}
	})

	t.Run("nested path counts every path component", func(t *testing.T) {
		p := filepath.Join(root, "world", "region", "r.0.0.mca")
		if got := depthBelow(root, p); got != 3 {
			t.Fatalf("got %d want 3", got)
		}
	})

	// Real (non-obvious) behavior: depthBelow does NOT verify containment.
	// filepath.Rel happily returns a "../.." relative path for a sibling or
	// ancestor, and depthBelow just counts its slash-separated components,
	// so a path OUTSIDE root still returns a small positive number instead
	// of, say, -1 or an error. This is safe in practice only because the
	// walker (restore_cleanup.go) always calls it with paths yielded by
	// WalkDir starting AT root, so an out-of-root path never actually
	// occurs at the real call site. Flagging as a smell, not fixing.
	t.Run("path outside root is not detected, just counted (documented real behavior)", func(t *testing.T) {
		sibling := filepath.Join(filepath.Dir(root), "uuid2", "world")
		got := depthBelow(root, sibling)
		if got <= 0 {
			t.Fatalf("got %d, expected a positive hop count even though sibling is not under root", got)
		}
	})
}

func TestFirstPersistedStoragePath(t *testing.T) {
	t.Run("single path returns its absolute form", func(t *testing.T) {
		dir := t.TempDir()
		want, err := filepath.Abs(dir)
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		if got := firstPersistedStoragePath(dir); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("multiple paths: first non-empty wins", func(t *testing.T) {
		dir1 := t.TempDir()
		dir2 := t.TempDir()
		csv := " ," + dir1 + "," + dir2
		want, err := filepath.Abs(dir1)
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		if got := firstPersistedStoragePath(csv); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("empty CSV falls back to default", func(t *testing.T) {
		baseDir, err := os.Getwd()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		want := filepath.Join(baseDir, "dylaris_data", "servers")
		if got := firstPersistedStoragePath(""); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("whitespace-only CSV falls back to default", func(t *testing.T) {
		baseDir, err := os.Getwd()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
		want := filepath.Join(baseDir, "dylaris_data", "servers")
		if got := firstPersistedStoragePath("   "); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})
}

func TestStripJava8IncompatibleFlags(t *testing.T) {
	t.Run("strips known java-9+ only flags", func(t *testing.T) {
		in := "-XX:+UseG1GC -XX:-ShrinkHeapInSteps -Xlog:gc::utctime,level,tags -Dfoo=bar"
		want := "-XX:+UseG1GC -Dfoo=bar"
		if got := stripJava8IncompatibleFlags(in); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("leaves java-8-safe flags untouched (whitespace normalized)", func(t *testing.T) {
		in := "-XX:+UseG1GC   -Dfoo=bar"
		want := "-XX:+UseG1GC -Dfoo=bar"
		if got := stripJava8IncompatibleFlags(in); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("single bad flag strips to empty", func(t *testing.T) {
		if got := stripJava8IncompatibleFlags("-XX:+ShrinkHeapInSteps"); got != "" {
			t.Fatalf("got %q want empty", got)
		}
	})

	t.Run("empty input returns empty", func(t *testing.T) {
		if got := stripJava8IncompatibleFlags(""); got != "" {
			t.Fatalf("got %q want empty", got)
		}
	})
}

func TestExtractJvmFlagsFromCommand(t *testing.T) {
	t.Run("buildStartCommand-style jar command", func(t *testing.T) {
		cmd := "java -Xlog:gc::utctime,level,tags -XX:+UseG1GC -Xms4096M -Xmx4096M -jar paper-1.20.1.jar nogui"
		// This case used to assert the opposite - that gcLogFlag comes back out
		// as if it were a user-set flag - and said so: "Any caller that
		// round-trips extract -> buildStartCommand would end up with -Xlog
		// re-added ... Flagging as a smell, not fixing."
		//
		// Both callers do exactly that round trip (RestartContainer and
		// UpdateResources in docker_mgr.go), so it was not a smell, it was the
		// bug. A live test server's container Cmd after six starts:
		//
		//	java -Xlog:gc::utctime,level,tags (x6) -Dterminal.ansi=true … -jar server.jar nogui
		//
		// one more copy per restart, forever. gcLogFlag is now dropped on
		// extraction, so the round trip is stable.
		want := "-XX:+UseG1GC"
		if got := extractJvmFlagsFromCommand(cmd); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	// TestExtractJvmFlags_RoundTripIsStable is the property the bug broke: the
	// command buildStartCommand produces must survive extract -> build
	// unchanged, because that is what every container recreate does.
	t.Run("round trip through buildStartCommand is stable", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "server.jar"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		first, err := buildStartCommand(dir, 2048, "-XX:+UseG1GC", "ghcr.io/x/mc-java21:latest")
		if err != nil {
			t.Fatalf("buildStartCommand: %v", err)
		}
		cmd := first
		for i := range 5 {
			next, err := buildStartCommand(dir, 2048, extractJvmFlagsFromCommand(cmd), "ghcr.io/x/mc-java21:latest")
			if err != nil {
				t.Fatalf("rebuild %d: %v", i, err)
			}
			if next != first {
				t.Fatalf("rebuild %d drifted:\n first: %s\n  now:  %s", i+1, first, next)
			}
			cmd = next
		}
		if n := strings.Count(cmd, gcLogFlag); n != 1 {
			t.Errorf("gcLogFlag appears %d times after 5 rebuilds, want 1", n)
		}
	})

	t.Run("legacy Core format (Xms/Xmx before flags)", func(t *testing.T) {
		cmd := "java -Xms2048M -Xmx2048M -XX:+UseG1GC -Dfoo=bar -jar server.jar nogui"
		want := "-XX:+UseG1GC -Dfoo=bar"
		if got := extractJvmFlagsFromCommand(cmd); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
	})

	t.Run("argfile form with only structural tokens yields empty", func(t *testing.T) {
		cmd := "java @user_jvm_args.txt -Xms4096M -Xmx4096M " +
			"@libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt nogui"
		if got := extractJvmFlagsFromCommand(cmd); got != "" {
			t.Fatalf("got %q want empty", got)
		}
	})

	t.Run("empty command returns empty", func(t *testing.T) {
		if got := extractJvmFlagsFromCommand(""); got != "" {
			t.Fatalf("got %q want empty", got)
		}
	})
}
