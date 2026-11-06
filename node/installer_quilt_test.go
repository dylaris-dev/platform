package main

import (
	"archive/zip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeQuilt answers Quilt's meta, the two Mavens and Mojang without a network.
// libName is the one library the launch profile lists. Every URL is recorded.
type fakeQuilt struct {
	libName string
	seen    *[]string
}

func (f fakeQuilt) RoundTrip(r *http.Request) (*http.Response, error) {
	u := r.URL.String()
	*f.seen = append(*f.seen, u)
	body, code := "", 200
	switch {
	case u == "https://meta.quiltmc.org/v3/versions/loader/1.20.1/0.26.4/server/json":
		body = `{"launcherMainClass":"org.quiltmc.loader.impl.launch.server.QuiltServerLauncher",` +
			`"libraries":[{"name":"` + f.libName + `","url":"https://maven.quiltmc.org/repository/release/"}]}`
	case strings.HasPrefix(u, "https://maven.quiltmc.org/"):
		body = "LIB"
	case u == "https://launchermeta.mojang.com/mc/game/version_manifest.json":
		body = `{"versions":[{"id":"1.20.1","url":"https://piston-meta.mojang.com/v/1.20.1.json"}]}`
	case u == "https://piston-meta.mojang.com/v/1.20.1.json":
		body = `{"downloads":{"server":{"url":"https://piston-data.mojang.com/server.jar"}}}`
	case u == "https://piston-data.mojang.com/server.jar":
		body = "VANILLA"
	default:
		code = 404
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

func useFakeQuilt(t *testing.T, f fakeQuilt) {
	t.Helper()
	meta, dl := installerMetaClient, installerDownloadClient
	installerMetaClient = &http.Client{Transport: f}
	installerDownloadClient = &http.Client{Transport: f}
	t.Cleanup(func() { installerMetaClient, installerDownloadClient = meta, dl })
}

// A Quilt pack used to go through Fabric's meta with the Quilt loader version,
// a loader Fabric does not have, so every Quilt pack failed after downloading
// all of its mods. It now installs what Quilt's own installer lays down.
func TestModpackQuiltLoaderInstallsQuilt(t *testing.T) {
	cases := []struct {
		name    string
		lib     string
		wantErr bool
	}{
		{"quilt loader", "org.quiltmc:quilt-loader:0.26.4", false},
		{"library escaping libraries/", "org.quiltmc:..:0.26.4", true},
		{"library with a slash", "org/../..:x:1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var seen []string
			useFakeQuilt(t, fakeQuilt{libName: c.lib, seen: &seen})
			dir := t.TempDir()
			err := installMrpackLoader(dir, map[string]string{"minecraft": "1.20.1", "quilt-loader": "0.26.4"}, InstallerConfig{})
			for _, u := range seen {
				if strings.Contains(u, "fabricmc.net") {
					t.Errorf("a Quilt pack asked Fabric: %s", u)
				}
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if got := DetectServerJar(dir); got != "quilt-server-launch.jar" {
				t.Errorf("detected %q, want quilt-server-launch.jar", got)
			}
			if b, _ := os.ReadFile(filepath.Join(dir, "server.jar")); string(b) != "VANILLA" {
				t.Errorf("server.jar = %q", b)
			}
			lib := filepath.Join(dir, "libraries", "org", "quiltmc", "quilt-loader", "0.26.4", "quilt-loader-0.26.4.jar")
			if b, _ := os.ReadFile(lib); string(b) != "LIB" {
				t.Errorf("library = %q", b)
			}
			zr, err := zip.OpenReader(filepath.Join(dir, "quilt-server-launch.jar"))
			if err != nil {
				t.Fatal(err)
			}
			defer zr.Close()
			if len(zr.File) != 1 || zr.File[0].Name != "META-INF/MANIFEST.MF" {
				t.Fatalf("launch jar holds %d entries", len(zr.File))
			}
			rc, _ := zr.File[0].Open()
			mf, _ := io.ReadAll(rc)
			rc.Close()
			for _, line := range strings.Split(string(mf), "\r\n") {
				if len(line) > 72 {
					t.Errorf("manifest line over 72 bytes: %q", line)
				}
			}
			mf = []byte(strings.ReplaceAll(string(mf), "\r\n ", ""))
			for _, want := range []string{
				"Main-Class: org.quiltmc.loader.impl.launch.server.QuiltServerLauncher\r\n",
				"Class-Path: libraries/org/quiltmc/quilt-loader/0.26.4/quilt-loader-0.26.4.jar\r\n",
			} {
				if !strings.Contains(string(mf), want) {
					t.Errorf("manifest %q lacks %q", mf, want)
				}
			}
		})
	}
}
