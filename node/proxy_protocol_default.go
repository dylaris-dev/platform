package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
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
	"time"

	"github.com/redis/go-redis/v9"

	"dylaris-pkg/queue"
)

// Real player IP (PROXY protocol) set up by the node. Core switches a NEW
// Paper, Velocity, Waterfall or BungeeCord server's forwarding on before it
// sends the setup (owner decision 2026-10-21), and sends the same request with
// any later install on a server whose switch is on. The server's own software
// has to expect the PROXY header, or nobody can join - so the node makes the
// installed sub-server expect it before it starts and answers whether it does.
// Core switches the server off on anything but a yes.
//
// "Expects it" is the answer, not "wrote it": a config already there that
// expects the header (a rerun of the same install, a reinstall that kept the
// config) is a yes. A config there that does not is never edited - it is the
// operator's - and is a no. The one file the node edits is the bare listener
// it wrote itself moments earlier in the same install.
//
// It runs as the LAST step of an install that otherwise succeeded, so no later
// failure can leave the setting on disk behind a switch Core turns back off.
//
// Why writing ahead of the first start is safe, per software (sources in the
// 2026-10-21 report):
//   - Paper loads an existing config/paper-global.yml, takes a missing _version
//     as "latest" (so no migration runs), fills every missing key with its
//     default, keeps proxies.proxy-protocol and saves the whole file back.
//   - BungeeCord and Waterfall fill every missing listener key and keep the
//     ones present (YamlConfig.get), the same as the listener host the node
//     already writes (writeBungeeListener); the key joins that listener.
//   - Velocity copies its default config only when velocity.toml is missing
//     and runs every migration on a file without config-version, so a partial
//     file would end up as a sparse, comment-less config. The node writes the
//     jar's own default-velocity.toml instead - what Velocity would have
//     written - with the one value changed.

// paperProxyProtocolYAML is the whole file the node writes for Paper. No
// _version: Paper stamps its own, and an older Paper refuses a newer number.
const paperProxyProtocolYAML = "proxies:\n  proxy-protocol: true\n"

// bungeeProxyProtocolLine continues the one listener writeBungeeListener writes.
const bungeeProxyProtocolLine = "  proxy_protocol: true\n"

// bungeeListenerOnly is exactly the file writeBungeeListener writes, and
// nothing else: the one config.yml the node may add to.
var bungeeListenerOnly = regexp.MustCompile(`\Alisteners:\n- host: 0\.0\.0\.0:[0-9]{1,5}\n\z`)

// proxyConfigReadCap bounds what is read back: every file here is a few KB.
const proxyConfigReadCap = 1 << 20

// proxyResultTTL keeps the durable answer for Core's sweep (see
// services.ProxyProtocolResultKey) long enough to survive a Core outage.
const proxyResultTTL = 24 * time.Hour

// writeProxyProtocolConfig makes the sub-server expect the PROXY header and
// reports whether it now does. A false is always safe to act on: Core turns
// the switch off, which is what a server without the setting expects.
func writeProxyProtocolConfig(serverPath, subName string, cfg InstallerConfig) bool {
	root, err := os.OpenRoot(serverPath)
	if err != nil {
		log.Printf("proxy-protocol: %s: %v", subName, err)
		return false
	}
	defer root.Close()
	// Opened through the server directory: the sub-server directory is the
	// tenant's, and a link in its place must not carry the write elsewhere.
	sub, err := root.OpenRoot(filepath.ToSlash(subName))
	if err != nil {
		log.Printf("proxy-protocol: %s: %v", subName, err)
		return false
	}
	defer sub.Close()

	var ok bool
	switch cfg.Type {
	case "paper":
		ok, err = writePaperProxyProtocol(sub)
	case "velocity":
		ok, err = writeVelocityProxyProtocol(sub)
	case "waterfall", "bungeecord":
		ok, err = addBungeeProxyProtocol(sub)
	default:
		err = fmt.Errorf("no real-IP setting for installer %q", cfg.Type)
	}
	if err != nil {
		log.Printf("proxy-protocol: %s (%s): %v", subName, cfg.Type, err)
		return false
	}
	if !ok {
		log.Printf("proxy-protocol: %s (%s): the config there does not expect the header; left alone", subName, cfg.Type)
	}
	return ok
}

// readSmall reads name inside root, bounded. A missing file is fs.ErrNotExist.
func readSmall(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, proxyConfigReadCap))
}

// createExclusive writes data to name only if nothing is there. exists is
// true, with no error, when something was.
func createExclusive(root *os.Root, name string, data []byte) (exists bool, err error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		// Half a config is worse than none: the next start would read it.
		_ = root.Remove(name)
		return false, werr
	}
	chownForMCIn(root, name)
	return false, nil
}

func writePaperProxyProtocol(sub *os.Root) (bool, error) {
	if err := sub.Mkdir("config", 0o755); err == nil {
		chownForMCIn(sub, "config")
	} else if !errors.Is(err, fs.ErrExist) {
		return false, err
	}
	name := path.Join("config", "paper-global.yml")
	exists, err := createExclusive(sub, name, []byte(paperProxyProtocolYAML))
	if err != nil || !exists {
		return err == nil, err
	}
	b, err := readSmall(sub, name)
	if err != nil {
		return false, err
	}
	return paperExpectsProxy(b), nil
}

// paperExpectsProxy reports a paper-global.yml whose top-level proxies block
// sets proxy-protocol: true - the node's own file, or the full one Paper
// saved from it.
func paperExpectsProxy(b []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(b))
	in := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \r")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			in = line == "proxies:"
			continue
		}
		if in && line == "  proxy-protocol: true" {
			return true
		}
	}
	return false
}

// addBungeeProxyProtocol adds proxy_protocol to the listener in config.yml
// when the file is exactly what writeBungeeListener wrote during this install.
// Any other config.yml is the operator's (or BungeeCord's own, saved from
// ours) and is only read: a yes when every listener already expects it.
func addBungeeProxyProtocol(sub *os.Root) (bool, error) {
	f, err := sub.OpenFile("config.yml", os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	b, err := io.ReadAll(io.LimitReader(f, proxyConfigReadCap))
	if err != nil || !bungeeListenerOnly.Match(b) {
		f.Close()
		return err == nil && bungeeExpectsProxy(b), err
	}
	_, werr := f.WriteAt([]byte(bungeeProxyProtocolLine), int64(len(b)))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		// Back to the listener alone: half a key is worse than none.
		if t, terr := sub.OpenFile("config.yml", os.O_WRONLY|os.O_TRUNC, 0); terr == nil {
			t.Write(b)
			t.Close()
		}
		return false, werr
	}
	return true, nil
}

// bungeeExpectsProxy reports a config.yml whose listeners ALL set
// proxy_protocol: true: one listener without it would refuse the gateway's
// header on its port. A listener is counted by its host key.
func bungeeExpectsProxy(b []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(b))
	in := false
	hosts, on := 0, 0
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \r")
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
			in = line == "listeners:"
			continue
		}
		if !in {
			continue
		}
		key := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		if strings.HasPrefix(key, "host:") {
			hosts++
		} else if key == "proxy_protocol: true" {
			on++
		}
	}
	return hosts > 0 && hosts == on
}

func writeVelocityProxyProtocol(sub *os.Root) (bool, error) {
	if b, err := readSmall(sub, "velocity.toml"); err == nil {
		_, val, ok := velocityHaproxy(b)
		return ok && val == "true", nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	def, err := zipEntryIn(sub, "server.jar", "default-velocity.toml")
	if err != nil {
		return false, err
	}
	conf, ok := velocityConfigWithProxyProtocol(def)
	if !ok {
		return false, errors.New("the jar's default-velocity.toml has no single [advanced] haproxy-protocol = false")
	}
	// Velocity creates forwarding.secret on its first start only when it also
	// creates velocity.toml, so the node does both. Any random string serves.
	if _, err := createExclusive(sub, "forwarding.secret", []byte(rand.Text())); err != nil {
		return false, err
	}
	exists, err := createExclusive(sub, "velocity.toml", conf)
	return err == nil && !exists, err
}

// zipEntryIn reads one entry of the zip archive name inside root.
func zipEntryIn(root *os.Root, name, entry string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	for _, zf := range zr.File {
		if zf.Name != entry {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, proxyConfigReadCap))
	}
	return nil, fmt.Errorf("%s has no %s", name, entry)
}

// velocityHaproxy finds THE haproxy-protocol key of a velocity.toml: exactly
// one, inside [advanced]. at is its line, val its trimmed value. Anything
// else - none, two, one elsewhere - is not a layout to guess at.
func velocityHaproxy(conf []byte) (at int, val string, ok bool) {
	lines := strings.Split(string(conf), "\n")
	section := ""
	at = -1
	for i, line := range lines {
		t := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(t, "[") {
			section = t
			continue
		}
		key, v, found := strings.Cut(t, "=")
		if !found || strings.TrimSpace(key) != "haproxy-protocol" {
			continue
		}
		if at >= 0 || section != "[advanced]" {
			return -1, "", false
		}
		at, val = i, strings.TrimSpace(v)
	}
	return at, val, at >= 0
}

// velocityConfigWithProxyProtocol turns Velocity's own default config into
// one that expects the PROXY header: the single [advanced] haproxy-protocol,
// which every Velocity 3 default has set to false, becomes true.
func velocityConfigWithProxyProtocol(def []byte) ([]byte, bool) {
	at, val, ok := velocityHaproxy(def)
	if !ok || val != "false" {
		return nil, false
	}
	lines := strings.Split(string(def), "\n")
	lines[at] = strings.Replace(lines[at], "false", "true", 1)
	return []byte(strings.Join(lines, "\n")), true
}

// reportProxyProtocol answers an install that carried the real-IP request,
// twice: durably in the result key Core's sweep reads, and immediately on the
// node's own setup channel (see reportSetup). Detached from ctx: the answer
// matters most when the node is being stopped (a watchtower update) right
// after an install.
func reportProxyProtocol(ctx context.Context, rdb *redis.Client, serverUUID, subServer string, expects bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	val := "0"
	if expects {
		val = "1"
	}
	if err := rdb.Set(ctx, "dylaris:server:"+serverUUID+":proxy_protocol_result", val, proxyResultTTL).Err(); err != nil {
		log.Printf("proxy-protocol report: result key: %v", err)
	}
	data, err := json.Marshal(map[string]interface{}{
		"serverUuid":    serverUUID,
		"subServer":     subServer,
		"proxyProtocol": expects,
		"timestamp":     time.Now().Unix(),
	})
	if err != nil {
		log.Printf("proxy-protocol report: marshal failed: %v", err)
		return
	}
	if err := rdb.Publish(ctx, queue.SetupResultsChannel(nodeID), data).Err(); err != nil {
		log.Printf("proxy-protocol report publish failed: %v", err)
	}
}

// proxyProtocolReporter answers an install's real-IP request exactly once.
// The deferred call answers no on every way out that never reached the write
// - failed install, EULA, start command, a server deleted meanwhile - so Core
// can switch the server back off; the write's own answer comes first.
// Without the request it does nothing.
func proxyProtocolReporter(ctx context.Context, rdb *redis.Client, cfg InstallerConfig, serverUUID, subServer string) func(expects bool) {
	done := false
	return func(expects bool) {
		if !cfg.ProxyProtocol || done {
			return
		}
		done = true
		reportProxyProtocol(ctx, rdb, serverUUID, subServer, expects)
	}
}
