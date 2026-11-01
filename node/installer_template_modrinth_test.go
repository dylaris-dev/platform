package main

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serveModrinthCDN serves body for every path and routes the fetch step to it:
// the URL still has to pass the real cdn.modrinth.com check, only the network
// is replaced, and the real downloadAndVerify does the hashing.
func serveModrinthCDN(t *testing.T, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	orig := downloadTemplateFile
	downloadTemplateFile = func(url, dest, sha string, max int64) error {
		return downloadAndVerifyMax(srv.URL+"/"+strings.TrimPrefix(url, "https://cdn.modrinth.com/"), dest, sha, max)
	}
	t.Cleanup(func() { downloadTemplateFile = orig })
}

func sha512Of(s string) string {
	h := sha512.Sum512([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestInstallTemplateModrinth(t *testing.T) {
	const jar = "PK fake jar bytes"
	good := TemplateModrinthFile{Path: "plugins/LuckPerms.jar", URL: "https://cdn.modrinth.com/data/abc/versions/def/LuckPerms.jar", Sha512: sha512Of(jar), Size: int64(len(jar))}
	with := func(mut func(*TemplateModrinthFile)) TemplateModrinthFile {
		f := good
		mut(&f)
		return f
	}
	cases := []struct {
		name    string
		file    TemplateModrinthFile
		wantErr string
		lands   string
	}{
		{"happy path", good, "", "plugins/LuckPerms.jar"},
		{"nested under mods", with(func(f *TemplateModrinthFile) { f.Path = "mods/extra/x.jar" }), "", "mods/extra/x.jar"},
		{"upper-case hash", with(func(f *TemplateModrinthFile) { f.Sha512 = strings.ToUpper(f.Sha512) }), "", "plugins/LuckPerms.jar"},
		{"size smaller than the body", with(func(f *TemplateModrinthFile) { f.Size = 4 }), "exceeds the 4 byte limit", ""},
		{"no size uses the general cap", with(func(f *TemplateModrinthFile) { f.Size = 0 }), "", "plugins/LuckPerms.jar"},
		{"wrong sha512", with(func(f *TemplateModrinthFile) { f.Sha512 = sha512Of("other") }), "sha512 mismatch", ""},
		{"missing sha512", with(func(f *TemplateModrinthFile) { f.Sha512 = "" }), "no valid sha512", ""},
		{"non-cdn url", with(func(f *TemplateModrinthFile) { f.URL = "https://evil.example/LuckPerms.jar" }), "cdn.modrinth.com", ""},
		{"cdn lookalike", with(func(f *TemplateModrinthFile) { f.URL = "https://cdn.modrinth.com.evil.example/x.jar" }), "cdn.modrinth.com", ""},
		{"traversal", with(func(f *TemplateModrinthFile) { f.Path = "plugins/../../escape.jar" }), "mods/ or plugins/", ""},
		{"traversal back into plugins is cleaned", with(func(f *TemplateModrinthFile) { f.Path = "mods/../plugins/y.jar" }), "", "plugins/y.jar"},
		{"root jar", with(func(f *TemplateModrinthFile) { f.Path = "server.jar" }), "mods/ or plugins/", ""},
		{"other folder", with(func(f *TemplateModrinthFile) { f.Path = "config/x.jar" }), "mods/ or plugins/", ""},
		{"not a jar", with(func(f *TemplateModrinthFile) { f.Path = "plugins/x.sh" }), "mods/ or plugins/", ""},
		{"plugins itself", with(func(f *TemplateModrinthFile) { f.Path = "plugins" }), "mods/ or plugins/", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			serveModrinthCDN(t, jar)
			dir := t.TempDir()
			err := installTemplateModrinth(dir, []TemplateModrinthFile{c.file})
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				// Named, so the operator knows which file.
				if !strings.Contains(err.Error(), c.file.Path) {
					t.Fatalf("error does not name the file: %v", err)
				}
				// Nothing left behind, not even a partial download.
				var left []string
				_ = filepath.Walk(dir, func(p string, info os.FileInfo, _ error) error {
					if info != nil && !info.IsDir() {
						left = append(left, p)
					}
					return nil
				})
				if len(left) != 0 {
					t.Fatalf("files left after a refusal: %v", left)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(c.lands)))
			if err != nil || string(got) != jar {
				t.Fatalf("read %s: %q, %v", c.lands, got, err)
			}
		})
	}
}

// One bad file fails the whole install, after the good ones before it.
func TestInstallTemplateModrinthStopsAtTheFirstFailure(t *testing.T) {
	const jar = "jar"
	serveModrinthCDN(t, jar)
	dir := t.TempDir()
	err := installTemplateModrinth(dir, []TemplateModrinthFile{
		{Path: "mods/a.jar", URL: "https://cdn.modrinth.com/a.jar", Sha512: sha512Of(jar)},
		{Path: "mods/b.jar", URL: "https://cdn.modrinth.com/b.jar", Sha512: sha512Of("nope")},
	})
	if err == nil || !strings.Contains(err.Error(), "mods/b.jar") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mods", "b.jar")); !os.IsNotExist(err) {
		t.Fatalf("b.jar exists: %v", err)
	}
}

func TestInstallTemplateModrinthCapsTheList(t *testing.T) {
	files := make([]TemplateModrinthFile, templateModrinthMax+1)
	if err := installTemplateModrinth(t.TempDir(), files); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v", err)
	}
}

// An install config from Core carries the files under modrinthFiles.
func TestInstallerConfigDecodesModrinthFiles(t *testing.T) {
	var cfg InstallerConfig
	if err := json.Unmarshal([]byte(`{"type":"upload-zip","modrinthFiles":[{"path":"mods/a.jar","url":"https://cdn.modrinth.com/a.jar","sha512":"ab","size":3}]}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.ModrinthFiles) != 1 || cfg.ModrinthFiles[0] != (TemplateModrinthFile{"mods/a.jar", "https://cdn.modrinth.com/a.jar", "ab", 3}) {
		t.Fatalf("decoded %+v", cfg.ModrinthFiles)
	}
}

// A "plugins" link planted by the tenant's container leads nowhere.
func TestInstallTemplateModrinthRefusesALinkedFolder(t *testing.T) {
	const jar = "jar"
	serveModrinthCDN(t, jar)
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "plugins")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	err := installTemplateModrinth(dir, []TemplateModrinthFile{{Path: "plugins/x.jar", URL: "https://cdn.modrinth.com/x.jar", Sha512: sha512Of(jar)}})
	if err == nil {
		t.Fatal("installed through a link")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("wrote outside: %v", entries)
	}
}
