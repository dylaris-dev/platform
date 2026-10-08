package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// MC_RUN_AS is the escape hatch, and every branch of it decides who owns a
// tenant's world - so a typo must not silently turn the hardening off.
func TestMCUser(t *testing.T) {
	cases := []struct {
		name, env string
		wantUID   int
		wantSpec  string
	}{
		{name: "unset", env: "", wantUID: 1000, wantSpec: "1000:1000"},
		{name: "another uid", env: "1500", wantUID: 1500, wantSpec: "1500:1500"},
		// 0 is a deliberate opt-out, not a mistake: an operator with data owned
		// by root who would rather keep it than have every world rewritten.
		{name: "explicit root opt-out", env: "0", wantUID: 0, wantSpec: ""},
		// Garbage falls back to the safe default rather than to root. A typo in
		// an env var must not be the thing that hands a tenant uid 0.
		{name: "not a number", env: "dylaris", wantUID: 1000, wantSpec: "1000:1000"},
		{name: "negative", env: "-1", wantUID: 1000, wantSpec: "1000:1000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MC_RUN_AS", tc.env)
			if got := mcUser(); got != tc.wantUID {
				t.Errorf("mcUser() = %d, want %d", got, tc.wantUID)
			}
			if got := mcUserSpec(); got != tc.wantSpec {
				t.Errorf("mcUserSpec() = %q, want %q", got, tc.wantSpec)
			}
		})
	}
}

// The ownership pass must not touch anything above the sub-server directory.
//
// That boundary IS the feature: .active_server and .dylaris-backups sit in the
// same bind mount the tenant's container gets, and they stay root's precisely so
// a plugin cannot rewrite which sub-server runs or plant a symlink the backup
// download RPC would follow.
func TestOwnershipStopsAtTheSubServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file ownership is a uid concept; the node only runs on Linux")
	}
	if os.Geteuid() != 0 {
		t.Skip("chown needs root; asserted in CI's container and by the live test")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "survival")
	if err := os.MkdirAll(filepath.Join(sub, "world"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, ".active_server")
	if err := os.WriteFile(marker, []byte("survival"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("MC_RUN_AS", "1000")
	if err := ensureSubServerOwnership(sub); err != nil {
		t.Fatalf("ensureSubServerOwnership: %v", err)
	}

	fi, err := os.Lstat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if !ownedBy(fi, 1000) {
		t.Error("the sub-server directory was not handed to the container's uid")
	}
	mfi, err := os.Lstat(marker)
	if err != nil {
		t.Fatal(err)
	}
	if ownedBy(mfi, 1000) {
		t.Error(".active_server was handed to the tenant; it must stay root's")
	}
}

// A missing sub-server directory is the normal state of a server that has not
// been installed yet. It must not be an error on the way into a container start.
func TestOwnershipOfSomethingNotInstalledYet(t *testing.T) {
	t.Setenv("MC_RUN_AS", "1000")
	if err := ensureSubServerOwnership(filepath.Join(t.TempDir(), "nothing-here")); err != nil {
		t.Errorf("a server with nothing installed reported an error: %v", err)
	}
}

// Both container-build sites must set the user, and there is no way for a test
// to drive them without a Docker daemon - so this reads the source, the same way
// the panel's build wiring is asserted.
//
// The failure it guards against is silent: forget it at one site and that path
// creates a root container, which works perfectly until a tenant plugin decides
// to write /data/.active_server.
func TestEveryContainerBuildSiteSetsTheUser(t *testing.T) {
	b, err := os.ReadFile("docker_mgr.go")
	if err != nil {
		t.Fatalf("read docker_mgr.go: %v", err)
	}
	src := string(b)

	// Count the MC container configs by their image field, which every one of
	// them sets, and require a User beside each.
	configs := strings.Count(src, "Image:      config.Docker.Image")
	users := strings.Count(src, "User:       mcUserSpec()")
	if configs == 0 {
		t.Fatal("no MC container config found; this test is reading the wrong shape")
	}
	if users != configs {
		t.Errorf("%d MC container configs but %d set User: a container built without it runs as root", configs, users)
	}

	if !strings.Contains(src, "ensureSubServerOwnership(") {
		t.Error("nothing hands the world to the container's uid at start; a non-root server cannot write it")
	}
}

// copyTree exists to MOVE a whole server to another storage path, protected
// entries included - .active_server, .node_config.json, .dylaris-backups - and
// it shares its walker with copyDir, which is the user-facing duplicate.
//
// So the ownership has to be applied on one path and not the other. Chowning in
// copyFile, the obvious place, would reach both: a storage move would hand the
// tenant's container the very files running non-root took away from it, and
// nothing would look wrong until a plugin rewrote .active_server.
func TestOnlyTheDuplicatingCopyHandsFilesOver(t *testing.T) {
	b, err := os.ReadFile("rootfs.go")
	if err != nil {
		t.Fatalf("read rootfs.go: %v", err)
	}
	src := string(b)

	body, ok := cutFunc(src, "func copyFileIn(src *os.Root, srcName string, dst *os.Root, dstName string, deny map[fileIdentity]bool) error {")
	if !ok {
		t.Fatal("copyFileIn is gone; move this assertion with it")
	}
	if strings.Contains(body, "chownForMC") {
		t.Error("copyFileIn chowns, so a verbatim MOVE does too - it would hand " +
			".active_server and .dylaris-backups to the tenant's uid")
	}

	walk, ok := cutFunc(src, "func copyWalkIn(src *os.Root, srcName string, dst *os.Root, dstName string, forTenant bool) error {")
	if !ok {
		t.Fatal("copyWalkIn is gone")
	}
	if !strings.Contains(walk, "chownForMCIn") {
		t.Error("copyWalkIn never hands a duplicated file to the container's uid")
	}
	if !strings.Contains(walk, "if forTenant {") {
		t.Error("copyWalkIn chowns unconditionally; the verbatim MOVE must be excluded")
	}
}

// Every path that STARTS a Minecraft container must run the ownership pass, not
// just the one that creates it.
//
// PowerAction("start") starts an existing container, and its only caller is the
// reconciler's crash-restart loop - which is precisely where the repair matters
// most, because a file the container may not write is one of the things that
// makes it crash in the first place. Skipping it there turns a one-off
// permission problem into five restarts into the same error and then a server
// that stays down.
func TestEveryStartPathRepairsOwnership(t *testing.T) {
	b, err := os.ReadFile("docker_mgr.go")
	if err != nil {
		t.Fatalf("read docker_mgr.go: %v", err)
	}
	src := string(b)

	for _, header := range []string{
		"func (dm *DockerManager) startMinecraftContainer(config ServerConfig, netID, netName string, autoStart bool) (string, error) {",
		"func (dm *DockerManager) PowerAction(uuid string, action string) error {",
	} {
		body, ok := cutFunc(src, header)
		if !ok {
			t.Errorf("%s is gone; move this assertion with it", header)
			continue
		}
		if !strings.Contains(body, "ensureSubServerOwnership") {
			t.Errorf("%s starts a container without repairing ownership first", header)
		}
	}
}

// cutFunc returns a function's source, from its header to the next one.
func cutFunc(src, header string) (string, bool) {
	i := strings.Index(src, header)
	if i < 0 {
		return "", false
	}
	body := src[i+len(header):]
	if j := strings.Index(body, "\nfunc "); j > 0 {
		body = body[:j]
	}
	return body, true
}

// An install writes as root into a directory that may already be the
// container's. The start-time repair reads only that directory and skipped the
// walk: an uploaded server's cache/, world/ and plugins/ stayed root's and
// Paper failed every start with AccessDeniedException.
func TestInstallHandsWhatItWroteToTheContainer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file ownership is a uid concept; the node only runs on Linux")
	}
	if os.Geteuid() != 0 {
		t.Skip("chown needs root; asserted in CI's container and by the live test")
	}
	t.Setenv("MC_RUN_AS", "1000")
	data := t.TempDir()
	sub := filepath.Join(data, "main")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Lchown(sub, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	writeUploadZip(t, sub, map[string]string{"server.jar": "x", "cache/mojang_1.21.11.jar": "y"})
	if _, err := InstallServer(data, "main", InstallerConfig{Type: "upload-zip", Structure: "direct"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"cache", "cache/mojang_1.21.11.jar", "server.jar"} {
		fi, err := os.Lstat(filepath.Join(sub, p))
		if err != nil {
			t.Fatal(err)
		}
		if !ownedBy(fi, 1000) {
			t.Errorf("%s is not the container's after the install", p)
		}
	}
}

// The same, where no root is available: every install goes through the
// hand-over, not only the start path.
func TestInstallServerHandsTheTreeOver(t *testing.T) {
	b, err := os.ReadFile("installer.go")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := cutFunc(string(b), "func InstallServer(serverDataPath, subServerName string, config InstallerConfig) ([]byte, error) {")
	if !ok {
		t.Fatal("InstallServer is gone; move this assertion with it")
	}
	if !strings.Contains(body, "handInstalledTree(") {
		t.Error("InstallServer returns without handing what it wrote to the container's uid")
	}
}
