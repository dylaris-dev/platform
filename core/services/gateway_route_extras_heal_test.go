package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/services/remotegw"
	"dylaris-core/store"
)

func assertKeyRepaired(t *testing.T, e *extrasEnv, srv models.Server, d string, key []byte) {
	t.Helper()
	if r := e.row(d); r.Edition != "both" || len(openExtrasOps(e.st)) != 0 {
		t.Fatalf("row %+v, ops %v", r, openExtrasOps(e.st))
	}
	if got := e.st.keys[srv.ID]; !bytes.Equal(got, key) {
		t.Fatalf("stored key %s, want the provider's %s", extrasKeyID(got), extrasKeyID(key))
	}
	if got := e.published[srv.UUID]; !bytes.Equal(got, key) {
		t.Fatalf("published key %s, want %s", extrasKeyID(got), extrasKeyID(key))
	}
	if !strings.Contains(e.logs.String(), "restart server "+srv.UUID) {
		t.Fatalf("no restart line:\n%s", e.logs.String())
	}
	sum := sha256.Sum256(key)
	for _, enc := range []string{hex.EncodeToString(key), hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(key)} {
		if strings.Contains(e.logs.String(), enc) {
			t.Fatal("the log carries the key or its hash")
		}
	}
}

// Review I-1: the column and the provider agree on Bedrock, but the key here
// is missing (the Core died between the provider's yes and storing it) or an
// earlier address's. Floodgate would refuse every Bedrock player while the
// panel shows on; the sweep takes the provider's key, publishes it and asks
// for the restart.
func TestReconcileRouteExtrasRepairsTheKeyWhenBothSidesAgree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(e *extrasEnv, srv models.Server)
	}{
		{"no key stored", func(e *extrasEnv, srv models.Server) { delete(e.st.keys, srv.ID) }},
		{"an earlier address's key", func(e *extrasEnv, srv models.Server) {
			e.st.keys[srv.ID] = bytes.Repeat([]byte{7}, 16)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExtrasEnv(t)
			srv, d := e.addServer(1)
			key := e.bedrockOn(t, srv, d, true)
			tc.spoil(e, srv)
			before := len(e.fake.Calls())
			e.sweep(t)
			if n := extrasWrites(e.fake.Calls()[before:]); n != 0 {
				t.Fatalf("%d provider writes; both sides agree, only the key is read", n)
			}
			assertKeyRepaired(t, e, srv, d, key)
		})
	}
}

// E2E D4: the row here lost its key, but the node's copy already is the
// provider's key (the server runs with it). The row is repaired without a
// restart request on the Errors screen; it would be a restart for nothing.
func TestReconcileRouteExtrasRepairOfTheSameKeyAsksNoRestart(t *testing.T) {
	e := newExtrasEnv(t)
	srv, d := e.addServer(1)
	key := e.bedrockOn(t, srv, d, true)
	delete(e.st.keys, srv.ID)
	e.published[srv.UUID] = append([]byte(nil), key...)
	e.sweep(t)
	if got := e.st.keys[srv.ID]; !bytes.Equal(got, key) {
		t.Fatalf("stored key %s, want the provider's %s", extrasKeyID(got), extrasKeyID(key))
	}
	if strings.Contains(e.logs.String(), "restart server") || !strings.Contains(e.logs.String(), "the server already has it") {
		t.Fatalf("logs:\n%s", e.logs.String())
	}
	sum := sha256.Sum256(key)
	for _, enc := range []string{hex.EncodeToString(key), hex.EncodeToString(sum[:]), base64.StdEncoding.EncodeToString(key)} {
		if strings.Contains(e.logs.String(), enc) {
			t.Fatal("the log carries the key or its hash")
		}
	}
}

// The key check costs one download per Bedrock address per process: once a
// key was found equal, later sweeps send nothing until the row or the key
// changes. Downloads share the sweep's budget.
func TestReconcileRouteExtrasChecksEachKeyOncePerProcess(t *testing.T) {
	e := newExtrasEnv(t)
	for i := 0; i < 25; i++ {
		srv, d := e.addServer(i)
		e.bedrockOn(t, srv, d, true)
	}
	downloads := func() int {
		before := len(e.fake.Calls())
		e.sweep(t)
		n := 0
		for _, c := range e.fake.Calls()[before:] {
			if strings.HasSuffix(c, "/floodgate-key") {
				n++
			}
		}
		return n
	}
	for i, want := range []int{10, 10, 5, 0, 0} {
		if got := downloads(); got != want {
			t.Fatalf("sweep %d: %d key downloads, want %d", i, got, want)
		}
	}
	bedrockKeyChecked.Clear() // a restart or a leader change
	if got := downloads(); got != routeExtrasPerSweep {
		t.Fatalf("after a restart: %d key downloads, want %d", got, routeExtrasPerSweep)
	}
	if ops := openExtrasOps(e.st); len(ops) != 0 || strings.Contains(e.logs.String(), "restart server") {
		t.Fatalf("ops %v, logs:\n%s", ops, e.logs.String())
	}
}

// ageRow makes a row look written long ago, as it is by the next sweep.
func (e *extrasEnv) ageRow(domain string) {
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	r := e.st.routes[domain]
	r.UpdatedAt = time.Now().Add(-time.Hour)
	e.st.routes[domain] = r
}

// Every failure point of Tasks 7 and 8 that leaves the provider on and the
// record here off, reached through the real service path, is switched off at
// the provider by the next sweep: one off item, nothing turned on here, and
// the server's other address keeps its voice (review M-1, M-5).
func TestReconcileRouteExtrasHealsEveryProviderOnLocalOff(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		voice bool
		make  func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string)
	}{
		{"voice: provider yes, local write failed", true, func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string) {
			e.st.voiceSetErr = errors.New("connection reset")
			if _, err := g.EnableServerVoice(ctx, srv.UUID, d, "plasmo"); err == nil {
				t.Fatal("enable succeeded")
			}
		}},
		{"voice: second address refused by the index (review M-1)", true, func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string) {
			e.st.voiceSetErr = store.ErrRemoteVoiceTaken
			if _, err := g.EnableServerVoice(ctx, srv.UUID, d, "plasmo"); !errors.Is(err, store.ErrRemoteVoiceTaken) {
				t.Fatalf("enable: %v", err)
			}
		}},
		{"voice: the answer was lost", true, func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string) {
			e.fake.LoseNextAnswer()
			if _, err := g.EnableServerVoice(ctx, srv.UUID, d, "plasmo"); err == nil {
				t.Fatal("enable succeeded")
			}
		}},
		{"voice: an off whose enqueue failed", true, func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string) {
			if _, err := g.EnableServerVoice(ctx, srv.UUID, d, "plasmo"); err != nil {
				t.Fatal(err)
			}
			e.st.enqueueErr = errors.New("connection reset")
			// Only this address: the other one keeps voice for the M-1 check.
			if _, err := e.st.SetRemoteRouteVoice(d, nil, ""); err != nil {
				t.Fatal(err)
			}
			if err := e.st.EnqueueOutbox(OutboxOpVoice, d, OutboxVoice{}); err == nil {
				t.Fatal("enqueue knob not set")
			}
			e.st.enqueueErr = nil
		}},
		{"Bedrock: key download failed", false, func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string) {
			e.fake.FailAfter(1, 1, 500, "internal", 0)
			if _, err := g.AddBedrock(ctx, srv.ID, srv.UUID, d, true); err == nil {
				t.Fatal("add succeeded")
			}
		}},
		{"Bedrock: storing the key failed", false, func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string) {
			e.st.keySetErr = errors.New("connection reset")
			if _, err := g.AddBedrock(ctx, srv.ID, srv.UUID, d, true); err == nil {
				t.Fatal("add succeeded")
			}
			e.st.keySetErr = nil
		}},
		{"Bedrock: stored but untold, the list could not confirm", false, func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string) {
			e.fake.SetBedrockStoredUntold(true)
			e.fake.SetListSaysOff(true)
			if _, err := g.AddBedrock(ctx, srv.ID, srv.UUID, d, true); err == nil {
				t.Fatal("add succeeded")
			}
			e.fake.SetBedrockStoredUntold(false)
			e.fake.SetListSaysOff(false)
		}},
		{"Bedrock: an off whose enqueue failed", false, func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string) {
			if _, err := g.AddBedrock(ctx, srv.ID, srv.UUID, d, true); err != nil {
				t.Fatal(err)
			}
			e.st.enqueueErr = errors.New("connection reset")
			if err := g.SetEditionWithoutAdding(srv.UUID, d, true, false); err != nil {
				t.Fatal(err)
			}
			e.st.enqueueErr = nil
		}},
		{"Bedrock: the losing half of a double click reset the column (review M-5)", false, func(t *testing.T, e *extrasEnv, g *RemoteGateway, srv models.Server, d string) {
			if _, err := g.AddBedrock(ctx, srv.ID, srv.UUID, d, true); err != nil {
				t.Fatal(err)
			}
			if _, err := e.st.SetRemoteRouteEdition(d, ""); err != nil { // the other request's undo
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newExtrasEnv(t)
			g := NewRemoteGateway(e.st, func() *remotegw.Client { return e.c })
			srv, d := e.addServer(1)
			// A second address of the same server, with voice on here and
			// there: the sweep must leave it alone.
			other := "s00.example.com"
			e.st.mu.Lock()
			e.st.routes[other] = store.RemoteRoute{Domain: other, ServerUUID: srv.UUID, NodeID: 9, Port: 25565}
			e.st.mu.Unlock()
			e.fake.AddRoute(e.kit.LinkID, other, "mc-"+srv.UUID, 25565)
			otherPort := e.voiceOn(t, other)
			tc.make(t, e, g, srv, d)
			e.st.voiceSetErr = nil
			rs, err := e.c.Routes(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var p remotegw.Route
			for _, x := range rs {
				if x.Domain == d {
					p = x
				}
			}
			r := e.row(d)
			if tc.voice && (r.VoicePort != nil || p.Voice == nil || !p.Voice.Enabled) ||
				!tc.voice && (r.Edition != "" || p.Bedrock == nil || !p.Bedrock.Bedrock) {
				t.Fatalf("not the state under test: row %+v, provider %+v %+v", r, p.Voice, p.Bedrock)
			}
			e.ageRow(d)
			e.ageRow(other)
			e.sweep(t)
			want := "voice " + d + " {Enabled:false TargetPort:0}"
			if !tc.voice {
				want = "bedrock " + d + " {Java:true Bedrock:false}"
			}
			if ops := openExtrasOps(e.st); len(ops) != 1 || ops[0] != want {
				t.Fatalf("ops %v, want [%s]", ops, want)
			}
			drainAt(ctx, e.st, e.c, time.Now())
			e.sweep(t)
			if ops := openExtrasOps(e.st); len(ops) != 0 {
				t.Fatalf("after the drain: ops %v", ops)
			}
			if r := e.row(d); r.VoicePort != nil || r.Edition != "" {
				t.Fatalf("the sweep turned something on here: %+v", r)
			}
			if o := e.row(other); o.VoicePort == nil || *o.VoicePort != otherPort {
				t.Fatalf("the other address lost its voice: %+v", o)
			}
		})
	}
}
