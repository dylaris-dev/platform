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

type fakePaperTransport struct{}

func (fakePaperTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := "PAPERJAR"
	if strings.Contains(r.URL.Path, "/builds/latest") {
		body = `{"id":69,"downloads":{"server:default":{"name":"paper.jar","url":"https://fill-data.papermc.io/paper.jar"}}}`
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

// fakePaper answers the PaperMC API and the jar download without a network.
func fakePaper(t *testing.T) {
	t.Helper()
	meta, dl := installerMetaClient, installerDownloadClient
	installerMetaClient = &http.Client{Transport: fakePaperTransport{}}
	installerDownloadClient = &http.Client{Transport: fakePaperTransport{}}
	t.Cleanup(func() { installerMetaClient, installerDownloadClient = meta, dl })
}

func writeUploadZip(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, ".upload.zip"))
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
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

// The upload that never started: a Paper server whose jar lives under
// versions/, nothing in the top folder.
var paperUpload = map[string]string{
	"versions/1.21.11/paper-1.21.11.jar": "old",
	"libraries/io/papermc/x.jar":         "lib",
	"world/level.dat":                    "w",
	"plugins/P.jar":                      "p",
	"server.properties":                  "motd=hi",
	"paper-1.21.8.jar":                   "stale root jar",
}

func TestUploadZip_NoJarNoSoftware_Refused(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "main")
	os.MkdirAll(dir, 0o755)
	writeUploadZip(t, dir, map[string]string{"versions/1.21.11/paper-1.21.11.jar": "x", "world/level.dat": "w"})

	_, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "direct"})
	if err == nil || !strings.Contains(err.Error(), "choose the server software") {
		t.Fatalf("want a refusal naming the fix, got %v", err)
	}
}

func TestUploadZip_OwnJarKept(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "main")
	os.MkdirAll(dir, 0o755)
	writeUploadZip(t, dir, map[string]string{"server.jar": "mine", "world/level.dat": "w"})

	if _, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "direct"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "server.jar")); string(b) != "mine" {
		t.Fatalf("own jar replaced: %q", b)
	}
}

func TestUploadZip_SoftwareInstalledOverUpload(t *testing.T) {
	fakePaper(t)
	data := t.TempDir()
	dir := filepath.Join(data, "main")
	os.MkdirAll(dir, 0o755)
	writeUploadZip(t, dir, paperUpload)

	if _, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "direct", Software: "paper", Version: "1.21.11"}); err != nil {
		t.Fatal(err)
	}
	lf := resolveLaunch(dir)
	if lf.Mode != launchJar || lf.Jar != "server.jar" {
		t.Fatalf("start would use %+v, want the installed server.jar", lf)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "server.jar")); string(b) != "PAPERJAR" {
		t.Fatalf("server.jar = %q", b)
	}
	for _, gone := range []string{"paper-1.21.8.jar", "versions"} {
		if exists(filepath.Join(dir, gone)) {
			t.Errorf("%s survived", gone)
		}
	}
	for _, kept := range []string{"world/level.dat", "plugins/P.jar", "server.properties", "libraries/io/papermc/x.jar"} {
		if !exists(filepath.Join(dir, kept)) {
			t.Errorf("%s lost", kept)
		}
	}
}

// A stale Forge argfile outranks every jar, so it goes with libraries/.
func TestUploadSoftware_ForgeArgfileCleared(t *testing.T) {
	fakePaper(t)
	data := t.TempDir()
	dir := filepath.Join(data, "main")
	os.MkdirAll(dir, 0o755)
	writeUploadZip(t, dir, map[string]string{
		"libraries/net/minecraftforge/forge/1.20.1-47.2.0/unix_args.txt": "-cp x",
		"world/level.dat": "w",
	})

	if _, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "direct", Software: "paper", Version: "1.21.11"}); err != nil {
		t.Fatal(err)
	}
	if lf := resolveLaunch(dir); lf.Mode != launchJar {
		t.Fatalf("start would use %+v, want the installed jar", lf)
	}
	if !exists(filepath.Join(dir, "world/level.dat")) {
		t.Fatal("world lost")
	}
}

func TestUploadSoftware_OnlyServerInstallers(t *testing.T) {
	data := t.TempDir()
	os.MkdirAll(filepath.Join(data, "main"), 0o755)
	for _, sw := range []string{"import", "library", "modpack", "upload", "technic"} {
		_, err := InstallServer(data, "main", InstallerConfig{Type: "upload", Software: sw, URL: "http://169.254.169.254/"})
		if err == nil || !strings.Contains(err.Error(), "unsupported server software") {
			t.Errorf("software %q: got %v", sw, err)
		}
	}
}

// The folder to move up comes from the archive: a sub-server that already
// holds other folders used to keep the upload one level down, unused.
func TestUploadZip_SubfolderMovedUpBesideExistingFolders(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "main")
	os.MkdirAll(filepath.Join(dir, "logs"), 0o755)
	os.MkdirAll(filepath.Join(dir, "cache"), 0o755)
	writeUploadZip(t, dir, map[string]string{
		"srv/server.jar":            "mine",
		"srv/world/level.dat":       "w",
		"__MACOSX/srv/._server.jar": "junk",
		".DS_Store":                 "junk",
	})
	if _, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "subfolder"}); err != nil {
		t.Fatal(err)
	}
	if lf := resolveLaunch(dir); lf.Mode != launchJar || lf.Jar != "server.jar" {
		t.Fatalf("start would use %+v", lf)
	}
	if !exists(filepath.Join(dir, "world", "level.dat")) || exists(filepath.Join(dir, "srv")) {
		t.Fatal("the upload was not moved up")
	}
}

// Existing files are refused by name instead of silently kept in use.
func TestUploadZip_SubfolderClashRefused(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "main")
	os.MkdirAll(dir, 0o755)
	// A file, not a folder: a rename onto a file replaces it without an error.
	os.WriteFile(filepath.Join(dir, "server.properties"), []byte("old"), 0o644)
	writeUploadZip(t, dir, map[string]string{"srv/server.jar": "new", "srv/server.properties": "new"})
	_, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "subfolder"})
	if err == nil || !strings.Contains(err.Error(), "server.properties") {
		t.Fatalf("want a refusal naming server.properties, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "server.properties")); string(b) != "old" {
		t.Fatalf("existing file replaced: %q", b)
	}
}

// A file beside the folder means the archive is the server's top folder.
func TestUploadZip_FileBesideFolderNotMoved(t *testing.T) {
	data := t.TempDir()
	dir := filepath.Join(data, "main")
	os.MkdirAll(dir, 0o755)
	writeUploadZip(t, dir, map[string]string{"server.jar": "mine", "world/level.dat": "w"})
	if _, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "subfolder"}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, "world", "level.dat")) || exists(filepath.Join(dir, "level.dat")) {
		t.Fatal("world was moved up")
	}
}
