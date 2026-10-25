package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
	"github.com/redis/go-redis/v9"
)

// Extra ports a server publishes next to its game port in direct routing
// (voice chat, a map's web view). Core writes them per server into
// dylaris:server:<uuid>:ports; the node only reads. The JSON shape is a
// FROZEN contract with core/services/server_ports.go (NodePortEntry): fields
// may be added, never renamed.

// extraPortsLabel names, on the container, the host-side bindings that are
// extra ports ("udp:26001,tcp:26002"). The game binding is every other one:
// without the label, code that took "the first binding" as the game port
// would adopt an extra port into the port ledger.
const extraPortsLabel = "dylaris.extra_ports"

// maxExtraPorts bounds what one key can make the node bind.
const maxExtraPorts = 50

type extraPort struct {
	Protocol      string `json:"p"`
	HostPort      int    `json:"h"`
	ContainerPort int    `json:"c"`
}

type extraPortsDoc struct {
	V     int         `json:"v"`
	Ports []extraPort `json:"ports"`
}

// parseExtraPorts reads the key's value and keeps the entries it can bind:
// a known version and protocol, ports in range, not the game port, not the
// node's own ports, no duplicates. Everything else is dropped and logged,
// never fatal: an extra port must not keep a server down.
func parseExtraPorts(raw []byte, gameHostPort int) []extraPort {
	var doc extraPortsDoc
	if err := json.Unmarshal(raw, &doc); err != nil || doc.V != 1 {
		return nil
	}
	seen := map[string]bool{}
	out := []extraPort{}
	for _, p := range doc.Ports {
		p.Protocol = strings.ToLower(p.Protocol)
		key := p.Protocol + ":" + strconv.Itoa(p.HostPort)
		switch {
		case p.Protocol != "tcp" && p.Protocol != "udp",
			p.HostPort < 1024 || p.HostPort > 65535,
			p.ContainerPort < 1 || p.ContainerPort > 65535,
			hostPortProblem(p.HostPort) != "",
			p.Protocol == "tcp" && p.HostPort == gameHostPort,
			seen[key]:
			log.Printf("extra ports: skipping %+v", p)
			continue
		}
		if len(out) == maxExtraPorts {
			log.Printf("extra ports: more than %d, the rest are skipped", maxExtraPorts)
			break
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

// readExtraPorts fetches uuid's extra ports; nil when none, or when Redis
// cannot say (the container then starts with its game port alone).
func readExtraPorts(rdb *redis.Client, uuid string, gameHostPort int) []extraPort {
	if rdb == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := rdb.Get(ctx, "dylaris:server:"+uuid+":ports").Bytes()
	if err != nil {
		if err != redis.Nil {
			log.Printf("extra ports: server %s: %v", uuid, err)
		}
		return nil
	}
	return parseExtraPorts(raw, gameHostPort)
}

// addExtraPorts adds the bindings to pm and exposed, and returns the label
// value naming them. A container port already published (the game port, or
// an earlier entry) gets the host port appended to its bindings.
func addExtraPorts(pm nat.PortMap, exposed nat.PortSet, ports []extraPort) string {
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		k := nat.Port(fmt.Sprintf("%d/%s", p.ContainerPort, p.Protocol))
		pm[k] = append(pm[k], nat.PortBinding{HostIP: "0.0.0.0", HostPort: strconv.Itoa(p.HostPort)})
		exposed[k] = struct{}{}
		names = append(names, p.Protocol+":"+strconv.Itoa(p.HostPort))
	}
	return strings.Join(names, ",")
}

// isExtraBinding reports whether the binding of container port k on hostPort
// is one of the label's extra ports.
func isExtraBinding(label string, k nat.Port, hostPort string) bool {
	if label == "" {
		return false
	}
	want := k.Proto() + ":" + hostPort
	for _, e := range strings.Split(label, ",") {
		if e == want {
			return true
		}
	}
	return false
}

// containerLabel is a label of an inspected container, "" when absent.
func containerLabel(info container.InspectResponse, key string) string {
	if info.Config == nil {
		return ""
	}
	return info.Config.Labels[key]
}
