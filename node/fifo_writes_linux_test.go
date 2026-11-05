package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A named pipe the tenant's server planted at a name the node later opens -
// for a write, or for the read a stop makes - is refused at once. A blocking
// open of a pipe with no peer never returns, and each one held a command
// worker for good.
func TestTenantPlantedPipesDoNotBlockTheNode(t *testing.T) {
	cases := []struct {
		name string
		pipe string // relative to the server directory
		run  func(serverDir string) error
	}{
		{"stop reads config.yml", "main/config.yml", func(dir string) error {
			if got := consoleStopCommand(dir, "main"); got != "stop" {
				t.Errorf("console stop command = %q, want stop", got)
			}
			return errRefusedForTest
		}},
		// The stats collector reads it inline, so a blocked open there stopped
		// that server's disk and memory guards.
		{"ping reads server.properties", "srv-pipe-0001/main/server.properties", func(dir string) error {
			prevMgr := globalStorageMgr
			defer func() { globalStorageMgr = prevMgr }()
			globalStorageMgr = NewStorageManager(dir, nil)
			if statusPingDisabled("srv-pipe-0001", "/data/main") {
				t.Error("a pipe read as enable-status=false")
			}
			return errRefusedForTest
		}},
		{"download to server.jar.part", "main/server.jar.part", func(dir string) error {
			// More than a pipe buffer holds, so a write into the pipe blocks too.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write(make([]byte, 1<<20))
			}))
			defer srv.Close()
			dest := filepath.Join(dir, "main", "server.jar.part")
			if err := downloadWith(http.DefaultClient, srv.URL, dest, time.Second, 0); err != nil {
				return err
			}
			// The download lands beside the pipe and is renamed over it, which
			// replaces the pipe rather than writing into it.
			if st, err := os.Lstat(dest); err != nil || !st.Mode().IsRegular() {
				t.Errorf("server.jar.part is not a regular file after the download: %v", err)
			}
			return errRefusedForTest
		}},
		{"createIn", "main/mods/a.jar", func(dir string) error {
			root, err := os.OpenRoot(dir)
			if err != nil {
				return err
			}
			defer root.Close()
			f, err := createIn(root, "main/mods/a.jar", 0o644)
			if f != nil {
				f.Close()
			}
			return err
		}},
		{"floodgate key", "main/plugins/floodgate/key.pem", func(dir string) error {
			return writeFloodgateKey(filepath.Join(dir, "main"), make([]byte, floodgateKeyLen))
		}},
		{"eula", "main/eula.txt", func(dir string) error {
			return writeEULA(dir, "main")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, filepath.FromSlash(c.pipe))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(p, 0o644); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			done := make(chan error, 1)
			go func() { done <- c.run(dir) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("wrote into a named pipe as if it were a file")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("blocked on a named pipe")
			}
		})
	}
}

// A regular file at the same names is still written: the guard refuses pipes,
// not files.
func TestNonBlockingWriteStillWritesAFile(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(dir, "eula.txt"), []byte("eula=false, and longer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileIn(root, "eula.txt", []byte("eula=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "eula.txt")); string(b) != "eula=true\n" {
		t.Fatalf("file holds %q", b)
	}
}

var errRefusedForTest = os.ErrInvalid
