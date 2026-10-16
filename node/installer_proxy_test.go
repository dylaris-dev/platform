package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeFill answers the Fill API and the jar download for one project without a
// network. checksum "" leaves the field out of the response, the way a source
// without one answers. Every request URL is recorded.
type fakeFill struct {
	jar      string
	checksum string
	seen     *[]string
}

func (f fakeFill) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.seen != nil {
		*f.seen = append(*f.seen, r.URL.String())
	}
	body := f.jar
	if strings.Contains(r.URL.Path, "/builds/latest") {
		sums := ""
		if f.checksum != "" {
			sums = fmt.Sprintf(`,"checksums":{"sha256":%q}`, f.checksum)
		}
		body = `{"id":615,"downloads":{"server:default":{"name":"x.jar","url":"https://fill-data.papermc.io/x.jar"` + sums + `}}}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

func useFakeFill(t *testing.T, f fakeFill) {
	t.Helper()
	meta, dl := installerMetaClient, installerDownloadClient
	installerMetaClient = &http.Client{Transport: f}
	installerDownloadClient = &http.Client{Transport: f}
	t.Cleanup(func() { installerMetaClient, installerDownloadClient = meta, dl })
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestInstallPaperMC_Checksum(t *testing.T) {
	const jar = "THE-JAR"
	cases := []struct {
		name     string
		project  string
		checksum string
		wantErr  bool
	}{
		{"matching checksum installs", "velocity", sha256Hex(jar), false},
		{"upper-case checksum installs", "waterfall", strings.ToUpper(sha256Hex(jar)), false},
		{"missing checksum still installs", "paper", "", false},
		{"mismatch fails", "paper", sha256Hex("something else"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useFakeFill(t, fakeFill{jar: jar, checksum: tc.checksum})
			dir := t.TempDir()
			err := installPaperMC(dir, tc.project, "1.21")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			got, readErr := os.ReadFile(filepath.Join(dir, "server.jar"))
			if tc.wantErr {
				if readErr == nil {
					t.Fatalf("a failed install left server.jar behind")
				}
			} else if string(got) != jar {
				t.Fatalf("server.jar = %q, %v", got, readErr)
			}
			if _, err := os.Stat(filepath.Join(dir, "server.jar.part")); err == nil {
				t.Fatalf("server.jar.part left behind")
			}
		})
	}
}

// A mismatch must not destroy the jar a reinstall was replacing.
func TestInstallPaperMC_MismatchKeepsOldJar(t *testing.T) {
	useFakeFill(t, fakeFill{jar: "NEW", checksum: sha256Hex("other")})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "server.jar"), []byte("OLD"), 0o644)
	if err := installPaperMC(dir, "paper", "1.21.4"); err == nil {
		t.Fatal("want a checksum error")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "server.jar")); string(got) != "OLD" {
		t.Fatalf("server.jar = %q, want the old jar kept", got)
	}
}

func TestInstallPaperMC_ProjectInURL(t *testing.T) {
	var seen []string
	useFakeFill(t, fakeFill{jar: "J", seen: &seen})
	if err := installPaperMC(t.TempDir(), "velocity", "3.5.1"); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 || seen[0] != "https://fill.papermc.io/v3/projects/velocity/versions/3.5.1/builds/latest" {
		t.Fatalf("requests = %v", seen)
	}
	if err := installPaperMC(t.TempDir(), "bungeecord", "1"); err == nil {
		t.Fatal("a project Fill does not serve must be refused")
	}
}

func TestBungeeCordBuild(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"#2104", "2104", true},
		{"2104", "2104", true},
		{" #7 ", "7", true},
		{"", "", false},
		{"#", "", false},
		{"latest", "", false},
		{"#21a4", "", false},
		{"#2104/../../x", "", false},
		{"1.21", "", false},
		{"#1234567890", "", false},
	}
	for _, tc := range cases {
		got, err := bungeeCordBuild(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("bungeeCordBuild(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestInstallServer_BungeeCord(t *testing.T) {
	var seen []string
	useFakeFill(t, fakeFill{jar: "BUNGEE", seen: &seen})
	data := t.TempDir()
	if _, err := InstallServer(data, "proxy", InstallerConfig{Type: "bungeecord", Version: "#2104", ContainerPort: 25570}); err != nil {
		t.Fatal(err)
	}
	if want := "https://hub.spigotmc.org/jenkins/job/BungeeCord/2104/artifact/bootstrap/target/BungeeCord.jar"; len(seen) != 1 || seen[0] != want {
		t.Fatalf("requests = %v, want %s", seen, want)
	}
	if got, _ := os.ReadFile(filepath.Join(data, "proxy", "server.jar")); string(got) != "BUNGEE" {
		t.Fatalf("server.jar = %q", got)
	}
	cfg, _ := os.ReadFile(filepath.Join(data, "proxy", "config.yml"))
	if !strings.Contains(string(cfg), "host: 0.0.0.0:25570") {
		t.Fatalf("config.yml = %q", cfg)
	}

	seen = nil
	if _, err := InstallServer(data, "proxy2", InstallerConfig{Type: "bungeecord", Version: "latest"}); err == nil {
		t.Fatal("a non-numeric build must be refused")
	}
	if len(seen) != 0 {
		t.Fatalf("refused build still downloaded: %v", seen)
	}
}

func TestInstallServer_VelocityWritesNoConfig(t *testing.T) {
	useFakeFill(t, fakeFill{jar: "VELO"})
	data := t.TempDir()
	if _, err := InstallServer(data, "proxy", InstallerConfig{Type: "velocity", Version: "3.5.1"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.yml", "velocity.toml"} {
		if _, err := os.Stat(filepath.Join(data, "proxy", name)); err == nil {
			t.Errorf("%s written for Velocity; it must generate its own", name)
		}
	}
	if lf := resolveLaunch(filepath.Join(data, "proxy")); lf.Mode != launchJar || lf.Jar != "server.jar" {
		t.Fatalf("launch = %+v, want server.jar", lf)
	}
}

func TestWriteBungeeListener(t *testing.T) {
	t.Run("absent: written with the container port", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeBungeeListener(dir, 25599); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
		if string(got) != "listeners:\n- host: 0.0.0.0:25599\n" {
			t.Fatalf("config.yml = %q", got)
		}
	})
	t.Run("absent, no port: node default", func(t *testing.T) {
		dir := t.TempDir()
		if err := writeBungeeListener(dir, 0); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
		port := getContainerPort()
		if port <= 0 {
			port = 25565
		}
		if want := fmt.Sprintf("host: 0.0.0.0:%d", port); !strings.Contains(string(got), want) {
			t.Fatalf("config.yml = %q, want %s", got, want)
		}
	})
	t.Run("present: left alone", func(t *testing.T) {
		dir := t.TempDir()
		mine := "listeners:\n- host: 0.0.0.0:25577\n  motd: mine\n"
		os.WriteFile(filepath.Join(dir, "config.yml"), []byte(mine), 0o644)
		if err := writeBungeeListener(dir, 25565); err != nil {
			t.Fatal(err)
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "config.yml")); string(got) != mine {
			t.Fatalf("config.yml overwritten: %q", got)
		}
	})
}

func TestConsoleStopCommand(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		sub   string
		want  string
	}{
		{"bungeecord config", map[string]string{"config.yml": "listeners:\n- query_port: 25577\n  host: 0.0.0.0:25565\n"}, "proxy", "end"},
		{"listeners after other keys", map[string]string{"config.yml": "server_connect_timeout: 5000\nlisteners:\n- host: 0.0.0.0:25565\n"}, "proxy", "end"},
		{"paper root without config.yml", map[string]string{"server.properties": "motd=x", "bukkit.yml": "settings: {}", "spigot.yml": "settings: {}", "commands.yml": "aliases: {}", "plugins/X/config.yml": "listeners:\n"}, "proxy", "stop"},
		{"config.yml without a top-level listeners", map[string]string{"config.yml": "bedrock:\n  listeners:\n  - port: 19132\n# listeners: none\n"}, "proxy", "stop"},
		{"velocity", map[string]string{"velocity.toml": "bind = \"0.0.0.0:25565\"\n"}, "proxy", "stop"},
		{"no sub-server", map[string]string{"config.yml": "listeners:\n"}, "", "stop"},
		{"missing sub-server", nil, "gone", "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serverDir := t.TempDir()
			for name, body := range tc.files {
				p := filepath.Join(serverDir, "proxy", filepath.FromSlash(name))
				os.MkdirAll(filepath.Dir(p), 0o755)
				os.WriteFile(p, []byte(body), 0o644)
			}
			if got := consoleStopCommand(serverDir, tc.sub); got != tc.want {
				t.Fatalf("consoleStopCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSaveUnsupported(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"[12:00:00] [Server thread/INFO]: Unknown command. Type \"/help\" for help.", true},
		{"[12:00:00 INFO]: This command does not exist.", true},
		{"12:00:00 [INFO] Command not found", true},
		{"[12:00:00 INFO]: Command not found", true},
		{"12:00:00 [INFO] \x1b[0;31;1mCommand not found\x1b[m", true},
		{"Command not found", true},
		{"[12:00:00] [Server thread/INFO]: Saved the game", false},
		{"[12:00:00 INFO]: <Steve> Command not found", false},
		{"[12:00:00 INFO]: [MyPlugin] lookup: Command not found in cache", false},
		{"[12:00:00 INFO]: This command does not exist. Try /help", false},
		{"[12:00:00] [Server thread/INFO]: Command not foundation laid", false},
	}
	for _, tc := range cases {
		if got := saveUnsupported(tc.line); got != tc.want {
			t.Errorf("saveUnsupported(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
}
