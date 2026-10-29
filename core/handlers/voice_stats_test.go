package handlers

import (
	"testing"

	"dylaris-core/services"
	"dylaris-core/store"
)

func TestVoiceOverviewGroupsByRegion(t *testing.T) {
	relays := []services.VoiceRelay{{ID: "v1", Region: "eu"}, {ID: "v2", Region: "eu"}, {ID: "v3", Region: "us"}}
	ports := []store.VoicePort{
		{ServerID: 1, Region: "eu", Port: 26000, Mod: "svc"},
		{ServerID: 2, Region: "eu", Port: 26001, Mod: "plasmo"},
		{ServerID: 3, Region: "us", Port: 26000, Mod: "svc"},
	}
	usage := []store.VoiceUsageRow{
		{ServerID: 1, ServerName: "a", Region: "eu", RxBytes: 100, TxBytes: 1000},
		{ServerID: 2, ServerName: "b", Region: "eu", RxBytes: 20, TxBytes: 200},
		{ServerID: 3, ServerName: "c", Region: "us", RxBytes: 5, TxBytes: 50},
	}
	ov := buildVoiceOverview("2026-10", relays, ports, usage)

	if len(ov.Regions) != 2 || ov.Regions[0].Region != "eu" || ov.Regions[1].Region != "us" {
		t.Fatalf("regions = %+v, want eu then us", ov.Regions)
	}
	if len(ov.Servers) != 3 {
		t.Fatalf("servers = %+v, want 3", ov.Servers)
	}
	for _, rs := range ov.Regions {
		var rx, tx int64
		for _, s := range ov.Servers {
			if s.Region == rs.Region {
				rx += s.RxBytes
				tx += s.TxBytes
			}
		}
		if rs.RxBytes != rx || rs.TxBytes != tx {
			t.Errorf("%s: region rx/tx %d/%d, servers sum to %d/%d", rs.Region, rs.RxBytes, rs.TxBytes, rx, tx)
		}
		if rs.PortsTotal != 1000 {
			t.Errorf("%s: PortsTotal = %d, want 1000", rs.Region, rs.PortsTotal)
		}
	}
	eu, us := ov.Regions[0], ov.Regions[1]
	if eu.RxBytes != 120 || eu.TxBytes != 1200 || us.RxBytes != 5 || us.TxBytes != 50 {
		t.Errorf("region sums wrong: eu %+v us %+v", eu, us)
	}
	if eu.Relays != 2 || us.Relays != 1 || eu.PortsUsed != 2 || us.PortsUsed != 1 {
		t.Errorf("relay/port counts wrong: eu %+v us %+v", eu, us)
	}
	if s := ov.Servers[1]; s.ServerID != 2 || s.Port != 26001 || s.Mod != "plasmo" {
		t.Errorf("server 2 lost its port row: %+v", s)
	}
}

func TestVoiceOverviewListsIdlePortHolders(t *testing.T) {
	ports := []store.VoicePort{{ServerID: 9, ServerUUID: "u-9", ServerName: "idle", Region: "eu", Port: 26005, Mod: "svc"}}
	ov := buildVoiceOverview("2026-10", nil, ports, nil)
	if len(ov.Servers) != 1 || ov.Servers[0].Port != 26005 || ov.Servers[0].RxBytes != 0 || ov.Servers[0].ServerName != "idle" {
		t.Fatalf("servers = %+v, want the idle holder with zero bytes", ov.Servers)
	}
	if ov.Relays == nil {
		t.Error("relays must encode as [], not null")
	}
}

func TestBedrockServerStatsMapsRowsAndIsNeverNil(t *testing.T) {
	if got := bedrockServerStats(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty = %#v, want a non-nil empty slice (JSON [])", got)
	}
	got := bedrockServerStats([]store.BedrockUsageRow{{ServerID: 3, ServerName: "a", Region: "eu", RxBytes: 5, TxBytes: 9}})
	if len(got) != 1 || got[0] != (BedrockServerStat{ServerID: 3, ServerName: "a", Region: "eu", RxBytes: 5, TxBytes: 9}) {
		t.Fatalf("got %+v", got)
	}
}
