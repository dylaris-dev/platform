package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A library install used to copy whatever local path the request named into the
// tenant's server. The node must not read a path from an install request at all.
func TestLibraryInstallRefusesALocalPath(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "environ")
	if err := os.WriteFile(secret, []byte("CLUSTER_SECRET=do-not-leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := installFromLibrary(dest, secret, ""); err == nil {
		t.Fatal("a library install with a local path was accepted")
	}
	if _, err := os.Stat(filepath.Join(dest, "server.jar")); err == nil {
		t.Fatal("the local file was copied into the server")
	}
	// Even with a valid URL beside it, the path is refused rather than ignored.
	withCoreMirrorHost(t, "core.example")
	if err := installFromLibrary(dest, secret, "https://core.example/mirror/library/x.jar"); err == nil {
		t.Fatal("a library install naming a local path next to a URL was accepted")
	}
}

func TestLibraryInstallOnlyDownloadsFromCore(t *testing.T) {
	withCoreMirrorHost(t, "core.example")
	for _, u := range []string{
		"https://evil.example/x.jar",
		"https://core.example@evil.example/x.jar",
		"https://core.example.evil.example/x.jar",
		"http://core.example/x.jar",
		"https://169.254.169.254/hetzner/v1/metadata",
	} {
		if err := installFromLibrary(t.TempDir(), "", u); err == nil {
			t.Errorf("library install from %s was accepted", u)
		}
	}
	withCoreMirrorHost(t, "")
	if err := installFromLibrary(t.TempDir(), "", "https://core.example/x.jar"); err == nil {
		t.Error("library install accepted with no Core address known")
	}
}

func TestLibraryInstallDownloadsFromCore(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("jar-bytes"))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	withCoreMirrorHost(t, u.Host)
	prev := installerDownloadClient
	installerDownloadClient = srv.Client()
	t.Cleanup(func() { installerDownloadClient = prev })

	dest := t.TempDir()
	if err := installFromLibrary(dest, "", srv.URL+"/mirror/library/1/sig/paper.jar"); err != nil {
		t.Fatalf("install from Core: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "server.jar"))
	if err != nil || string(got) != "jar-bytes" {
		t.Fatalf("server.jar = %q, %v", got, err)
	}
}

// "import" fetches a caller-supplied URL into the tenant's server, so it must
// not reach the node's private network.
func TestImportInstallRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("internal"))
	}))
	defer srv.Close()
	dest := t.TempDir()
	err := installFromURL(dest, srv.URL+"/server.jar", downloadImportGuarded)
	if err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("import from loopback: err = %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "server.jar")); err == nil {
		t.Fatal("the internal response was written into the server")
	}
}

// A pack's per-file URLs are chosen by whoever wrote the pack. The host
// allowlist checks only the first URL, so the download itself must refuse a
// private address, which is where a redirect would send it.
func TestPackFileDownloadRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("internal"))
	}))
	defer srv.Close()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := downloadBoundedGuarded(root, "mod.jar", srv.URL+"/mod.jar", 1<<20); err == nil {
		t.Fatal("a pack file was downloaded from a private address")
	}
}
