package main

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"testing"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("bad CIDR %q: %v", s, err)
	}
	return n
}

func TestCidrsOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"10.0.0.0/26", "10.0.0.0/26", true},
		{"10.0.0.0/26", "10.0.0.32/27", true}, // b inside a
		{"10.0.0.0/25", "10.0.0.0/26", true},  // b inside a (wider a)
		{"10.0.0.0/26", "10.0.0.64/26", false},
		{"10.0.0.0/26", "172.16.0.0/26", false},
	}
	for _, c := range cases {
		if got := cidrsOverlap(mustCIDR(t, c.a), mustCIDR(t, c.b)); got != c.want {
			t.Errorf("cidrsOverlap(%s,%s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestParseCIDRsSkipsJunk(t *testing.T) {
	got := parseCIDRs([]string{"10.0.0.0/26", "", "  ", "not-a-cidr", "fd00::/64", "192.168.5.0/24"})
	if len(got) != 2 {
		t.Fatalf("parseCIDRs kept %d nets, want 2 (%v)", len(got), got)
	}
	if got[0].String() != "10.0.0.0/26" || got[1].String() != "192.168.5.0/24" {
		t.Fatalf("parseCIDRs = %v, want [10.0.0.0/26 192.168.5.0/24]", got)
	}
}

func TestCapacityForPrefix(t *testing.T) {
	cases := []struct {
		prefix int
		want   int
	}{
		{26, 59}, // 64 - 5 (network, gateway, node, link, broadcast)
		{25, 123},
		{24, 251},
	}
	for _, c := range cases {
		if got := capacityForPrefix(c.prefix); got != c.want {
			t.Errorf("capacityForPrefix(%d) = %d, want %d", c.prefix, got, c.want)
		}
	}
}

func TestNextPrefixLen(t *testing.T) {
	if p, ok := nextPrefixLen(26); !ok || p != 25 {
		t.Errorf("nextPrefixLen(26) = %d,%v want 25,true", p, ok)
	}
	if p, ok := nextPrefixLen(25); !ok || p != 24 {
		t.Errorf("nextPrefixLen(25) = %d,%v want 24,true", p, ok)
	}
	if _, ok := nextPrefixLen(24); ok {
		t.Errorf("nextPrefixLen(24) ok = true, want false (already at max block)")
	}
}

func TestServerIPInSubnetStableAndDistinct(t *testing.T) {
	sn := mustCIDR(t, "10.0.0.0/26")
	// node is .2, link is .3; slot 1 -> .4, slot 59 -> .62.
	if ip := nodeIPInSubnet(sn); ip.String() != "10.0.0.2" {
		t.Fatalf("nodeIPInSubnet = %s, want 10.0.0.2", ip)
	}
	if ip := serverIPInSubnet(sn, 1); ip.String() != "10.0.0.4" {
		t.Fatalf("serverIPInSubnet(slot 1) = %s, want 10.0.0.4", ip)
	}
	if ip := serverIPInSubnet(sn, 59); ip.String() != "10.0.0.62" {
		t.Fatalf("serverIPInSubnet(slot 59) = %s, want 10.0.0.62", ip)
	}
	// Same slot in a shifted subnet (enlarge case) remaps deterministically.
	big := mustCIDR(t, "10.0.1.0/25")
	if ip := serverIPInSubnet(big, 1); ip.String() != "10.0.1.4" {
		t.Fatalf("serverIPInSubnet in /25 slot 1 = %s, want 10.0.1.4", ip)
	}
}

func TestNextFreeSubnetAvoidsOverlaps(t *testing.T) {
	t.Run("empty used -> first block", func(t *testing.T) {
		got, err := nextFreeSubnet(nil, 26)
		if err != nil {
			t.Fatalf("nextFreeSubnet: %v", err)
		}
		if got.String() != "10.0.0.0/26" {
			t.Fatalf("got %s, want 10.0.0.0/26", got)
		}
	})
	t.Run("skips used blocks in order", func(t *testing.T) {
		used := []*net.IPNet{mustCIDR(t, "10.0.0.0/26"), mustCIDR(t, "10.0.0.64/26")}
		got, err := nextFreeSubnet(used, 26)
		if err != nil {
			t.Fatalf("nextFreeSubnet: %v", err)
		}
		if got.String() != "10.0.0.128/26" {
			t.Fatalf("got %s, want 10.0.0.128/26", got)
		}
	})
	t.Run("skips a wide used block", func(t *testing.T) {
		// A used /24 covering 10.0.0.0-10.0.0.255 pushes the next /26 to 10.0.1.0.
		used := []*net.IPNet{mustCIDR(t, "10.0.0.0/24")}
		got, err := nextFreeSubnet(used, 26)
		if err != nil {
			t.Fatalf("nextFreeSubnet: %v", err)
		}
		if got.String() != "10.0.1.0/26" {
			t.Fatalf("got %s, want 10.0.1.0/26", got)
		}
	})
}

func TestAllocatorSubnetAndSlotStable(t *testing.T) {
	a := loadTenantAllocator(t.TempDir())
	if _, err := a.ensureSubnet("owner-A", nil); err != nil {
		t.Fatalf("ensureSubnet: %v", err)
	}
	// First owner gets 10.0.0.0/26.
	if sn, _ := a.subnetString("owner-A"); sn != "10.0.0.0/26" {
		t.Fatalf("owner-A subnet = %s, want 10.0.0.0/26", sn)
	}
	s1, err := a.allocateSlot("owner-A", "srv-1")
	if err != nil {
		t.Fatalf("allocateSlot srv-1: %v", err)
	}
	// Idempotent: same server -> same slot.
	s1again, _ := a.allocateSlot("owner-A", "srv-1")
	if s1 != s1again {
		t.Fatalf("slot not stable: %d vs %d", s1, s1again)
	}
	s2, _ := a.allocateSlot("owner-A", "srv-2")
	if s2 == s1 {
		t.Fatalf("distinct servers share slot %d", s1)
	}
	ip, _, err := a.ipFor("owner-A", "srv-1")
	if err != nil {
		t.Fatalf("ipFor: %v", err)
	}
	if ip.String() != "10.0.0.4" {
		t.Fatalf("srv-1 ip = %s, want 10.0.0.4", ip)
	}
	if owner, ok := a.ownerForServer("srv-2"); !ok || owner != "owner-A" {
		t.Fatalf("ownerForServer(srv-2) = %q,%v want owner-A,true", owner, ok)
	}
}

func TestAllocatorSecondOwnerGetsSecondBlock(t *testing.T) {
	a := loadTenantAllocator(t.TempDir())
	if _, err := a.ensureSubnet("owner-A", nil); err != nil {
		t.Fatalf("ensureSubnet A: %v", err)
	}
	if _, err := a.ensureSubnet("owner-B", nil); err != nil {
		t.Fatalf("ensureSubnet B: %v", err)
	}
	snA, _ := a.subnetString("owner-A")
	snB, _ := a.subnetString("owner-B")
	if snA != "10.0.0.0/26" || snB != "10.0.0.64/26" {
		t.Fatalf("subnets = %s,%s want 10.0.0.0/26,10.0.0.64/26", snA, snB)
	}
	// Docker already using 10.0.0.0/26 pushes a new owner past it.
	dockerUsed := parseCIDRs([]string{"10.0.0.128/26"})
	if _, err := a.ensureSubnet("owner-C", dockerUsed); err != nil {
		t.Fatalf("ensureSubnet C: %v", err)
	}
	snC, _ := a.subnetString("owner-C")
	if snC != "10.0.0.192/26" {
		t.Fatalf("owner-C subnet = %s, want 10.0.0.192/26 (avoids A,B and docker .128)", snC)
	}
}

func TestAllocatorSubnetFullThenEnlarge(t *testing.T) {
	a := loadTenantAllocator(t.TempDir())
	if _, err := a.ensureSubnet("owner-A", nil); err != nil {
		t.Fatalf("ensureSubnet: %v", err)
	}
	// Fill all 59 slots of the /26.
	for i := 0; i < 59; i++ {
		if _, err := a.allocateSlot("owner-A", fmt.Sprintf("srv-%d", i)); err != nil {
			t.Fatalf("allocateSlot %d: %v", i, err)
		}
	}
	// 61st overflows.
	if _, err := a.allocateSlot("owner-A", "srv-overflow"); !errors.Is(err, errSubnetFull) {
		t.Fatalf("expected errSubnetFull, got %v", err)
	}
	oldN, newN, err := a.enlarge("owner-A", nil)
	if err != nil {
		t.Fatalf("enlarge: %v", err)
	}
	if oldN.String() != "10.0.0.0/26" {
		t.Fatalf("old = %s, want 10.0.0.0/26", oldN)
	}
	ones, _ := newN.Mask.Size()
	if ones != 25 {
		t.Fatalf("new prefix = /%d, want /25", ones)
	}
	// Existing slots keep their index; srv-0 stays slot 1 -> remapped IP.
	ip, _, err := a.ipFor("owner-A", "srv-0")
	if err != nil {
		t.Fatalf("ipFor after enlarge: %v", err)
	}
	if !newN.Contains(ip) {
		t.Fatalf("remapped ip %s not in new subnet %s", ip, newN)
	}
	// Now the overflow server fits.
	if _, err := a.allocateSlot("owner-A", "srv-overflow"); err != nil {
		t.Fatalf("allocateSlot after enlarge: %v", err)
	}
}

func TestAllocatorReleaseFreesOwnerWhenEmpty(t *testing.T) {
	a := loadTenantAllocator(t.TempDir())
	if _, err := a.ensureSubnet("owner-A", nil); err != nil {
		t.Fatalf("ensureSubnet: %v", err)
	}
	a.allocateSlot("owner-A", "srv-1")
	a.allocateSlot("owner-A", "srv-2")
	empty, err := a.release("owner-A", "srv-1")
	if err != nil {
		t.Fatalf("release srv-1: %v", err)
	}
	if empty {
		t.Fatalf("owner-A reported empty with srv-2 still present")
	}
	empty, err = a.release("owner-A", "srv-2")
	if err != nil {
		t.Fatalf("release srv-2: %v", err)
	}
	if !empty {
		t.Fatalf("owner-A should be empty after last release")
	}
	if _, ok := a.subnetString("owner-A"); ok {
		t.Fatalf("owner-A subnet should be gone after empty release")
	}
}

func TestAllocatorPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := loadTenantAllocator(dir)
	a.ensureSubnet("owner-A", nil)
	a.allocateSlot("owner-A", "srv-1")

	// Reload from disk: assignments survive.
	b := loadTenantAllocator(dir)
	if owner, ok := b.ownerForServer("srv-1"); !ok || owner != "owner-A" {
		t.Fatalf("reloaded ownerForServer(srv-1) = %q,%v want owner-A,true", owner, ok)
	}
	ip, _, err := b.ipFor("owner-A", "srv-1")
	if err != nil || ip.String() != "10.0.0.4" {
		t.Fatalf("reloaded ipFor(srv-1) = %v,%v want 10.0.0.4", ip, err)
	}
}

// enlarge() writes the new subnet to disk before any Docker work is attempted,
// which is the only order that stops two callers picking the same free block.
// The cost is that an abandoned enlarge leaves the file describing a network
// nobody built - and then every address handed out is outside the live network,
// so nothing on it starts. restoreSubnet is how that is undone.
func TestRestoreSubnetAfterAnAbandonedEnlarge(t *testing.T) {
	a := loadTenantAllocator(t.TempDir())

	if _, err := a.ensureSubnet("owner-A", nil); err != nil {
		t.Fatalf("ensureSubnet: %v", err)
	}
	before := a.state.Owners["owner-A"].Subnet

	oldNet, newNet, err := a.enlarge("owner-A", nil)
	if err != nil {
		t.Fatalf("enlarge: %v", err)
	}
	if a.state.Owners["owner-A"].Subnet == before {
		t.Fatal("enlarge did not move the owner, so this proves nothing")
	}
	if oldNet.String() != before {
		t.Fatalf("enlarge reported old subnet %s, want %s", oldNet, before)
	}

	if err := a.restoreSubnet("owner-A", oldNet); err != nil {
		t.Fatalf("restoreSubnet: %v", err)
	}
	if got := a.state.Owners["owner-A"].Subnet; got != before {
		t.Errorf("subnet = %s, want %s (%s was never built)", got, before, newNet)
	}

	// And it has to survive a restart: the node reads this file at boot, so a
	// rollback that lived only in memory would come back wrong.
	reloaded := loadTenantAllocator(filepath.Dir(a.path))
	if got := reloaded.state.Owners["owner-A"].Subnet; got != before {
		t.Errorf("after reload subnet = %s, want %s - the rollback was not saved", got, before)
	}
}

// Restoring is safe to call when there is nothing to restore. Both cases reach
// it on the abandon path: an owner whose record is already gone, and an enlarge
// that failed before it moved anything.
func TestRestoreSubnetIsANoOpWithNothingToRestore(t *testing.T) {
	a := loadTenantAllocator(t.TempDir())
	if _, err := a.ensureSubnet("owner-A", nil); err != nil {
		t.Fatalf("ensureSubnet: %v", err)
	}
	current := a.state.Owners["owner-A"].Subnet

	if err := a.restoreSubnet("owner-A", nil); err != nil {
		t.Errorf("nil subnet: %v", err)
	}
	if err := a.restoreSubnet("owner-nobody", mustCIDR(t, "10.99.0.0/26")); err != nil {
		t.Errorf("unknown owner: %v", err)
	}
	if got := a.state.Owners["owner-A"].Subnet; got != current {
		t.Errorf("subnet moved to %s, want %s left alone", got, current)
	}
}
