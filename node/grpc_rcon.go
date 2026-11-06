package main

// Node-side handler for the RCON-exec RPC. Core sends us the
// server UUID + command + password + (optionally) the RCON port; we open a
// TCP connection to the container's RCON socket and forward the reply.
//
// Address resolution: the MC container is a sibling of the node, and its IP is
// resolved straight from the Docker daemon (resolveMCContainerIP), not from
// Docker DNS. The daemon is authoritative, so this works even from a host-net
// node (whose resolver is the host's, not Docker's embedded 127.0.0.11) and it
// removes the DNS attack the previous net.LookupIP path had to defend against:
// when the name did not resolve locally, Docker's embedded resolver forwarded it
// upstream and a wildcard DNS record could answer with a PUBLIC address (observed
// live on a stopped server resolving to 46.225.53.182). execRcon's first write is
// SERVERDATA_AUTH carrying the RCON password in the clear, so an attacker-chosen
// host must never be a possible target. guardPrivateAddr keeps that property on
// the daemon-attested result as defense in depth: only a private/link-local
// address is ever dialled, never loopback (the node's own process) or a public
// address.

import (
	"fmt"
	"net"
	"time"

	pb "dylaris-proto/node"
)

// resolveMCContainerIP is set once at startup (main.go) from
// DockerManager.ResolveMCContainerIP. It returns mc_<uuid>'s private IP from the
// Docker daemon. A package var so the RCON and tab-proxy paths - both free
// functions today - share one daemon-backed resolver without threading the
// docker client through every constructor, and so tests can inject a fake.
var resolveMCContainerIP func(uuid string) (net.IP, error)

// resolveMCAddr resolves mc_<uuid> to a guarded "ip:port" for a control-plane
// dial (RCON, tab proxy). The address is only ever returned when it is a
// private/link-local IP.
func resolveMCAddr(uuid string, port int) (string, error) {
	if resolveMCContainerIP == nil {
		return "", fmt.Errorf("container IP resolver not initialised")
	}
	ip, err := resolveMCContainerIP(uuid)
	if err != nil {
		return "", err
	}
	return guardPrivateAddr(ip, port)
}

// guardPrivateAddr formats ip:port only for a private/link-local address. The MC
// container always sits on a Docker bridge/overlay network, so its address is
// always private; anything else (public, loopback, nil) means the value is not
// our container and must never receive RCON credentials.
func guardPrivateAddr(ip net.IP, port int) (string, error) {
	if ip == nil {
		return "", fmt.Errorf("no address for container (is the server running?); refusing to send RCON credentials")
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return net.JoinHostPort(ip.String(), fmt.Sprint(port)), nil
	}
	// Naming the address is deliberate: it tells an operator the value is not on
	// this node's network.
	return "", fmt.Errorf("container resolved to non-private address %v — it is not on this node's network; "+
		"refusing to send RCON credentials there", ip)
}

func (h *StreamHandler) handleRconExec(requestID, serverUUID string, req *pb.RconExecReq) *pb.NodeMessage {
	start := time.Now()
	if req == nil || req.Command == "" {
		return rconErrMsg(requestID, "empty command", start)
	}
	if req.RconPassword == "" {
		return rconErrMsg(requestID, "rcon password not configured for this server", start)
	}

	port := int(req.RconPort)
	if port == 0 {
		port = rconDefaultPortVar
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = rconDefaultTimeout
	}

	addr, err := resolveMCAddr(serverUUID, port)
	if err != nil {
		return rconErrMsg(requestID, err.Error(), start)
	}
	// The container lookup spends from the same budget Core is waiting on.
	remaining := timeout - time.Since(start)
	if remaining <= 0 {
		return rconErrMsg(requestID, "rcon timed out resolving the server's address", start)
	}
	out, err := execRcon(addr, req.RconPassword, req.Command, remaining)
	if err != nil {
		return rconErrMsg(requestID, err.Error(), start)
	}
	return &pb.NodeMessage{
		RequestId:  requestID,
		ServerUuid: serverUUID,
		Payload: &pb.NodeMessage_RconExecResp{
			RconExecResp: &pb.RconExecResp{
				Ok:         true,
				Output:     out,
				DurationMs: time.Since(start).Milliseconds(),
			},
		},
	}
}

func rconErrMsg(requestID, msg string, start time.Time) *pb.NodeMessage {
	return &pb.NodeMessage{
		RequestId: requestID,
		Payload: &pb.NodeMessage_RconExecResp{
			RconExecResp: &pb.RconExecResp{
				Ok:         false,
				Error:      msg,
				DurationMs: time.Since(start).Milliseconds(),
			},
		},
	}
}
