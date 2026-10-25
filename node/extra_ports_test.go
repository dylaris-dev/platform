package main

import (
	"testing"

	"github.com/docker/go-connections/nat"
)

func TestParseExtraPortsKeepsOnlyWhatCanBeBound(t *testing.T) {
	raw := []byte(`{"v":1,"ports":[
		{"p":"udp","h":26001,"c":24454},
		{"p":"TCP","h":26002,"c":8100},
		{"p":"udp","h":26001,"c":9999},
		{"p":"tcp","h":25601,"c":25565},
		{"p":"tcp","h":25521,"c":80},
		{"p":"sctp","h":26003,"c":1},
		{"p":"udp","h":80,"c":1},
		{"p":"udp","h":26004,"c":0}
	]}`)
	got := parseExtraPorts(raw, 25601)
	want := []extraPort{{"udp", 26001, 24454}, {"tcp", 26002, 8100}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{``, `{}`, `{"v":2,"ports":[{"p":"udp","h":26001,"c":1}]}`, `not json`} {
		if got := parseExtraPorts([]byte(bad), 0); len(got) != 0 {
			t.Errorf("parseExtraPorts(%q) = %+v, want none", bad, got)
		}
	}
}

// The port ledger adopts "the game port" from the container. With an extra
// TCP port next to it, a reader that took the first binding of the map picked
// either at random. Repeated because map order is what made it random.
func TestTheGamePortIsNeverAnExtraPort(t *testing.T) {
	for i := 0; i < 200; i++ {
		pm := nat.PortMap{"25565/tcp": {{HostIP: "0.0.0.0", HostPort: "25601"}}}
		exposed := nat.PortSet{"25565/tcp": {}}
		label := addExtraPorts(pm, exposed, []extraPort{{"tcp", 26002, 8100}, {"udp", 26001, 24454}, {"tcp", 26005, 25565}})
		if label != "tcp:26002,udp:26001,tcp:26005" {
			t.Fatalf("label = %q", label)
		}
		if h, c := boundPorts(pm, label); h != 25601 || c != 25565 {
			t.Fatalf("boundPorts = %d, %d, want the game binding 25601 -> 25565", h, c)
		}
		if len(pm["25565/tcp"]) != 2 || len(exposed) != 3 {
			t.Fatalf("bindings %+v exposed %+v", pm, exposed)
		}
	}
}

func TestIsExtraBinding(t *testing.T) {
	label := "udp:26001,tcp:26002"
	for _, c := range []struct {
		k    nat.Port
		host string
		want bool
	}{
		{"24454/udp", "26001", true},
		{"8100/tcp", "26002", true},
		{"8100/udp", "26002", false},
		{"25565/tcp", "25601", false},
	} {
		if got := isExtraBinding(label, c.k, c.host); got != c.want {
			t.Errorf("isExtraBinding(%s, %s) = %v, want %v", c.k, c.host, got, c.want)
		}
	}
	if isExtraBinding("", "8100/tcp", "26002") {
		t.Error("no label, no extra ports")
	}
}
