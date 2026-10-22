package main

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// velocityDefaultSample has the shape of Velocity 3's default-velocity.toml
// around the key: a top-level table, comments, and [advanced] between others.
const velocityDefaultSample = `# Config version. Do not change this
config-version = "2.8"

bind = "0.0.0.0:25565"

[servers]
lobby = "127.0.0.1:30066"

[advanced]
# Enables compatibility with HAProxy's PROXY protocol. If you don't know what this is for, then
# don't enable it.
haproxy-protocol = false

tcp-fast-open = false

[query]
enabled = false
`

func writeJar(t *testing.T, dir string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, "server.jar"))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func newSubServer(t *testing.T) (serverPath, sub, subDir string) {
	t.Helper()
	serverPath = t.TempDir()
	sub = "main"
	subDir = filepath.Join(serverPath, sub)
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProxyConfig_Paper(t *testing.T) {
	serverPath, sub, subDir := newSubServer(t)
	if !writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "paper"}) {
		t.Fatal("not written")
	}
	if got := readFile(t, filepath.Join(subDir, "config", "paper-global.yml")); got != "proxies:\n  proxy-protocol: true\n" {
		t.Fatalf("paper-global.yml = %q", got)
	}
	// A rerun finds its own file: still a yes, or Core would switch off a
	// server whose config expects the header.
	if !writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "paper"}) {
		t.Fatal("a rerun did not recognise the node's own file")
	}
}

func TestPaperExpectsProxy(t *testing.T) {
	cases := map[string]bool{
		paperProxyProtocolYAML: true,
		// Paper's full file saved from ours.
		"_version: 31\nchunk-loading-basic:\n  player-max-chunk-generate-rate: -1.0\nproxies:\n  bungee-cord:\n    online-mode: true\n  proxy-protocol: true\n  velocity:\n    enabled: false\nspam-limiter:\n  tab-spam-limit: 500\n": true,
		"proxies:\r\n  proxy-protocol: true\r\n":                                                           true,
		"proxies:\n  proxy-protocol: false\n":                                                              false,
		"proxies:\n  velocity:\n    proxy-protocol: true\n":                                                false,
		"other:\n  proxy-protocol: true\n":                                                                 false,
		"proxies:\n  bungee-cord:\n    online-mode: true\nunsupported-settings:\n  proxy-protocol: true\n": false,
		"# proxies:\n#   proxy-protocol: true\n":                                                           false,
		"":                                                                                                 false,
	}
	for in, want := range cases {
		if got := paperExpectsProxy([]byte(in)); got != want {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
}

func TestProxyConfig_PaperLeavesTheOperatorsFile(t *testing.T) {
	for mine, want := range map[string]bool{
		"_version: 31\nproxies:\n  proxy-protocol: false\n": false,
		"_version: 31\nproxies:\n  proxy-protocol: true\n":  true,
	} {
		serverPath, sub, subDir := newSubServer(t)
		os.MkdirAll(filepath.Join(subDir, "config"), 0o755)
		os.WriteFile(filepath.Join(subDir, "config", "paper-global.yml"), []byte(mine), 0o644)
		if got := writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "paper"}); got != want {
			t.Fatalf("%q: answered %v, want %v", mine, got, want)
		}
		if got := readFile(t, filepath.Join(subDir, "config", "paper-global.yml")); got != mine {
			t.Fatalf("overwritten: %q", got)
		}
	}
}

func TestProxyConfig_Velocity(t *testing.T) {
	serverPath, sub, subDir := newSubServer(t)
	writeJar(t, subDir, map[string]string{"default-velocity.toml": velocityDefaultSample, "other.txt": "x"})
	if !writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "velocity"}) {
		t.Fatal("not written")
	}
	want := strings.Replace(velocityDefaultSample, "haproxy-protocol = false", "haproxy-protocol = true", 1)
	if got := readFile(t, filepath.Join(subDir, "velocity.toml")); got != want {
		t.Fatalf("velocity.toml =\n%s\nwant\n%s", got, want)
	}
	if s := readFile(t, filepath.Join(subDir, "forwarding.secret")); len(s) < 12 {
		t.Fatalf("forwarding.secret = %q", s)
	}
	if !writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "velocity"}) {
		t.Fatal("a rerun did not recognise the node's own velocity.toml")
	}
}

func TestVelocityExistingConfig(t *testing.T) {
	serverPath, sub, subDir := newSubServer(t)
	// No jar at all: an existing velocity.toml is answered from itself.
	os.WriteFile(filepath.Join(subDir, "velocity.toml"), []byte("[advanced]\nhaproxy-protocol = true\n"), 0o644)
	if !writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "velocity"}) {
		t.Fatal("an existing velocity.toml expecting the header was not recognised")
	}
	os.WriteFile(filepath.Join(subDir, "velocity.toml"), []byte("[advanced]\nhaproxy-protocol = false\n"), 0o644)
	if writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "velocity"}) {
		t.Fatal("an existing velocity.toml with the header off was vouched for")
	}
}

func TestProxyConfig_VelocityKeepsWhatIsThere(t *testing.T) {
	serverPath, sub, subDir := newSubServer(t)
	writeJar(t, subDir, map[string]string{"default-velocity.toml": velocityDefaultSample})
	os.WriteFile(filepath.Join(subDir, "forwarding.secret"), []byte("mine"), 0o600)
	os.WriteFile(filepath.Join(subDir, "velocity.toml"), []byte("bind = \"0.0.0.0:25565\"\n"), 0o644)
	if writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "velocity"}) {
		t.Fatal("an existing velocity.toml without the key was vouched for")
	}
	if readFile(t, filepath.Join(subDir, "forwarding.secret")) != "mine" || readFile(t, filepath.Join(subDir, "velocity.toml")) != "bind = \"0.0.0.0:25565\"\n" {
		t.Fatal("an existing file was changed")
	}
}

func TestProxyConfig_VelocityJarWithoutTheKey(t *testing.T) {
	serverPath, sub, subDir := newSubServer(t)
	writeJar(t, subDir, map[string]string{"default-velocity.toml": "bind = \"0.0.0.0:25577\"\n"})
	if writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "velocity"}) {
		t.Fatal("reported written")
	}
	if _, err := os.Stat(filepath.Join(subDir, "velocity.toml")); !os.IsNotExist(err) {
		t.Fatal("velocity.toml written for a jar without the key")
	}
}

func TestVelocityConfigWithProxyProtocol(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string // "" = refused
	}{
		"default":         {velocityDefaultSample, strings.Replace(velocityDefaultSample, "haproxy-protocol = false", "haproxy-protocol = true", 1)},
		"crlf kept":       {"[advanced]\r\nhaproxy-protocol = false\r\n", "[advanced]\r\nhaproxy-protocol = true\r\n"},
		"no key":          {"[advanced]\nread-timeout = 30000\n", ""},
		"wrong section":   {"[query]\nhaproxy-protocol = false\n", ""},
		"top level":       {"haproxy-protocol = false\n[advanced]\n", ""},
		"twice":           {"[advanced]\nhaproxy-protocol = false\nhaproxy-protocol = false\n", ""},
		"already true":    {"[advanced]\nhaproxy-protocol = true\n", ""},
		"commented out":   {"[advanced]\n# haproxy-protocol = false\n", ""},
		"other key, same": {"[advanced]\nhaproxy-protocol-x = false\n", ""},
	}
	for name, c := range cases {
		got, ok := velocityConfigWithProxyProtocol([]byte(c.in))
		if c.want == "" {
			if ok {
				t.Errorf("%s: accepted, got %q", name, got)
			}
			continue
		}
		if !ok || string(got) != c.want {
			t.Errorf("%s: got %q ok=%v, want %q", name, got, ok, c.want)
		}
	}
}

func TestProxyConfig_Bungee(t *testing.T) {
	for _, typ := range []string{"bungeecord", "waterfall"} {
		serverPath, sub, subDir := newSubServer(t)
		if err := writeBungeeListener(subDir, 25565); err != nil {
			t.Fatal(err)
		}
		if !writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: typ}) {
			t.Fatalf("%s: the node's own listener was not configured", typ)
		}
		if got := readFile(t, filepath.Join(subDir, "config.yml")); got != "listeners:\n- host: 0.0.0.0:25565\n  proxy_protocol: true\n" {
			t.Fatalf("config.yml = %q", got)
		}
		// A rerun leaves the file as it is and still answers yes.
		if !writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: typ}) {
			t.Fatalf("%s: a rerun did not recognise the node's own listener", typ)
		}
		if got := readFile(t, filepath.Join(subDir, "config.yml")); got != "listeners:\n- host: 0.0.0.0:25565\n  proxy_protocol: true\n" {
			t.Fatalf("%s: a rerun changed config.yml to %q", typ, got)
		}
	}
}

func TestProxyConfig_BungeeNotVouchedFor(t *testing.T) {
	cases := map[string]string{
		"operator's file":      "listeners:\n- host: 0.0.0.0:25577\n  motd: mine\n",
		"second listener":      "listeners:\n- host: 0.0.0.0:25565\n- host: 0.0.0.0:25566\n",
		"listener with a line": "listeners:\n- host: 0.0.0.0:25565\n  proxy_protocol: false\n",
		"no trailing newline":  "listeners:\n- host: 0.0.0.0:25565",
		"one of two listeners": "listeners:\n- query_port: 25577\n  host: 0.0.0.0:25565\n  proxy_protocol: true\n- host: 0.0.0.0:25566\n",
	}
	for name, body := range cases {
		serverPath, sub, subDir := newSubServer(t)
		os.WriteFile(filepath.Join(subDir, "config.yml"), []byte(body), 0o644)
		if writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "bungeecord"}) {
			t.Errorf("%s: vouched for", name)
		}
		if got := readFile(t, filepath.Join(subDir, "config.yml")); got != body {
			t.Errorf("%s: changed to %q", name, got)
		}
	}
	serverPath, sub, _ := newSubServer(t)
	if writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: "bungeecord"}) {
		t.Error("no config.yml: vouched for")
	}
}

func TestProxyConfig_OtherSoftwareIsNeverConfigured(t *testing.T) {
	for _, typ := range []string{"vanilla", "fabric", "forge", "neoforge", "upload", "modpack"} {
		serverPath, sub, subDir := newSubServer(t)
		if writeProxyProtocolConfig(serverPath, sub, InstallerConfig{Type: typ}) {
			t.Errorf("%s: reported written", typ)
		}
		if entries, _ := os.ReadDir(subDir); len(entries) != 0 {
			t.Errorf("%s: wrote %d entries", typ, len(entries))
		}
	}
}

// A sub-server that is a link out of the server directory is not followed.
func TestProxyConfig_SubServerLinkIsNotFollowed(t *testing.T) {
	serverPath := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(serverPath, "main")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if writeProxyProtocolConfig(serverPath, "main", InstallerConfig{Type: "paper"}) {
		t.Fatal("wrote through a link")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote %d entries outside the server directory", len(entries))
	}
}
