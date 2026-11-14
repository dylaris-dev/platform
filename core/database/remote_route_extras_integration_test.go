package database

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-core/store"
)

// keyID names a Floodgate key by length and sha256 prefix: a failure message
// must never carry the key itself.
func keyID(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("len=%d sha256=%s", len(b), hex.EncodeToString(sum[:4]))
}

func extrasDomains(t *testing.T, st *store.PostgresStore, n int) []string {
	t.Helper()
	base := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("x%d-%s.example.com", i, base)
	}
	t.Cleanup(func() {
		for _, d := range out {
			_ = st.DeleteRemoteRoute(d)
		}
	})
	return out
}

func intp(n int) *int { return &n }

// Phase 67 on a database that predates it: an old row reads voice off and
// Java only, both indexes exist, and a second boot is a no-op.
func TestIntegrationRemoteRouteExtrasSchema(t *testing.T) {
	db, st := integrationDB(t)
	d := extrasDomains(t, st, 1)[0]

	for _, q := range []string{
		`DROP INDEX IF EXISTS idx_remote_routes_one_voice`,
		`DROP INDEX IF EXISTS idx_remote_routes_one_bedrock`,
		`ALTER TABLE remote_routes DROP COLUMN IF EXISTS voice_port`,
		`ALTER TABLE remote_routes DROP COLUMN IF EXISTS voice_mod`,
		`ALTER TABLE remote_routes DROP COLUMN IF EXISTS edition`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO remote_routes (domain, server_uuid, node_id, port) VALUES ($1, 'srv-old', 1, 25565)`, d); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := applyRemoteRouteExtras(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	r, err := st.GetRemoteRoute(d)
	if err != nil || r == nil || r.VoicePort != nil || r.VoiceMod != "" || r.Edition != "" {
		t.Fatalf("old row = %+v, %v; want voice off, Java only", r, err)
	}
	var idx int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_indexes WHERE indexname IN ('idx_remote_routes_one_voice', 'idx_remote_routes_one_bedrock')`).Scan(&idx); err != nil || idx != 2 {
		t.Fatalf("index count %d, err %v", idx, err)
	}
}

// Voice: a second address of the same server is refused with the typed error,
// another server is fine, and clearing the first frees the server.
func TestIntegrationRemoteRouteOneVoicePerServer(t *testing.T) {
	_, st := integrationDB(t)
	d := extrasDomains(t, st, 3)
	for i, srv := range []string{"srv-v1", "srv-v1", "srv-v2"} {
		if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d[i], ServerUUID: srv, NodeID: 1, Port: 25565}); err != nil {
			t.Fatal(err)
		}
	}
	if found, err := st.SetRemoteRouteVoice(d[0], intp(24454), "plasmo"); err != nil || !found {
		t.Fatalf("first = %v, %v", found, err)
	}
	if _, err := st.SetRemoteRouteVoice(d[1], intp(24455), "plasmo"); !errors.Is(err, store.ErrRemoteVoiceTaken) {
		t.Fatalf("second address of the server = %v, want ErrRemoteVoiceTaken", err)
	}
	if found, err := st.SetRemoteRouteVoice(d[2], intp(24456), "voicechat"); err != nil || !found {
		t.Fatalf("other server = %v, %v", found, err)
	}
	if found, err := st.SetRemoteRouteVoice(d[0], nil, ""); err != nil || !found {
		t.Fatalf("clear = %v, %v", found, err)
	}
	if found, err := st.SetRemoteRouteVoice(d[1], intp(24455), "plasmo"); err != nil || !found {
		t.Fatalf("after clearing the first = %v, %v", found, err)
	}
	r, err := st.GetRemoteRoute(d[1])
	if err != nil || r == nil || r.VoicePort == nil || *r.VoicePort != 24455 || r.VoiceMod != "plasmo" {
		t.Fatalf("read back = %+v, %v", r, err)
	}
	if found, err := st.SetRemoteRouteVoice("missing-"+d[0], intp(1), "plasmo"); err != nil || found {
		t.Fatalf("missing domain = %v, %v; want not found", found, err)
	}
}

// Bedrock: the same rule on the edition.
func TestIntegrationRemoteRouteOneBedrockPerServer(t *testing.T) {
	_, st := integrationDB(t)
	d := extrasDomains(t, st, 3)
	for i, srv := range []string{"srv-b1", "srv-b1", "srv-b2"} {
		if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d[i], ServerUUID: srv, NodeID: 1, Port: 25565}); err != nil {
			t.Fatal(err)
		}
	}
	if found, err := st.SetRemoteRouteEdition(d[0], "both"); err != nil || !found {
		t.Fatalf("first = %v, %v", found, err)
	}
	if _, err := st.SetRemoteRouteEdition(d[1], "bedrock"); !errors.Is(err, store.ErrRemoteBedrockTaken) {
		t.Fatalf("second address of the server = %v, want ErrRemoteBedrockTaken", err)
	}
	// Java only on a second address is not Bedrock and is always fine.
	if found, err := st.SetRemoteRouteEdition(d[1], ""); err != nil || !found {
		t.Fatalf("Java only on the second = %v, %v", found, err)
	}
	if found, err := st.SetRemoteRouteEdition(d[2], "bedrock"); err != nil || !found {
		t.Fatalf("other server = %v, %v", found, err)
	}
	if found, err := st.SetRemoteRouteEdition(d[0], ""); err != nil || !found {
		t.Fatalf("clear = %v, %v", found, err)
	}
	if found, err := st.SetRemoteRouteEdition(d[1], "bedrock"); err != nil || !found {
		t.Fatalf("after clearing the first = %v, %v", found, err)
	}
	if r, err := st.GetRemoteRoute(d[1]); err != nil || r == nil || r.Edition != "bedrock" {
		t.Fatalf("read back = %+v, %v", r, err)
	}
	if _, err := st.SetRemoteRouteEdition(d[2], "none"); err == nil {
		t.Fatal("edition outside '', both, bedrock was stored")
	}
}

// Two writers racing for the two addresses of one server: exactly one wins,
// the other gets the typed error. Without the partial unique indexes both win.
func TestIntegrationRemoteRouteExtrasRace(t *testing.T) {
	_, st := integrationDB(t)
	d := extrasDomains(t, st, 2)
	for _, dom := range d {
		if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: dom, ServerUUID: "srv-race", NodeID: 1, Port: 25565}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		set   func(domain string, on bool) error
		taken error
	}{
		{"voice", func(dom string, on bool) error {
			var p *int
			if on {
				p = intp(24454)
			}
			_, err := st.SetRemoteRouteVoice(dom, p, "plasmo")
			return err
		}, store.ErrRemoteVoiceTaken},
		{"bedrock", func(dom string, on bool) error {
			e := ""
			if on {
				e = "both"
			}
			_, err := st.SetRemoteRouteEdition(dom, e)
			return err
		}, store.ErrRemoteBedrockTaken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for round := 0; round < 20; round++ {
				var wg sync.WaitGroup
				start := make(chan struct{})
				errs := make([]error, 2)
				for i := range d {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						errs[i] = tc.set(d[i], true)
					}(i)
				}
				close(start)
				wg.Wait()
				won, lost := 0, 0
				for _, err := range errs {
					switch {
					case err == nil:
						won++
					case errors.Is(err, tc.taken):
						lost++
					default:
						t.Fatalf("round %d: unexpected error %v", round, err)
					}
				}
				if won != 1 || lost != 1 {
					t.Fatalf("round %d: %d won, %d refused; want exactly one each", round, won, lost)
				}
				for _, dom := range d {
					if err := tc.set(dom, false); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

// A move or a real-IP change replayed through UpsertRemoteRoute, and a claim
// on a held domain, leave the voice and edition columns alone.
func TestIntegrationUpsertKeepsVoiceAndEdition(t *testing.T) {
	_, st := integrationDB(t)
	d := extrasDomains(t, st, 1)[0]
	if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-k", NodeID: 1, Port: 25565}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRemoteRouteVoice(d, intp(24460), "plasmo"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRemoteRouteEdition(d, "both"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-k", NodeID: 2, Port: 25570, ProxyProtocol: true}); err != nil {
		t.Fatal(err)
	}
	if claimed, _, err := st.ClaimRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-k", NodeID: 3, Port: 25580}); err != nil || claimed {
		t.Fatalf("claim on a held domain = %v, %v", claimed, err)
	}
	r, err := st.GetRemoteRoute(d)
	if err != nil || r == nil {
		t.Fatalf("get = %+v, %v", r, err)
	}
	if r.NodeID != 2 || r.VoicePort == nil || *r.VoicePort != 24460 || r.VoiceMod != "plasmo" || r.Edition != "both" {
		t.Fatalf("after upsert and claim = %+v (voice %v)", r, r.VoicePort)
	}
}

// TouchRemoteRoute moves updated_at and nothing else.
func TestIntegrationRemoteRouteTouch(t *testing.T) {
	db, st := integrationDB(t)
	d := extrasDomains(t, st, 1)[0]
	if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-t", NodeID: 1, Port: 25565}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE remote_routes SET updated_at = NOW() - INTERVAL '1 hour' WHERE domain = $1`, d); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetRemoteRoute(d)
	if err := st.TouchRemoteRoute(d); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetRemoteRoute(d)
	if !after.UpdatedAt.After(before.UpdatedAt) || after.NodeID != before.NodeID || after.Port != before.Port {
		t.Fatalf("touch: before %+v, after %+v", before, after)
	}
}

// The row's age comes from the database's own clock (review M-3): the sweep's
// "written moments ago" never compares a replica's clock with the database's.
func TestIntegrationRemoteRouteAgeIsTheDatabases(t *testing.T) {
	db, st := integrationDB(t)
	d := extrasDomains(t, st, 1)[0]
	if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: d, ServerUUID: "srv-age", NodeID: 1, Port: 25565}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.GetRemoteRoute(d)
	if !fresh.AgeKnown || fresh.Age < 0 || fresh.Age > 10*time.Second {
		t.Fatalf("just written: age %v known %v", fresh.Age, fresh.AgeKnown)
	}
	if _, err := db.Exec(`UPDATE remote_routes SET updated_at = NOW() - INTERVAL '1 hour' WHERE domain = $1`, d); err != nil {
		t.Fatal(err)
	}
	old, _ := st.GetRemoteRoute(d)
	if old.Age < 59*time.Minute || old.Age > 61*time.Minute {
		t.Fatalf("an hour old: age %v", old.Age)
	}
	rows, err := st.ListRemoteRoutes()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Domain == d && (!r.AgeKnown || r.Age < 59*time.Minute) {
			t.Fatalf("list: age %v known %v", r.Age, r.AgeKnown)
		}
	}
}

// SetBedrockKey replaces, reports whether the bytes changed, and stores
// ciphertext.
func TestIntegrationSetBedrockKeyReplaces(t *testing.T) {
	db, st := integrationDB(t)
	st.SetBedrockEncryptionKey("cluster-secret-a")
	f := newFixture(t, st)

	first := bytes.Repeat([]byte{0x11}, 16)
	second := bytes.Repeat([]byte{0x22}, 16)
	steps := []struct {
		name        string
		key         []byte
		wantChanged bool
	}{
		{"first write", first, true},
		{"same bytes", first, false},
		{"new bytes", second, true},
		{"new bytes again", second, false},
	}
	for _, s := range steps {
		changed, err := st.SetBedrockKey(f.server.ID, s.key)
		if err != nil || changed != s.wantChanged {
			t.Fatalf("%s: changed %v, %v; want %v", s.name, changed, err, s.wantChanged)
		}
		got, ok, err := st.GetBedrockKey(f.server.ID)
		if err != nil || !ok || !bytes.Equal(got, s.key) {
			t.Fatalf("%s: stored key %s ok %v %v; want %s", s.name, keyID(got), ok, err, keyID(s.key))
		}
		var enc string
		if err := db.QueryRow(`SELECT floodgate_key_enc FROM bedrock_servers WHERE server_id = $1`, f.server.ID).Scan(&enc); err != nil {
			t.Fatal(err)
		}
		if enc == "" || bytes.Contains([]byte(enc), s.key) || strings.Contains(enc, hex.EncodeToString(s.key)) {
			t.Fatalf("%s: the stored column is not ciphertext (len %d)", s.name, len(enc))
		}
	}
	// An unreadable stored key is replaced, not a dead end.
	if _, err := db.Exec(`UPDATE bedrock_servers SET floodgate_key_enc = 'garbage' WHERE server_id = $1`, f.server.ID); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.SetBedrockKey(f.server.ID, first); err != nil || !changed {
		t.Fatalf("over an unreadable key: changed %v, %v", changed, err)
	}
	if got, _, err := st.GetBedrockKey(f.server.ID); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("after replacing an unreadable key: %s, %v", keyID(got), err)
	}
}

// EnableBedrock, which local mode uses, still keeps the first key.
func TestIntegrationEnableBedrockStillKeepsTheFirstKey(t *testing.T) {
	_, st := integrationDB(t)
	st.SetBedrockEncryptionKey("cluster-secret-a")
	f := newFixture(t, st)
	first := bytes.Repeat([]byte{0x33}, 16)
	if _, err := st.SetBedrockKey(f.server.ID, first); err != nil {
		t.Fatal(err)
	}
	if err := st.EnableBedrock(f.server.ID, bytes.Repeat([]byte{0x44}, 16)); err != nil {
		t.Fatal(err)
	}
	if got, _, err := st.GetBedrockKey(f.server.ID); err != nil || !bytes.Equal(got, first) {
		t.Fatalf("EnableBedrock replaced the key: %s, %v; want %s", keyID(got), err, keyID(first))
	}
}

// A dev database that booted the first cut of phase 67 has the looser Bedrock
// index (any non-empty edition) and no CHECK: the next boot rebuilds the index with the
// two Bedrock values, adds the CHECK, and a third boot changes nothing.
func TestIntegrationRemoteRouteExtrasTightensTheBedrockIndex(t *testing.T) {
	db, st := integrationDB(t)
	d := extrasDomains(t, st, 2)
	for _, q := range []string{
		`DROP INDEX IF EXISTS idx_remote_routes_one_bedrock`,
		`ALTER TABLE remote_routes DROP CONSTRAINT IF EXISTS remote_routes_edition_check`,
		`CREATE UNIQUE INDEX idx_remote_routes_one_bedrock ON remote_routes (server_uuid) WHERE edition <> ''`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := applyRemoteRouteExtras(db); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	var def string
	if err := db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_remote_routes_one_bedrock'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(def, "'both'") || !strings.Contains(def, "'bedrock'") || !strings.Contains(def, "UNIQUE") {
		t.Fatalf("Bedrock index = %s, want UNIQUE ... WHERE edition IN ('both', 'bedrock')", def)
	}
	var checks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_constraint WHERE conname = 'remote_routes_edition_check'`).Scan(&checks); err != nil || checks != 1 {
		t.Fatalf("edition CHECK count %d, err %v", checks, err)
	}
	for _, dom := range d {
		if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: dom, ServerUUID: "srv-chk", NodeID: 1, Port: 25565}); err != nil {
			t.Fatal(err)
		}
	}
	// The CHECK holds even for a writer that bypasses the store's own check.
	if _, err := db.Exec(`UPDATE remote_routes SET edition = 'none' WHERE domain = $1`, d[0]); err == nil {
		t.Fatal("edition 'none' was stored past the CHECK")
	}
	// The index still answers with the typed error.
	if _, err := st.SetRemoteRouteEdition(d[0], "both"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRemoteRouteEdition(d[1], "bedrock"); !errors.Is(err, store.ErrRemoteBedrockTaken) {
		t.Fatalf("second Bedrock address = %v, want ErrRemoteBedrockTaken", err)
	}
}

// A unique violation is mapped by the index it hit: a voice write that hits
// the Bedrock index (or anything else) must not read as "voice taken".
func TestIntegrationRemoteRouteExtrasMapByIndexName(t *testing.T) {
	db, st := integrationDB(t)
	d := extrasDomains(t, st, 2)
	for _, dom := range d {
		if err := st.UpsertRemoteRoute(store.RemoteRoute{Domain: dom, ServerUUID: "srv-name", NodeID: 1, Port: 25565}); err != nil {
			t.Fatal(err)
		}
	}
	// A unique index on voice_mod stands in for "another constraint the
	// voice UPDATE can hit".
	if _, err := db.Exec(`CREATE UNIQUE INDEX dy_test_one_voice_mod ON remote_routes (voice_mod) WHERE voice_mod = 'unique-mod'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DROP INDEX IF EXISTS dy_test_one_voice_mod`) })
	if _, err := st.SetRemoteRouteVoice(d[0], nil, "unique-mod"); err != nil {
		t.Fatal(err)
	}
	_, err := st.SetRemoteRouteVoice(d[1], nil, "unique-mod")
	if err == nil || errors.Is(err, store.ErrRemoteVoiceTaken) || errors.Is(err, store.ErrRemoteBedrockTaken) {
		t.Fatalf("violation of another index = %v, want a plain error, not a remote-route sentinel", err)
	}
	if _, err := st.SetRemoteRouteVoice(d[0], intp(24454), "svc"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetRemoteRouteVoice(d[1], intp(24455), "svc"); !errors.Is(err, store.ErrRemoteVoiceTaken) {
		t.Fatalf("voice index violation = %v, want ErrRemoteVoiceTaken", err)
	}
}
