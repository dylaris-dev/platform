package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// Minecraft proxies: Velocity and Waterfall come from the PaperMC Fill API
// (installPaperMC), BungeeCord from its Jenkins. All three land as server.jar,
// the name resolveLaunch already starts.

// bungeeCordBuildPattern is a Jenkins build number. Core's version list sends
// it as "#2104", the way Jenkins writes it.
var bungeeCordBuildPattern = regexp.MustCompile(`^#?([0-9]{1,9})$`)

// bungeeCordBuild returns the build number in version, or an error for
// anything else: it becomes part of the download URL.
func bungeeCordBuild(version string) (string, error) {
	m := bungeeCordBuildPattern.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return "", fmt.Errorf("BungeeCord version must be a Jenkins build number such as #2104, got %q", version)
	}
	return m[1], nil
}

// installBungeeCord downloads one BungeeCord build. hub.spigotmc.org is where
// ci.md-5.net redirects to; asking it directly saves the hop. Jenkins publishes
// no sha256 for the artifact, so there is nothing to verify it against.
func installBungeeCord(dir, version string) error {
	build, err := bungeeCordBuild(version)
	if err != nil {
		return err
	}
	u := fmt.Sprintf("https://hub.spigotmc.org/jenkins/job/BungeeCord/%s/artifact/bootstrap/target/BungeeCord.jar", build)
	log.Printf("Downloading BungeeCord build %s ...", build)
	return installServerJar(dir, u, "")
}

// writeBungeeListener gives a new BungeeCord or Waterfall proxy a config.yml
// that listens on the container's port. Their default is 0.0.0.0:25577, while
// the container publishes - and the stats ping and the gateway dial - the
// node's container port, so a proxy left on its default would run and be
// unreachable. Velocity needs none of this: its default bind is 25565, and a
// velocity.toml written ahead of its first start would also stop it from
// generating its forwarding.secret.
//
// Only the listener's host is written. BungeeCord fills every missing key of a
// listener with its default on start and saves the file (YamlConfig.get).
//
// An existing config.yml is the operator's and is never touched: a reinstall
// to a newer build must not reset their listeners. O_EXCL makes that the
// create itself rather than a check before it, and refuses a planted link.
func writeBungeeListener(dir string, port int) error {
	if port <= 0 {
		port = getContainerPort()
	}
	if port <= 0 {
		port = 25565 // the node's own default, before its settings loaded
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile("config.yml", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("writing config.yml: %w", err)
	}
	_, werr := fmt.Fprintf(f, "listeners:\n- host: 0.0.0.0:%d\n", port)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("writing config.yml: %w", werr)
	}
	chownForMCIn(root, "config.yml")
	return nil
}

// bungeeConfigReadCap bounds how much of a tenant's config.yml is read to find
// one key near its top. BungeeCord writes listeners within the first few KB.
const bungeeConfigReadCap = 1 << 20

// consoleStopCommand is the console command that shuts the server in
// sub/ down cleanly. BungeeCord and Waterfall only know "end" and answer
// "stop" with "Command not found", which left them running until Docker
// killed them. Velocity accepts "stop" as an alias of "shutdown".
//
// They are told apart by a config.yml with a top-level "listeners:" key, which
// BungeeCord and Waterfall always write. A Paper, Spigot or modded server keeps
// its settings in server.properties, bukkit.yml, spigot.yml, commands.yml and
// a config/ DIRECTORY, and plugins keep theirs under plugins/<name>/, so none
// of them has that file. Read through a Root on the server directory: the
// sub-server directory is the tenant's to replace with a link.
func consoleStopCommand(serverDir, sub string) string {
	if sub == "" {
		return "stop"
	}
	root, err := os.OpenRoot(serverDir)
	if err != nil {
		return "stop"
	}
	defer root.Close()
	f, err := root.Open(path.Join(filepath.ToSlash(sub), "config.yml"))
	if err != nil {
		return "stop"
	}
	defer f.Close()
	if hasTopLevelListeners(io.LimitReader(f, bungeeConfigReadCap)) {
		return "end"
	}
	return "stop"
}

// hasTopLevelListeners reports a YAML document with "listeners" as a key at
// column 0. Indented occurrences belong to some other mapping.
func hasTopLevelListeners(r io.Reader) bool {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 64*1024)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "listeners:") {
			return true
		}
	}
	return false
}
