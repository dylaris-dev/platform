package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/network"
)

// What each private address in the heartbeat is, so the panel can show the
// machine's LAN address and fold away the rest. A host runs one Docker bridge
// per network and a WireGuard interface for the overlay, and all of them are
// RFC1918, so the flat list read as eleven addresses for a machine with one.
//
// Decided from what the machine says about each interface, not from the
// address: Docker's own subnets, and the kernel's device type for WireGuard.
// A LAN in 172.16/12 is still a LAN.
const (
	ipKindLAN     = "lan"
	ipKindDocker  = "docker"
	ipKindOverlay = "overlay"
)

// ipKind classifies one private address on interface iface.
func ipKind(ip net.IP, iface string, wireguard bool, dockerNets []*net.IPNet) string {
	if wireguard || strings.HasPrefix(iface, warpInterfacePrefix) {
		return ipKindOverlay
	}
	for _, n := range dockerNets {
		if n.Contains(ip) {
			return ipKindDocker
		}
	}
	return ipKindLAN
}

// isWireGuard reads the kernel's device type. Run inside the host's network
// namespace (the kits run the node with network_mode: host), sysfs shows the
// host's devices.
func isWireGuard(sysNet, iface string) bool {
	b, err := os.ReadFile(filepath.Join(sysNet, iface, "uevent"))
	return err == nil && strings.Contains(string(b), "DEVTYPE=wireguard")
}

// classifyPrivateIPs maps each RFC1918 address on the given interfaces to its
// kind.
func classifyPrivateIPs(ifaces map[string][]net.IP, sysNet string, dockerNets []*net.IPNet) map[string]string {
	out := map[string]string{}
	for name, ips := range ifaces {
		wg := isWireGuard(sysNet, name)
		for _, ip := range ips {
			if isRFC1918(ip) {
				out[ip.To4().String()] = ipKind(ip, name, wg, dockerNets)
			}
		}
	}
	return out
}

var ipKindsCache struct {
	sync.Mutex
	at    time.Time
	kinds map[string]string
}

// privateIPKinds is classifyPrivateIPs for this machine, refreshed once a
// minute rather than on every 5-second heartbeat. nil until Docker has
// answered once: without its subnets every bridge would read as LAN, and
// saying nothing lets the panel show the plain list instead. A later failure
// keeps the last answer, so Core is not told "unknown" and back every minute.
func privateIPKinds(dm *DockerManager) map[string]string {
	if dm == nil || dm.cli == nil {
		return nil
	}
	ipKindsCache.Lock()
	defer ipKindsCache.Unlock()
	if time.Since(ipKindsCache.at) < time.Minute {
		return ipKindsCache.kinds
	}
	ipKindsCache.at = time.Now()
	nets, err := dm.cli.NetworkList(dm.ctx, network.ListOptions{})
	if err != nil {
		return ipKindsCache.kinds
	}
	var dockerNets []*net.IPNet
	for _, n := range nets {
		for _, c := range n.IPAM.Config {
			if _, ipn, err := net.ParseCIDR(c.Subnet); err == nil {
				dockerNets = append(dockerNets, ipn)
			}
		}
	}
	list, err := net.Interfaces()
	if err != nil {
		return ipKindsCache.kinds
	}
	ifaces := map[string][]net.IP{}
	for _, i := range list {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				ifaces[i.Name] = append(ifaces[i.Name], ipn.IP)
			}
		}
	}
	ipKindsCache.kinds = classifyPrivateIPs(ifaces, "/sys/class/net", dockerNets)
	return ipKindsCache.kinds
}
