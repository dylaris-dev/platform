package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// The three places a server's real-IP switch lives, in the order they are
// written. A caller tells them apart to say which one did not take.
var (
	ErrProxyProtocolStore   = errors.New("proxy protocol: store")
	ErrProxyProtocolNodeKey = errors.New("proxy protocol: node key")
	ErrProxyProtocolHub     = errors.New("proxy protocol: hub queue")
)

type proxyProtocolStore interface {
	SetServerProxyProtocol(id int, enabled bool) error
}

type proxyProtocolHub interface {
	SetServerProxyProtocol(serverUUID string, enabled bool) error
}

// ApplyServerProxyProtocol switches real player IP forwarding for one server:
// the column (the truth), the node's copy for its status ping, and the hub
// message that flips the server's routes. It is the ONE write path, shared by
// the owner's switch and the default a new server gets, so the two can never
// disagree about what "on" means. A failure part way leaves the earlier writes
// in place; writing the same value again repairs all three.
func ApplyServerProxyProtocol(ctx context.Context, st proxyProtocolStore, rdb *redis.Client, hub proxyProtocolHub, serverID int, serverUUID string, enabled bool) error {
	if err := st.SetServerProxyProtocol(serverID, enabled); err != nil {
		return fmt.Errorf("%w: %w", ErrProxyProtocolStore, err)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	key := ServerProxyProtocolKey(serverUUID)
	var err error
	if enabled {
		err = rdb.Set(ctx, key, "1", 0).Err()
	} else {
		// The sub-servers known to expect the header go with it: whatever
		// happens to their files while it is off is the owner's business, and
		// turning it on again vouches only for the active one.
		err = rdb.Del(ctx, key, ProxyProtocolSubsKey(serverUUID)).Err()
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrProxyProtocolNodeKey, err)
	}
	if err := hub.SetServerProxyProtocol(serverUUID, enabled); err != nil {
		return fmt.Errorf("%w: %w", ErrProxyProtocolHub, err)
	}
	return nil
}

// ProxyProtocolDefaultInput is everything that decides whether a setup turns
// real player IP forwarding on by itself (owner decision 2026-10-21: new
// servers default to on, existing ones are never touched).
type ProxyProtocolDefaultInput struct {
	// NewServer: the server has never been set up. Core reads it as status
	// pending_setup AND no installer type recorded (every setup writes one,
	// nothing clears it) AND no sub-server install on record AND no gateway
	// route. pending_setup alone is not enough: deleting the active sub-server
	// puts an existing server back into it.
	NewServer bool
	// RouteRequested: the panel creates a gateway route right after this
	// setup. Without one the server is likely a backend for a proxy, which
	// dials it without the header; the owner's own switch refuses a server
	// without routes for the same reason. An older panel never says so.
	RouteRequested bool
	// InstallerType and Version are the installer as the node receives it.
	InstallerType string
	Version       string
	// BehindProxy: linked to a proxy on this platform. That proxy dials the
	// server directly, without a PROXY header, and would be refused.
	BehindProxy bool
	// RoutingMode is the platform's routing_mode. Only "gateway": in "both" the
	// server also takes players straight on its port, without the header.
	RoutingMode string
	// PlatformEnabled is gateway.proxy_protocol_enabled.
	PlatformEnabled bool
	// LinkClientAddr: the link that serves the server's node announced
	// client-addr, so it writes the header the server will require.
	LinkClientAddr bool
	// NodeWritesConfig: the node runs a release that writes the server's
	// config and reports back. An older node ignores the request, and the
	// switch would be on for a server that does not expect the header.
	NodeWritesConfig bool
}

// ProxyProtocolDefaultOn reports whether a setup turns forwarding on.
func ProxyProtocolDefaultOn(in ProxyProtocolDefaultInput) bool {
	return in.NewServer &&
		in.RouteRequested &&
		!in.BehindProxy &&
		in.RoutingMode == "gateway" &&
		in.PlatformEnabled &&
		in.LinkClientAddr &&
		in.NodeWritesConfig &&
		ProxyProtocolDefaultSoftware(in.InstallerType, in.Version)
}

// ProxyProtocolDefaultSoftware reports whether the node can configure this
// installer to expect a PROXY header ahead of its first start. Everything else
// (Vanilla, mod loaders, uploads, imports, modpacks) has no setting the node
// can write, or none every version honours, and stays opt-in.
//
//   - Paper reads config/paper-global.yml from 1.19 on; before that it was
//     paper.yml, which the node does not write.
//   - BungeeCord gained the listener's proxy_protocol in Jenkins build 1248
//     (2017-07-01).
//   - Waterfall inherits it from BungeeCord; 1.13 is the first line that
//     certainly carries it.
//   - Velocity is checked on the node, against the default config inside the
//     jar it just downloaded.
func ProxyProtocolDefaultSoftware(installerType, version string) bool {
	switch installerType {
	case "paper":
		return mcVersionAtLeast(version, 1, 19)
	case "waterfall":
		return mcVersionAtLeast(version, 1, 13)
	case "velocity":
		return true
	case "bungeecord":
		m := bungeeBuildDigits.FindStringSubmatch(strings.TrimSpace(version))
		if m == nil {
			return false
		}
		n, err := strconv.Atoi(m[1])
		return err == nil && n >= 1248
	}
	return false
}

var bungeeBuildDigits = regexp.MustCompile(`^#?([0-9]{1,9})$`)

// mcVersionAtLeast compares the first two numbers of a Minecraft version:
// "1.21.4" and "1.19" by their minor, "26.1" (the year-based scheme) by its
// major. Anything that does not start with two numbers is false.
func mcVersionAtLeast(v string, major, minor int) bool {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	if len(parts) < 2 {
		return false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minStr := parts[1]
	if i := strings.IndexFunc(minStr, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		minStr = minStr[:i]
	}
	mnr, err := strconv.Atoi(minStr)
	if err != nil {
		return false
	}
	return maj > major || (maj == major && mnr >= minor)
}
