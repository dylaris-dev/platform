package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
)

// mobyDefaultSeccomp is Docker's default seccomp profile, verbatim from
// github.com/moby/profiles seccomp/v0.1.0 (commit a33e0005c5fd), the version
// Docker 28.x and 29.x build in. To refresh it, replace the file with
// seccomp/default.json from the profiles tag the current Docker release pins
// (moby/moby go.mod, github.com/moby/profiles/seccomp) and update this
// comment; TestTenantSeccompKeepsTheDockerDefault checks the result.
//
//go:embed tenant_seccomp_moby_default.json
var mobyDefaultSeccomp []byte

// deniedIoctls are the requests that change an inode's quota project: in the
// initial user namespace the kernel lets a file's owner set its project id
// and clear project inheritance, so a tenant moved its own files out of its
// quota project and past the disk limit and the disk guard (R52-03).
var deniedIoctls = []uint32{
	0x401c5820, // FS_IOC_FSSETXATTR
	0x40086602, // FS_IOC_SETFLAGS (clears FS_PROJINHERIT_FL)
	0x40046602, // FS_IOC32_SETFLAGS, the 32-bit compat request
}

// tenantSeccompProfile is passed inline as SecurityOpt "seccomp=<json>",
// which is what `docker run --security-opt seccomp=file` sends too.
var tenantSeccompProfile = mustTenantSeccomp()

// daemonNoSeccomp is set when the Docker daemon has no seccomp support: a
// custom profile there fails every container create.
var daemonNoSeccomp atomic.Bool

func daemonHasSeccomp(securityOptions []string) bool {
	for _, o := range securityOptions {
		if strings.Contains(o, "name=seccomp") {
			return true
		}
	}
	return false
}

func mustTenantSeccomp() string {
	s, err := buildTenantSeccomp(mobyDefaultSeccomp, deniedIoctls)
	if err != nil {
		panic(err)
	}
	return s
}

// buildTenantSeccomp is base with ioctl allowed for every request except
// denied.
//
// A deny rule next to the default's unconditional allow does nothing:
// libseccomp keeps an unconditional rule and silently drops a conditional one
// for the same syscall, whichever comes first. So ioctl leaves the allow list
// and comes back as allow rules that cover every other request, and the denied
// ones fall to the default action (EPERM). The kernel takes the request as
// 32 bits, so the rules mask the argument to its low 32 bits: a request with
// high bits set reaches the same ioctl and must match the same way. Each rule
// is one masked prefix of the 32-bit space; the rules are ORed.
func buildTenantSeccomp(base []byte, denied []uint32) (string, error) {
	var p map[string]any
	if err := json.Unmarshal(base, &p); err != nil {
		return "", fmt.Errorf("seccomp profile: %w", err)
	}
	rules, _ := p["syscalls"].([]any)
	found := false
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		names, _ := rule["names"].([]any)
		// Only the plain allow-list rule; a conditional one (args, caps,
		// arches) never was what let ioctl through.
		if rule["action"] != "SCMP_ACT_ALLOW" || rule["args"] != nil || rule["includes"] != nil || rule["excludes"] != nil {
			continue
		}
		kept := names[:0]
		for _, n := range names {
			if n == "ioctl" {
				found = true
				continue
			}
			kept = append(kept, n)
		}
		rule["names"] = kept
	}
	if !found {
		return "", fmt.Errorf("seccomp profile: no unconditional ioctl allow to replace")
	}
	for _, b := range ioctlAllowBlocks(denied) {
		rules = append(rules, map[string]any{
			"names":  []string{"ioctl"},
			"action": "SCMP_ACT_ALLOW",
			"args": []map[string]any{{
				"index": 1, "value": b[0], "valueTwo": b[1], "op": "SCMP_CMP_MASKED_EQ",
			}},
		})
	}
	p["syscalls"] = rules
	out, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("seccomp profile: %w", err)
	}
	return string(out), nil
}

// ioctlAllowBlocks returns {mask, value} prefixes of the 32-bit space that
// together cover every value except denied: a prefix holding no denied value
// is one block, one holding some is split in two.
func ioctlAllowBlocks(denied []uint32) [][2]uint64 {
	var out [][2]uint64
	var split func(prefix uint64, bits uint)
	split = func(prefix uint64, bits uint) {
		mask := (uint64(0xffffffff) << (32 - bits)) & 0xffffffff
		hit := false
		for _, d := range denied {
			hit = hit || uint64(d)&mask == prefix
		}
		if !hit {
			out = append(out, [2]uint64{mask, prefix})
			return
		}
		if bits == 32 {
			return
		}
		split(prefix, bits+1)
		split(prefix|1<<(31-bits), bits+1)
	}
	split(0, 0)
	return out
}
