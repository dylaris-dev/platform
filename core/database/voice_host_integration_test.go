package database

import (
	"bytes"
	"testing"

	"dylaris-core/store"
)

// Phase 69 on a database that predates it: an old route-only voice row and an
// old remote route read an empty host (the route's own domain, as today), and a
// second boot is a no-op.
func TestIntegrationVoiceHostSchema(t *testing.T) {
	db, st := integrationDB(t)
	rt := linkRoute(t, st)
	d := extrasDomains(t, st, 1)[0]
	region := voiceRegion()

	for _, q := range []string{
		`ALTER TABLE link_route_voice DROP COLUMN IF EXISTS host`,
		`ALTER TABLE remote_routes DROP COLUMN IF EXISTS voice_host`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO link_route_voice (domain, region, port, target_port, uplink_key) VALUES ($1, $2, 26500, 24454, $3)`,
		rt.Domain, region, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO remote_routes (domain, server_uuid, node_id, port, voice_port, voice_mod) VALUES ($1, 'srv-old', 1, 25565, 24454, 'plasmo')`, d); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := applyVoiceHost(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	v, err := st.GetLinkRouteVoice(rt.Domain)
	if err != nil || v == nil || v.Host != "" || v.Port != 26500 || v.Region != region {
		t.Fatalf("old route voice row = %+v, %v; want host '' and the port and region it had", v, err)
	}
	r, err := st.GetRemoteRoute(d)
	if err != nil || r == nil || r.VoiceHost != "" || r.VoicePort == nil || *r.VoicePort != 24454 || r.VoiceMod != "plasmo" {
		t.Fatalf("old remote route = %+v, %v; want voice_host '' and the voice it had", r, err)
	}
}

// Same region again keeps the port and key and takes the new host; a region
// change reallocates and writes the host on the fresh row.
func TestIntegrationAllocateLinkRouteVoiceKeepsPortAndSetsHost(t *testing.T) {
	_, st := integrationDB(t)
	region := voiceRegion()
	rt := linkRoute(t, st)
	key := bytes.Repeat([]byte{0x42}, 32)
	first, err := st.AllocateLinkRouteVoice(rt.Domain, region, "", 24454, key, 26000, 26999)
	if err != nil || first.Host != "" {
		t.Fatalf("first = %+v, %v; want host ''", first, err)
	}
	again, err := st.AllocateLinkRouteVoice(rt.Domain, region, "voice.eu.example.test", 24455, bytes.Repeat([]byte{0x99}, 32), 26000, 26999)
	if err != nil {
		t.Fatal(err)
	}
	if again.Port != first.Port || !bytes.Equal(again.UplinkKey, key) || again.TargetPort != 24455 || again.Host != "voice.eu.example.test" {
		t.Fatalf("same region = %+v, want port %d, the first key, target 24455, the new host", again, first.Port)
	}
	if got, err := st.GetLinkRouteVoice(rt.Domain); err != nil || got == nil || got.Host != "voice.eu.example.test" {
		t.Fatalf("read back = %+v, %v", got, err)
	}
	moved, err := st.AllocateLinkRouteVoice(rt.Domain, region+"x", "voice.us.example.test", 24455, key, 26000, 26999)
	if err != nil || moved.Region != region+"x" || moved.Host != "voice.us.example.test" {
		t.Fatalf("other region = %+v, %v; want the new region and host", moved, err)
	}
	back, err := st.AllocateLinkRouteVoice(rt.Domain, region+"x", "", 24455, key, 26000, 26999)
	if err != nil || back.Host != "" || back.Port != moved.Port {
		t.Fatalf("host cleared = %+v, %v; want host '' on the same port", back, err)
	}
}

// An older Core replica still running mid-deploy writes in the shape it knows:
// its insert reads an empty host (the address, as before), and its same-region
// update and its remote voice write leave a host the new Core set alone.
func TestIntegrationVoiceHostOldReplicaWrites(t *testing.T) {
	db, st := integrationDB(t)
	region := voiceRegion()
	rt := linkRoute(t, st)
	if _, err := db.Exec(`INSERT INTO link_route_voice (domain, region, port, target_port, uplink_key) VALUES ($1, $2, 26600, 24454, $3)`,
		rt.Domain, region, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if v, err := st.GetLinkRouteVoice(rt.Domain); err != nil || v == nil || v.Host != "" {
		t.Fatalf("old-shape insert = %+v, %v; want host ''", v, err)
	}
	if _, err := st.AllocateLinkRouteVoice(rt.Domain, region, "voice.eu.example.test", 24454, make([]byte, 32), 26000, 26999); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE link_route_voice SET target_port = $2 WHERE domain = $1`, rt.Domain, 24460); err != nil {
		t.Fatal(err)
	}
	if v, err := st.GetLinkRouteVoice(rt.Domain); err != nil || v == nil || v.Host != "voice.eu.example.test" || v.TargetPort != 24460 {
		t.Fatalf("after an old-shape update = %+v, %v; want the host kept", v, err)
	}

	d := extrasDomains(t, st, 1)[0]
	if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-old-rep", NodeID: 1, Port: 25565}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRemoteRouteVoice(d, intp(24480), "plasmo", "voice.eu.example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE remote_routes SET voice_port = $2, voice_mod = $3, updated_at = NOW() WHERE domain = $1`, d, 24481, "plasmo"); err != nil {
		t.Fatal(err)
	}
	if r, err := st.GetRemoteRoute(d); err != nil || r == nil || r.VoiceHost != "voice.eu.example.test" || *r.VoicePort != 24481 {
		t.Fatalf("after an old-shape remote voice write = %+v, %v", r, err)
	}
}

// The provider-named host is set with the voice port, cleared with it, and
// an upsert or a claim (a move, a real-IP change) leaves it alone.
func TestIntegrationRemoteRouteVoiceHost(t *testing.T) {
	_, st := integrationDB(t)
	d := extrasDomains(t, st, 1)[0]
	if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-vh", NodeID: 1, Port: 25565}); err != nil {
		t.Fatal(err)
	}
	if found, err := st.SetRemoteRouteVoice(d, intp(24470), "plasmo", "voice.eu.example.test"); err != nil || !found {
		t.Fatalf("set = %v, %v", found, err)
	}
	if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-vh", NodeID: 2, Port: 25570}); err != nil {
		t.Fatal(err)
	}
	if claimed, _, err := st.ClaimRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-vh", NodeID: 3, Port: 25580}); err != nil || claimed {
		t.Fatalf("claim on a held domain = %v, %v", claimed, err)
	}
	r, err := st.GetRemoteRoute(d)
	if err != nil || r == nil || r.NodeID != 2 || r.VoiceHost != "voice.eu.example.test" || r.VoicePort == nil || *r.VoicePort != 24470 {
		t.Fatalf("after upsert and claim = %+v, %v", r, err)
	}
	rows, err := st.ListRemoteRoutes()
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, row := range rows {
		listed = listed || (row.Domain == d && row.VoiceHost == "voice.eu.example.test")
	}
	if !listed {
		t.Fatal("the list does not carry the voice host")
	}
	if found, err := st.SetRemoteRouteVoice(d, nil, "", ""); err != nil || !found {
		t.Fatalf("clear = %v, %v", found, err)
	}
	if r, err := st.GetRemoteRoute(d); err != nil || r == nil || r.VoiceHost != "" || r.VoicePort != nil {
		t.Fatalf("after clear = %+v, %v", r, err)
	}
}
