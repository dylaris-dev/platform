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
