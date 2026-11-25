package main

import (
	"encoding/json"
	"testing"

	"github.com/docker/docker/api/types/container"
)

type seccompRule struct {
	Names  []string `json:"names"`
	Action string   `json:"action"`
	Args   []struct {
		Index    uint   `json:"index"`
		Value    uint64 `json:"value"`
		ValueTwo uint64 `json:"valueTwo"`
		Op       string `json:"op"`
	} `json:"args"`
}

type seccompProfile struct {
	DefaultAction string        `json:"defaultAction"`
	Syscalls      []seccompRule `json:"syscalls"`
}

func parseSeccomp(t *testing.T, b []byte) seccompProfile {
	t.Helper()
	var p seccompProfile
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("profile does not parse: %v", err)
	}
	return p
}

// ioctlAllowed evaluates the profile's ioctl rules for request cmd the way
// libseccomp does: any ALLOW rule whose conditions all hold lets it through,
// otherwise the default action (EPERM) applies.
func ioctlAllowed(t *testing.T, p seccompProfile, cmd uint64) bool {
	t.Helper()
	for _, r := range p.Syscalls {
		if r.Action != "SCMP_ACT_ALLOW" || !containsStr(r.Names, "ioctl") {
			continue
		}
		ok := true
		for _, a := range r.Args {
			if a.Index != 1 || a.Op != "SCMP_CMP_MASKED_EQ" {
				t.Fatalf("unexpected ioctl condition %+v", a)
			}
			ok = ok && cmd&a.Value == a.ValueTwo
		}
		if ok {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// A tenant owns its files, and in the initial user namespace the owner may
// set their project id and clear project inheritance (FS_IOC_FSSETXATTR,
// FS_IOC_SETFLAGS): its data then left its quota project, past the disk
// limit and the disk guard (R52-03). The kernel truncates the request to
// 32 bits, so a request with high bits set must be refused as well.
func TestTenantSeccompRefusesProjectIoctls(t *testing.T) {
	p := parseSeccomp(t, []byte(tenantSeccompProfile))
	for _, r := range p.Syscalls {
		if containsStr(r.Names, "ioctl") && r.Action == "SCMP_ACT_ALLOW" && len(r.Args) == 0 {
			t.Fatal("ioctl is still allowed unconditionally; libseccomp then drops every conditional ioctl rule")
		}
	}
	for _, tc := range []struct {
		name string
		cmd  uint64
		want bool
	}{
		{"FS_IOC_FSSETXATTR", 0x401c5820, false},
		{"FS_IOC_FSSETXATTR high bits", 0x1_401c5820, false},
		{"FS_IOC_FSSETXATTR sign-extended", 0xffffffff_401c5820, false},
		{"FS_IOC_SETFLAGS", 0x40086602, false},
		{"FS_IOC32_SETFLAGS", 0x40046602, false},
		{"FS_IOC_FSGETXATTR", 0x801c581f, true},
		{"FS_IOC_GETFLAGS", 0x80086601, true},
		{"FS_IOC_GETFLAGS sign-extended", 0xffffffff_80086601, true},
		{"TCGETS", 0x5401, true},
		{"FIONREAD", 0x541b, true},
		{"FIONBIO", 0x5421, true},
		{"neighbour below", 0x401c581f, true},
		{"neighbour above", 0x401c5821, true},
		{"zero", 0, true},
		{"all ones", 0xffffffff, true},
	} {
		if got := ioctlAllowed(t, p, tc.cmd); got != tc.want {
			t.Errorf("%s (%#x): allowed = %v, want %v", tc.name, tc.cmd, got, tc.want)
		}
	}
}

// The profile is Docker's default plus the ioctl change: nothing the default
// allows may go missing, or Java breaks in ways no test here would show.
func TestTenantSeccompKeepsTheDockerDefault(t *testing.T) {
	base := parseSeccomp(t, mobyDefaultSeccomp)
	got := parseSeccomp(t, []byte(tenantSeccompProfile))
	if got.DefaultAction != base.DefaultAction || base.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Errorf("defaultAction = %q, base %q, want SCMP_ACT_ERRNO", got.DefaultAction, base.DefaultAction)
	}
	have := map[string]bool{}
	for _, r := range got.Syscalls {
		if r.Action == "SCMP_ACT_ALLOW" {
			for _, n := range r.Names {
				have[n] = true
			}
		}
	}
	for _, r := range base.Syscalls {
		if r.Action != "SCMP_ACT_ALLOW" {
			continue
		}
		for _, n := range r.Names {
			if !have[n] {
				t.Errorf("%s is no longer allowed", n)
			}
		}
	}
	if len(got.Syscalls) < len(base.Syscalls) {
		t.Errorf("%d rules, base has %d", len(got.Syscalls), len(base.Syscalls))
	}
}

func TestATenantContainerGetsTheSeccompProfile(t *testing.T) {
	t.Cleanup(func() { daemonNoSeccomp.Store(false) })
	for _, tc := range []struct {
		name      string
		noSeccomp bool
		want      bool
	}{
		{"daemon with seccomp", false, true},
		// A custom profile on a daemon without seccomp fails every create.
		{"daemon without seccomp", true, false},
	} {
		daemonNoSeccomp.Store(tc.noSeccomp)
		hc := &container.HostConfig{}
		hardenTenantContainer(hc)
		has := false
		for _, o := range hc.SecurityOpt {
			has = has || o == "seccomp="+tenantSeccompProfile
		}
		if has != tc.want {
			t.Errorf("%s: seccomp profile set = %v, want %v", tc.name, has, tc.want)
		}
	}
}

func TestDaemonHasSeccomp(t *testing.T) {
	for _, tc := range []struct {
		opts []string
		want bool
	}{
		{[]string{"name=apparmor", "name=seccomp,profile=builtin", "name=cgroupns"}, true},
		{[]string{"name=seccomp,profile=unconfined"}, true},
		{[]string{"name=apparmor"}, false},
		{nil, false},
	} {
		if got := daemonHasSeccomp(tc.opts); got != tc.want {
			t.Errorf("%v: got %v, want %v", tc.opts, got, tc.want)
		}
	}
}
