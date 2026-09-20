package main

import "testing"

// setCoreMirrorHost is package state shared with every other test in this
// package, so each case puts it back.
func withCoreMirrorHost(t *testing.T, host string) {
	t.Helper()
	prev, _ := coreMirrorHost.Load().(string)
	coreMirrorHost.Store(host)
	t.Cleanup(func() { coreMirrorHost.Store(prev) })
}

func TestArchiveURLFromCoreIsAllowedOnlyOnceCoreNamedItself(t *testing.T) {
	const url = "https://panel.example.com/mirror/modpacks/abc/pack.mrpack"

	withCoreMirrorHost(t, "")
	if err := validateMrpackArchiveURL(url); err == nil {
		t.Fatal("a pack from an unnamed host was accepted; a node that was told nothing must refuse")
	}

	setCoreMirrorHost("Panel.Example.com")
	if err := validateMrpackArchiveURL(url); err != nil {
		t.Fatalf("the Core host this node logged in to was refused: %v", err)
	}
	// The host is compared, not the path: the same Core serves the route under
	// whatever prefix its public URL carries.
	if err := validateMrpackArchiveURL("https://panel.example.com/other/pack.mrpack"); err != nil {
		t.Fatalf("same host, different path was refused: %v", err)
	}
	if err := validateMrpackArchiveURL("https://other.example.com/mirror/modpacks/abc/pack.mrpack"); err == nil {
		t.Fatal("a host Core never named was accepted")
	}
	// No hash covers the archive, so plain HTTP stays refused even for the Core
	// the node is logged in to.
	if err := validateMrpackArchiveURL("http://panel.example.com/mirror/modpacks/abc/pack.mrpack"); err == nil {
		t.Fatal("an http pack download was accepted")
	}
}

// Empty means "Core names none". Two Cores answer this node and a reply without
// the field (an older Core, or a replica with no public URL configured) must not
// revoke what the other one told it.
func TestAnEmptyMirrorHostDoesNotForgetTheOneWeHave(t *testing.T) {
	withCoreMirrorHost(t, "")
	setCoreMirrorHost("panel.example.com")
	setCoreMirrorHost("")
	setCoreMirrorHost("   ")
	if got, _ := coreMirrorHost.Load().(string); got != "panel.example.com" {
		t.Fatalf("host after two empty answers = %q, want panel.example.com", got)
	}
}

// The archive is the ONE URL Core chooses. Everything inside the manifest is
// written by whoever built the pack, so the Core host buys it nothing.
func TestTheCoreHostDoesNotWidenTheManifestAllowlist(t *testing.T) {
	withCoreMirrorHost(t, "panel.example.com")
	if err := validateMrpackURL("https://panel.example.com/mods/anything.jar"); err == nil {
		t.Fatal("a manifest download from the Core host was accepted; only the archive URL may use it")
	}
}

// Every download from that CDN has needed a CurseForge API key since
// 2026-07-16, so the entries could only produce a 401 mid-install.
func TestForgeCDNIsNoLongerAllowed(t *testing.T) {
	for _, u := range []string{
		"https://edge.forgecdn.net/files/1/2/mod.jar",
		"https://mediafilez.forgecdn.net/files/1/2/mod.jar",
	} {
		if err := validateMrpackURL(u); err == nil {
			t.Errorf("%s was accepted; it needs a CurseForge key and cannot be downloaded", u)
		}
	}
}
