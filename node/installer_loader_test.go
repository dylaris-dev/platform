package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A Forge/NeoForge install leaves neither its installer jar nor, once it
// worked, the installer's log; a failed download leaves no empty jar.
func TestRunLoaderInstallerCleansUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.jar" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("jar"))
	}))
	defer srv.Close()
	prevClient, prevRun := installerDownloadClient, runJavaInstallerFn
	installerDownloadClient = srv.Client()
	t.Cleanup(func() { installerDownloadClient, runJavaInstallerFn = prevClient, prevRun })

	const jar = "_forge-installer.jar"
	cases := []struct {
		name    string
		path    string
		runErr  error
		wantErr bool
		logLeft bool
	}{
		{name: "success removes jar and log", path: "/ok.jar"},
		{name: "failed download leaves no jar", path: "/missing.jar", wantErr: true},
		{name: "failed installer keeps its log", path: "/ok.jar", runErr: errors.New("exit 1"), wantErr: true, logLeft: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ran := false
			runJavaInstallerFn = func(_, _, _, installerJAR string, _ ...string) error {
				ran = true
				if _, err := os.Stat(filepath.Join(dir, installerJAR)); err != nil {
					t.Errorf("the installer ran without its jar: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, installerJAR+".log"), []byte("log"), 0o644); err != nil {
					t.Fatal(err)
				}
				return c.runErr
			}
			err := runLoaderInstaller(dir, "forge", srv.URL+c.path, jar, "java:21", "u", "--installServer")
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if ran == (c.path == "/missing.jar") {
				t.Fatalf("installer ran = %v", ran)
			}
			if _, err := os.Stat(filepath.Join(dir, jar)); !os.IsNotExist(err) {
				t.Errorf("installer jar left behind (stat err %v)", err)
			}
			_, err = os.Stat(filepath.Join(dir, jar+".log"))
			if left := err == nil; left != c.logLeft {
				t.Errorf("installer log left = %v, want %v", left, c.logLeft)
			}
		})
	}
}

// A blank NeoForge build means the newest one for the Minecraft version the
// install names. A version move sends exactly that, and the newest NeoForge
// overall belongs to whatever Minecraft line came out last.
func TestPickNeoForgeVersion(t *testing.T) {
	var meta neoForgeMetadata
	meta.Versioning.Release = "21.10.5"
	meta.Versioning.Versions.Version = []string{
		"20.6.120", "21.0.167", "21.1.9", "21.1.265", "21.1.30", "21.10.5",
		"21.11.0-beta", "26.1.0.3-beta", "26.1.2.5",
	}
	for _, c := range []struct{ mc, want string }{
		{"", "21.10.5"},
		{"1.21.1", "21.1.265"}, // numeric, not string order
		{"1.21", "21.0.167"},
		{"1.20.6", "20.6.120"},
		{"1.21.11", "21.11.0-beta"}, // only a pre-release exists
		{"26.1", "26.1.0.3-beta"},
		{"26.1.2", "26.1.2.5"},
		{"21.1.30", "21.1.30"}, // a NeoForge version itself
		{"1.19.2", ""},
	} {
		if got := pickNeoForgeVersion(meta, c.mc); got != c.want {
			t.Errorf("pickNeoForgeVersion(%q) = %q, want %q", c.mc, got, c.want)
		}
	}
}
