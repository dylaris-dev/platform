package database

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"dylaris-core/store"
)

// Each test allocates in a region of its own, so rows another test or an
// earlier local run left behind never take the port a test expects.
func voiceRegion() string { return uniqueName("vr_") }

func allocVoice(t *testing.T, st *store.PostgresStore, serverID int, region string, min, max int) store.VoicePort {
	t.Helper()
	got, err := st.AllocateVoicePort(store.VoicePort{
		ServerID: serverID, Region: region, Mod: "svc", InternalPort: 24454,
		UplinkKey: bytes.Repeat([]byte{byte(serverID)}, 32), Host: "play.example.test",
	}, min, max)
	if err != nil {
		t.Fatalf("AllocateVoicePort(server %d, %s): %v", serverID, region, err)
	}
	return got
}

func TestIntegrationVoicePortsLowestFree(t *testing.T) {
	_, st := integrationDB(t)
	region := voiceRegion()
	a, b, c := newFixture(t, st), newFixture(t, st), newFixture(t, st)
	for i, f := range []*fixture{a, b, c} {
		if got := allocVoice(t, st, f.server.ID, region, 26000, 26999); got.Port != 26000+i {
			t.Fatalf("server %d got port %d, want %d", i, got.Port, 26000+i)
		}
	}
	// Deleting the SERVER, not the row: the cascade is what frees the port.
	if err := st.DeleteServer(b.server.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	if p, err := st.GetVoicePort(b.server.ID); err != nil || p != nil {
		t.Fatalf("voice port of a deleted server = %+v, %v; want nil, nil", p, err)
	}
	d := newFixture(t, st)
	if got := allocVoice(t, st, d.server.ID, region, 26000, 26999); got.Port != 26001 {
		t.Fatalf("fourth server got port %d, want the freed 26001", got.Port)
	}
}

func TestIntegrationVoicePortsSameRegionKeepsPortAndKey(t *testing.T) {
	_, st := integrationDB(t)
	region := voiceRegion()
	f := newFixture(t, st)
	first := allocVoice(t, st, f.server.ID, region, 26000, 26999)

	again, err := st.AllocateVoicePort(store.VoicePort{
		ServerID: f.server.ID, Region: region, Mod: "plasmo", InternalPort: 24455,
		UplinkKey: bytes.Repeat([]byte{0xFF}, 32), Host: "other.example.test",
	}, 26000, 26999)
	if err != nil {
		t.Fatalf("second AllocateVoicePort: %v", err)
	}
	if again.Port != first.Port || !bytes.Equal(again.UplinkKey, first.UplinkKey) {
		t.Fatalf("same region moved port/key: %d %x -> %d %x", first.Port, first.UplinkKey, again.Port, again.UplinkKey)
	}
	got, err := st.GetVoicePort(f.server.ID)
	if err != nil || got == nil {
		t.Fatalf("GetVoicePort: %+v, %v", got, err)
	}
	if got.Mod != "plasmo" || got.InternalPort != 24455 || got.Host != "other.example.test" {
		t.Fatalf("mod/internal_port/host not updated: %+v", got)
	}
	if got.Port != first.Port || !bytes.Equal(got.UplinkKey, first.UplinkKey) || got.ServerUUID != f.server.UUID || got.ServerName != f.server.Name {
		t.Fatalf("stored row = %+v, want port %d, the first key and uuid %s", got, first.Port, f.server.UUID)
	}
}

func TestIntegrationVoicePortsRegionChangeReallocates(t *testing.T) {
	_, st := integrationDB(t)
	from, to := voiceRegion(), voiceRegion()
	filler, f := newFixture(t, st), newFixture(t, st)
	allocVoice(t, st, filler.server.ID, to, 26000, 26999) // takes 26000 in the target region
	allocVoice(t, st, f.server.ID, from, 26000, 26999)    // 26000 in the source region

	moved, err := st.AllocateVoicePort(store.VoicePort{
		ServerID: f.server.ID, Region: to, Mod: "svc", InternalPort: 24454,
		UplinkKey: bytes.Repeat([]byte{0xAB}, 32), Host: "h",
	}, 26000, 26999)
	if err != nil {
		t.Fatalf("AllocateVoicePort in new region: %v", err)
	}
	if moved.Region != to || moved.Port != 26001 || !bytes.Equal(moved.UplinkKey, bytes.Repeat([]byte{0xAB}, 32)) {
		t.Fatalf("region change = %+v, want region %s, port 26001 and the new key", moved, to)
	}
	got, _ := st.GetVoicePort(f.server.ID)
	if got == nil || got.Region != to || got.Port != 26001 {
		t.Fatalf("stored row after region change = %+v", got)
	}
}

func TestIntegrationVoicePortsPerRegionUnique(t *testing.T) {
	_, st := integrationDB(t)
	eu, us := voiceRegion(), voiceRegion()
	a, b := newFixture(t, st), newFixture(t, st)
	if p := allocVoice(t, st, a.server.ID, eu, 26000, 26999); p.Port != 26000 {
		t.Fatalf("eu port = %d", p.Port)
	}
	if p := allocVoice(t, st, b.server.ID, us, 26000, 26999); p.Port != 26000 {
		t.Fatalf("us port = %d, want 26000 alongside eu", p.Port)
	}
}

func TestIntegrationVoicePoolFull(t *testing.T) {
	_, st := integrationDB(t)
	region := voiceRegion()
	a, b := newFixture(t, st), newFixture(t, st)
	allocVoice(t, st, a.server.ID, region, 26000, 26000)
	_, err := st.AllocateVoicePort(store.VoicePort{
		ServerID: b.server.ID, Region: region, Mod: "svc", InternalPort: 24454,
		UplinkKey: make([]byte, 32), Host: "h",
	}, 26000, 26000)
	if !errors.Is(err, store.ErrVoicePoolFull) {
		t.Fatalf("second server in a one-port pool: err = %v, want ErrVoicePoolFull", err)
	}
}

func TestIntegrationVoiceUsageCascade(t *testing.T) {
	db, st := integrationDB(t)
	region := voiceRegion()
	f := newFixture(t, st)
	period := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := st.AddVoiceUsage(f.server.ID, period, region, 10, 20); err != nil {
			t.Fatalf("AddVoiceUsage: %v", err)
		}
	}
	rows, err := st.ListVoiceUsage(period)
	if err != nil {
		t.Fatalf("ListVoiceUsage: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ServerID == f.server.ID && r.Region == region {
			found = true
			if r.RxBytes != 20 || r.TxBytes != 40 || r.ServerUUID != f.server.UUID || r.ServerName != f.server.Name {
				t.Fatalf("usage row = %+v, want rx 20, tx 40 and the server's uuid/name", r)
			}
		}
	}
	if !found {
		t.Fatal("ListVoiceUsage did not return the row just written")
	}

	if err := st.DeleteServer(f.server.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM voice_usage WHERE server_id = $1`, f.server.ID).Scan(&n); err != nil {
		t.Fatalf("count voice_usage: %v", err)
	}
	if n != 0 {
		t.Fatalf("%d voice_usage rows survived their server", n)
	}
}

func TestIntegrationVoiceSubsetColumn(t *testing.T) {
	_, st := integrationDB(t)
	f := newFixture(t, st)
	region := voiceRegion()
	period := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := st.AddTrafficUsageRegion(f.user.ID, period, region, store.TrafficKindEdge, store.TrafficProductBYON, 1000); err != nil {
		t.Fatalf("AddTrafficUsageRegion: %v", err)
	}
	if err := st.AddTrafficUsageRegionVoice(f.user.ID, period, region, store.TrafficKindEdge, store.TrafficProductBYON, 300); err != nil {
		t.Fatalf("AddTrafficUsageRegionVoice: %v", err)
	}
	got, err := st.GetTrafficUsageRegions(f.user.ID, period)
	if err != nil {
		t.Fatalf("GetTrafficUsageRegions: %v", err)
	}
	if len(got) != 1 || got[0].Bytes != 1000 || got[0].VoiceBytes != 300 {
		t.Fatalf("regions = %+v, want one row with Bytes 1000 and VoiceBytes 300", got)
	}
}

func TestIntegrationUpdateVoicePortModNeverCreatesOrMoves(t *testing.T) {
	_, st := integrationDB(t)
	region := voiceRegion()
	f := newFixture(t, st)
	first := allocVoice(t, st, f.server.ID, region, 26000, 26999)

	for _, miss := range []struct {
		name   string
		region string
		port   int
	}{
		{"another region", voiceRegion(), first.Port},
		{"another port", region, first.Port + 1},
	} {
		ok, err := st.UpdateVoicePortMod(f.server.ID, miss.region, miss.port, "plasmo", 24455, "other.example.test")
		if err != nil || ok {
			t.Fatalf("%s: UpdateVoicePortMod = %v, %v; want false, nil", miss.name, ok, err)
		}
	}
	got, err := st.GetVoicePort(f.server.ID)
	if err != nil || got == nil || got.Mod != "svc" || got.Region != region || got.Port != first.Port {
		t.Fatalf("a missed update changed the row: %+v, %v", got, err)
	}

	ok, err := st.UpdateVoicePortMod(f.server.ID, region, first.Port, "plasmo", 24455, "other.example.test")
	if err != nil || !ok {
		t.Fatalf("matching UpdateVoicePortMod = %v, %v; want true, nil", ok, err)
	}
	got, err = st.GetVoicePort(f.server.ID)
	if err != nil || got == nil || got.Mod != "plasmo" || got.InternalPort != 24455 || got.Host != "other.example.test" ||
		got.Port != first.Port || !bytes.Equal(got.UplinkKey, first.UplinkKey) {
		t.Fatalf("matching update stored %+v, %v", got, err)
	}

	if err := st.DeleteVoicePort(f.server.ID); err != nil {
		t.Fatalf("DeleteVoicePort: %v", err)
	}
	ok, err = st.UpdateVoicePortMod(f.server.ID, region, first.Port, "svc", 24454, "play.example.test")
	if err != nil || ok {
		t.Fatalf("update of a deleted row = %v, %v; want false, nil", ok, err)
	}
	if p, err := st.GetVoicePort(f.server.ID); err != nil || p != nil {
		t.Fatalf("update resurrected the row: %+v, %v", p, err)
	}
}

// A scope row answers, an update flips it in place, and a delete hands the
// question back to the next scope (nil, not false).
func TestIntegrationVoicePermissions(t *testing.T) {
	_, st := integrationDB(t)
	scope := "user:" + uniqueName("vp_")
	t.Cleanup(func() { st.DeleteVoicePermission(scope) })

	if v, err := st.GetVoicePermission(scope); err != nil || v != nil {
		t.Fatalf("missing row = %v, %v; want nil, nil", v, err)
	}
	for _, want := range []bool{false, true} {
		if err := st.SetVoicePermission(scope, want); err != nil {
			t.Fatalf("SetVoicePermission(%v): %v", want, err)
		}
		if v, err := st.GetVoicePermission(scope); err != nil || v == nil || *v != want {
			t.Fatalf("after set %v: %v, %v", want, v, err)
		}
	}
	rows, err := st.ListVoicePermissions()
	if err != nil {
		t.Fatalf("ListVoicePermissions: %v", err)
	}
	found := false
	for _, r := range rows {
		found = found || (r.Scope == scope && r.Allowed && !r.UpdatedAt.IsZero())
	}
	if !found {
		t.Fatalf("%s not listed as allowed: %+v", scope, rows)
	}
	if err := st.DeleteVoicePermission(scope); err != nil {
		t.Fatalf("DeleteVoicePermission: %v", err)
	}
	if v, err := st.GetVoicePermission(scope); err != nil || v != nil {
		t.Fatalf("deleted row = %v, %v; want nil, nil", v, err)
	}
}

func TestIntegrationBedrockUsageAccumulatesAndCascades(t *testing.T) {
	db, st := integrationDB(t)
	f := newFixture(t, st)
	period := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := st.AddBedrockUsage(f.server.ID, period, "eu-central", 10, 20); err != nil {
			t.Fatalf("AddBedrockUsage: %v", err)
		}
	}
	rows, err := st.ListBedrockUsage(period)
	if err != nil {
		t.Fatalf("ListBedrockUsage: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ServerID == f.server.ID && r.Region == "eu-central" {
			found = true
			if r.RxBytes != 20 || r.TxBytes != 40 || r.ServerUUID != f.server.UUID {
				t.Fatalf("usage row = %+v, want rx 20, tx 40", r)
			}
		}
	}
	if !found {
		t.Fatal("ListBedrockUsage did not return the row just written")
	}

	// The subset column is separate from voice_bytes and from bytes.
	if err := st.AddTrafficTick(f.user.ID, period, 500, 0, []store.TrafficTickCell{
		{Region: "eu-central", Kind: "edge", Product: "byon", Bytes: 500, Voice: 50, Bedrock: 70},
	}); err != nil {
		t.Fatal(err)
	}
	cells, _ := st.GetTrafficUsageRegions(f.user.ID, period)
	if len(cells) != 1 || cells[0].Bytes != 500 || cells[0].VoiceBytes != 50 || cells[0].BedrockBytes != 70 {
		t.Fatalf("cells = %+v, want bytes 500, voice 50, bedrock 70", cells)
	}

	if err := st.DeleteServer(f.server.ID); err != nil {
		t.Fatalf("DeleteServer: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bedrock_usage WHERE server_id = $1`, f.server.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bedrock_usage rows after server delete = %d, err %v, want 0", n, err)
	}
}
