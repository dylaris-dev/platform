package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
)

// seedJail builds <base>/srv with a regular file and a directory, plus a file
// OUTSIDE srv that nothing done through srv may reach.
func seedJail(t *testing.T) (srv, outside string) {
	t.Helper()
	base := t.TempDir()
	srv = filepath.Join(base, "srv")
	if err := os.MkdirAll(filepath.Join(srv, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"a": "inside", "d/f": "inside"} {
		if err := os.WriteFile(filepath.Join(srv, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	outside = filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("OUTSIDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	return srv, outside
}

// requireSymlinks skips on a platform (Windows CI host) where an unprivileged
// process cannot create symlinks; the gate that matters runs on Linux.
func requireSymlinks(t *testing.T, dir string) {
	t.Helper()
	probe := filepath.Join(dir, ".symlink-probe")
	if err := os.Symlink("target", probe); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		t.Fatal(err)
	}
	os.Remove(probe)
}

// A link that leaves the jail is refused when opened through the Root, whether
// it names a file or a directory, and whether it is absolute or relative. The
// lexical check (resolveWithinDir) never sees it: the string names something
// inside, and only following the link on the filesystem reveals the escape.
func TestOpenJailedRefusesEscapingLink(t *testing.T) {
	srv, outside := seedJail(t)
	requireSymlinks(t, srv)
	cases := map[string]string{
		"absolute file link": outside + "/f",
		"relative file link": "../outside/f",
		"absolute dir link":  outside,
		"relative dir link":  "../outside",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			link := filepath.Join(srv, "evil")
			os.Remove(link)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			root, jailed, err := openJailed(srv, "evil")
			if err != nil {
				return // refused already at resolveWithinDir; also fine
			}
			defer root.Close()
			if _, err := root.Open(jailed); err == nil {
				t.Fatalf("opened an escaping link through the Root")
			}
			if _, err := root.Stat(jailed); err == nil {
				// Stat following the link out is the read leak.
				if b, rerr := root.ReadFile(jailed); rerr == nil && string(b) == "OUTSIDE" {
					t.Fatalf("read a file outside the jail through the Root")
				}
			}
		})
	}
}

// An absolute link is refused even when it points back INSIDE the jail: a Root
// treats every absolute link as an escape, and a link written from inside the
// tenant's container is absolute in the container's view. A relative link that
// stays inside is followed, so legitimate layouts keep working.
func TestOpenJailedLinkPolicy(t *testing.T) {
	srv, _ := seedJail(t)
	requireSymlinks(t, srv)
	if err := os.Symlink("a", filepath.Join(srv, "rel-inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(srv, "a"), filepath.Join(srv, "abs-inside")); err != nil {
		t.Fatal(err)
	}
	root, _, err := openJailed(srv, ".")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if b, err := root.ReadFile("rel-inside"); err != nil || string(b) != "inside" {
		t.Errorf("contained relative link not followed: %q, %v", b, err)
	}
	if _, err := root.Open("abs-inside"); err == nil {
		t.Error("absolute link followed; a Root must refuse it")
	}
}

// The whole reason for the Root: with the old check-then-open, a name flipped
// between a regular file and an escaping link in the gap read the file OUTSIDE
// the jail. Measured on the old code, this loop read it 636 times in ~0.5s.
// Through the Root the count must be zero, however the race falls.
func TestNoReadEscapesUnderRace(t *testing.T) {
	srv, outside := seedJail(t)
	requireSymlinks(t, srv)
	if err := os.WriteFile(filepath.Join(srv, "real"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(srv, "a")
	canary := filepath.Join(outside, "f")

	var stop atomic.Bool
	go func() {
		for !stop.Load() {
			os.Rename(filepath.Join(srv, "real"), target)
			_ = os.Symlink(canary, filepath.Join(srv, "tmp"))
			os.Rename(filepath.Join(srv, "tmp"), target)
		}
	}()
	t.Cleanup(func() { stop.Store(true) })

	leaked := 0
	for range 20000 {
		root, name, err := openJailed(srv, "a")
		if err != nil {
			continue
		}
		b, err := root.ReadFile(name)
		root.Close()
		if err == nil && string(b) == "OUTSIDE" {
			leaked++
		}
	}
	if leaked != 0 {
		t.Fatalf("read a file outside the jail %d times through the Root", leaked)
	}
}
