package main

import (
	"os"
	"runtime"
	"strings"

	pb "dylaris-proto/node"
)

// machineIdentity is what this node SAYS it is, sent with every auth so an
// operator can recognise it in the panel when Core is refusing it.
//
// Nothing here is a credential and Core stores none of it on the node row. It
// exists for one screen: a node whose secret and Core's have diverged retries
// forever and, until now, produced one line on Core's stdout and nothing
// anywhere else. "Some identity from some address" is not enough to decide
// whether to let a machine back in; a hostname and a hardware size are.
//
// Sent at AUTH rather than in the heartbeat on purpose. The heartbeat rides on
// Redis and needs the per-node ACL credentials, which is exactly what a node in
// this state does not have.
func machineIdentity() *pb.NodeIdentity {
	return &pb.NodeIdentity{
		Hostname:    machineHostname(),
		CpuCores:    int32(runtime.NumCPU()),
		CpuModel:    cpuModel(),
		MemoryBytes: totalMemoryBytes(),
	}
}

// machineHostname prefers NODE_HOSTNAME over the kernel's answer.
//
// Inside a Swarm task os.Hostname() is the TASK name - a container id with a
// slot number - which is useless for recognising a machine. Swarm can template
// the real one, so the compose file passes NODE_HOSTNAME={{.Node.Hostname}}.
// The kernel value stays as the fallback for a node started with plain Docker,
// where it is the right answer.
func machineHostname() string {
	if h := strings.TrimSpace(os.Getenv("NODE_HOSTNAME")); h != "" {
		return h
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// cpuModel reads the first model name out of /proc/cpuinfo. Empty on anything
// that does not have one, which includes every non-Linux build - a missing
// model is a blank field on a screen, not an error.
func cpuModel() string {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(name) {
		case "model name", "Model":
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// totalMemoryBytes reads MemTotal out of /proc/meminfo, which is in kB.
// Zero when it cannot be read.
func totalMemoryBytes() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != "MemTotal" {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return 0
		}
		var kb int64
		for _, c := range fields[0] {
			if c < '0' || c > '9' {
				return 0
			}
			kb = kb*10 + int64(c-'0')
		}
		return kb * 1024
	}
	return 0
}
