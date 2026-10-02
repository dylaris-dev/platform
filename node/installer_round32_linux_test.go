package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// .upload.zip is a name in the tenant's own directory. A link planted under it
// pointed the installer, running as root, at any archive on the host - another
// tenant's upload included - and unpacked it here.
func TestAnUploadZipThatIsALinkIsNotFollowed(t *testing.T) {
	root := t.TempDir()
	dest := filepath.Join(root, "survival")
	os.MkdirAll(dest, 0o755)
	foreign := filepath.Join(t.TempDir(), "other-tenant.zip")
	zipWith(t, foreign, map[string]string{"world/level.dat": "theirs"})
	if err := os.Symlink(foreign, filepath.Join(dest, ".upload.zip")); err != nil {
		t.Fatal(err)
	}
	if err := installFromUploadZip(dest, "flat"); err == nil {
		t.Fatal("an upload that is a link was unpacked")
	}
	if _, err := os.Stat(filepath.Join(dest, "world", "level.dat")); err == nil {
		t.Fatal("another tenant's archive was unpacked into this server")
	}
}

// The same for a backup import.
func TestABackupImportThatIsALinkIsNotFollowed(t *testing.T) {
	dest := t.TempDir()
	foreign := filepath.Join(t.TempDir(), "x.tar.gz")
	os.WriteFile(foreign, []byte("not even a gzip"), 0o644)
	link := filepath.Join(dest, backupImportArchiveName)
	if err := os.Symlink(foreign, link); err != nil {
		t.Fatal(err)
	}
	if _, err := unpackBackupArchive(link, dest); err == nil {
		t.Fatal("a backup archive that is a link was opened")
	}
}

// eula.txt is a name the tenant can plant a link under; the plain write
// followed it to the node's own config in the server root.
func TestTheEULAIsNotWrittenThroughALink(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "survival"), 0o755)
	cfg := filepath.Join(root, ".node_config.json")
	os.WriteFile(cfg, []byte(`{"ram":2048}`), 0o644)
	if err := os.Symlink("../.node_config.json", filepath.Join(root, "survival", "eula.txt")); err != nil {
		t.Fatal(err)
	}
	writeEULA(root, "survival")
	if got, _ := os.ReadFile(cfg); string(got) != `{"ram":2048}` {
		t.Fatalf("the EULA was written into the node's config: %q", got)
	}
	// And an ordinary sub-server gets its EULA.
	os.MkdirAll(filepath.Join(root, "lobby"), 0o755)
	if err := writeEULA(root, "lobby"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "lobby", "eula.txt")); string(got) != "eula=true\n" {
		t.Fatalf("eula.txt = %q", got)
	}
}

// The modpack's working directory has a fixed name; a link planted there was
// adopted, and pack.mrpack was created wherever it pointed.
func TestTheModpackWorkDirIsNotALink(t *testing.T) {
	dest := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, mrpackTempDir)); err != nil {
		t.Fatal(err)
	}
	// A download that succeeds, so the old code would really have written:
	// Core's own mirror host is the one an install may fetch from directly.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("PK not really a pack"))
	}))
	defer srv.Close()
	tr := http.DefaultTransport.(*http.Transport)
	prevTLS := tr.TLSClientConfig
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	defer func() { tr.TLSClientConfig = prevTLS }()
	host := strings.TrimPrefix(srv.URL, "https://")
	prevHost, _ := coreMirrorHost.Load().(string)
	coreMirrorHost.Store(host)
	defer coreMirrorHost.Store(prevHost)

	err := installModpack(dest, InstallerConfig{URL: srv.URL + "/pack.mrpack"})
	if err == nil {
		t.Fatal("the install went ahead through a linked work directory")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("the modpack wrote outside the server: %v", ents)
	}
}
