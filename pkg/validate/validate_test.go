package validate

import (
	"strings"
	"testing"
)

func TestUsername(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"alice", true},
		{"a_b.c-d", true},
		{"abc", true},
		{"A1b2c3", true},
		{"ab", false},   // too short
		{"a:b", false},  // colon - the Redis-key-injection case
		{"a b", false},  // space
		{"_abc", false}, // leading non-alnum
		{".abc", false}, // leading dot
		{"a@b", false},  // special char
		{"", false},     // empty
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false}, // 33 chars
	}
	for _, c := range cases {
		if got := IsUsername(c.in); got != c.want {
			t.Errorf("IsUsername(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestServerName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"My Server", true},
		{"srv_1", true},
		{"a", true},
		{"a-b+c_d 1", true},
		{" leading", false}, // leading space
		{"a/b", false},      // slash
		{"a:b", false},      // colon
		{"", false},
	}
	for _, c := range cases {
		if got := IsServerName(c.in); got != c.want {
			t.Errorf("IsServerName(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	// length: exactly 50 ok, 51 not
	n50 := "a" + string(make([]byte, 0))
	for len(n50) < 50 {
		n50 += "a"
	}
	if !IsServerName(n50) {
		t.Errorf("IsServerName(50 chars) = false, want true")
	}
	if IsServerName(n50 + "a") {
		t.Errorf("IsServerName(51 chars) = true, want false")
	}
}

func TestSubServerName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"survival", true},
		{"world_1", true},
		{"a-b+c", true},
		{"a b", false},      // space not allowed for a dir name
		{"a/b", false},      // slash
		{".dylaris", false}, // dot not allowed
		{"", false},
	}
	for _, c := range cases {
		if got := IsSubServerName(c.in); got != c.want {
			t.Errorf("IsSubServerName(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestServerUUID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"84087ad8-e332-44f2-adec-75869b8c1ee1", true},              // canonical UUID
		{"84087AD8-E332-44F2-ADEC-75869B8C1EE1", true},              // upper hex
		{"d10612d3-5a6c-4dce-99a3-18644d192bb4_k3j2h1x9p0qr", true}, // panel <ownerUUID>_<random>
		{"84087ad8e33244f2adec75869b8c1ee1", true},                  // no-dash id, still safe
		{"a:b:c:d:e:f:gg", false},                                   // colon = injection
		{"survival/../etc", false},                                  // slash/traversal
		{"has space here", false},                                   // whitespace
		{"short", false},                                            // < 8 chars
		{"", false},                                                 // empty
	}
	for _, c := range cases {
		if got := IsServerUUID(c.in); got != c.want {
			t.Errorf("IsServerUUID(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSlugLabelEmailMcVersion(t *testing.T) {
	if !IsSlug("my-pack_1") || IsSlug("My Pack") || IsSlug("-x") {
		t.Error("Slug cases failed")
	}
	if !IsLabel("eu-central") || IsLabel("EU") || IsLabel("a b") {
		t.Error("Label cases failed")
	}
	if !IsEmail("a@b.co") || IsEmail("no-at") || IsEmail("a @b.co") {
		t.Error("Email cases failed")
	}
	if !IsMcVersion("1.21") || !IsMcVersion("1.21.4") || IsMcVersion("1") || IsMcVersion("1.x") {
		t.Error("McVersion cases failed")
	}
	if !IsMinecraftUsername("Notch_99") || IsMinecraftUsername("ab") || IsMinecraftUsername("has space") || IsMinecraftUsername("has:colon") {
		t.Error("MinecraftUsername cases failed")
	}
}

func TestSanitizeServerName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"My Server!", "My Server"},
		{"  ---abc", "abc"},
		{"a/b*c", "abc"},
		{"", ""},
		{"!!!", ""},
	}
	for _, c := range cases {
		if got := SanitizeServerName(c.in); got != c.want {
			t.Errorf("SanitizeServerName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// over-length is trimmed to 50
	long := ""
	for i := 0; i < 60; i++ {
		long += "a"
	}
	if got := SanitizeServerName(long); len(got) != 50 {
		t.Errorf("SanitizeServerName(60 chars) len = %d, want 50", len(got))
	}
}

func TestIsSafeRelPath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"survival/world.zip", true},
		{"", true}, // server root
		{"a/b/c.txt", true},
		{"/etc/passwd", false}, // absolute
		{"../secret", false},   // traversal
		{"a/../../b", false},   // traversal mid-path
		{`C:\Windows`, false},  // drive
		{`a\..\b`, false},      // backslash traversal
	}
	for _, c := range cases {
		if got := IsSafeRelPath(c.in); got != c.want {
			t.Errorf("IsSafeRelPath(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsInstallerType(t *testing.T) {
	for _, ok := range []string{"paper", "vanilla", "upload-zip", "modpack", "velocity", "waterfall", "bungeecord"} {
		if !IsInstallerType(ok) {
			t.Errorf("IsInstallerType(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "rm -rf", "PAPER", "ftp"} {
		if IsInstallerType(bad) {
			t.Errorf("IsInstallerType(%q) = true, want false", bad)
		}
	}
}

func TestIsModrinthLoader(t *testing.T) {
	for _, ok := range []string{"paper", "spigot", "bukkit", "purpur", "fabric", "forge", "quilt", "neoforge", "velocity", "waterfall", "bungeecord"} {
		if !IsModrinthLoader(ok) {
			t.Errorf("IsModrinthLoader(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "PAPER", "vanilla", "upload", "modpack", "rm -rf"} {
		if IsModrinthLoader(bad) {
			t.Errorf("IsModrinthLoader(%q) = true, want false", bad)
		}
	}
}

func TestResourceBounds(t *testing.T) {
	if r := ResourceBounds(1024, 2, 5120, 8); r != "" {
		t.Errorf("valid resources rejected: %q", r)
	}
	if ResourceBounds(128, 2, 5120, 8) == "" {
		t.Error("RAM 128 should be rejected")
	}
	// CPU 0 = no limit, the same convention diskMB uses and the value the panel
	// ships as its default. Rejecting it made the default create fail.
	if r := ResourceBounds(1024, 0, 5120, 8); r != "" {
		t.Errorf("CPU 0 (no limit) rejected: %q", r)
	}
	if ResourceBounds(1024, -1, 5120, 8) == "" {
		t.Error("negative CPU should be rejected")
	}
	if ResourceBounds(1024, 16, 5120, 8) == "" {
		t.Error("CPU over host cap should be rejected")
	}
	// An uncapped server must still not be read as "0 cores, so under the cap"
	// on a host whose count is known.
	if r := ResourceBounds(1024, 0, 5120, 1); r != "" {
		t.Errorf("CPU 0 rejected against a known host cap: %q", r)
	}
	if ResourceBounds(1024, 2, -1, 8) == "" {
		t.Error("negative disk should be rejected")
	}
	// hostCPU 0 -> CPU ceiling skipped
	if ResourceBounds(1024, 999, 5120, 0) != "" {
		t.Error("hostCPU 0 should skip the CPU ceiling")
	}
}

func TestLocationName(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"home-desktop", true},
		{"pc01", true},
		{"a1b2", true},
		{"twenty-chars-exactly", true}, // 20
		{"pc1", false},                 // 3, under the floor
		{"", false},
		{"twenty-one-chars-here", false}, // 21
		{"-leading", false},              // becomes a flag once slugged into NODE_ID
		{"trailing-", false},
		{"-", false},
		{"----", false},
		{"has space", false},
		{"under_score", false},
		{"dot.name", false},
		{"slash/name", false},
		{"colon:name", false}, // would split a Redis key
		{"emoji\U0001F600x", false},
	}
	for _, c := range cases {
		if got := IsLocationName(c.in); got != c.want {
			t.Errorf("IsLocationName(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if len("twenty-chars-exactly") != 20 {
		t.Fatal("the boundary case is not actually 20 characters")
	}
}

func TestServerHostname(t *testing.T) {
	cases := []struct{ uuid, want string }{
		{"befa5934-e5f8-4bb0-b35d-df8da14fc003_d59de1665a", "mc-befa5934-e5f8-4bb0-b35d-df8da14fc003-d59de1665a"},
		{"Owner_ABC12345", "mc-owner-abc12345"},
		{"abcdefgh_", ""},             // a label may not end in '-'
		{strings.Repeat("a", 61), ""}, // 64 bytes with the prefix
		{strings.Repeat("a", 60), "mc-" + strings.Repeat("a", 60)},
		{"../etc/passwd", ""},
	}
	for _, c := range cases {
		if got := ServerHostname(c.uuid); got != c.want {
			t.Errorf("ServerHostname(%q) = %q, want %q", c.uuid, got, c.want)
		}
	}
}
