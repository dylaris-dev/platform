package services

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"dylaris-core/models"
	"dylaris-core/store"
)

func providerKeyOf(t *testing.T, e *extrasEnv, domain string) []byte {
	t.Helper()
	k, err := e.c.FloodgateKey(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Review C-1: the sweep only looks at an address the provider listed moments
// ago, so a 404 on the re-assert (or on the key right after a good PUT)
// contradicts that list: it is the provider failing, never "gone". Nothing is
// switched off, however many sweeps it lasts.
func TestReconcileRouteExtrasNotFoundIsNeverFinal(t *testing.T) {
	for _, voice := range []bool{true, false} {
		t.Run(fmt.Sprintf("voice %v", voice), func(t *testing.T) {
			e := newExtrasEnv(t)
			srv, d := e.addServer(1)
			if voice {
				e.voiceOn(t, d)
			} else {
				e.bedrockOn(t, srv, d, true)
			}
			e.fake.SetListSaysOff(true)
			for i := 0; i < 4; i++ {
				e.fake.FailAfter(1, 1, 404, "not_found", 0)
				e.sweep(t)
				e.later(outboxSweepEvery)
			}
			if !voice {
				// The PUT passes, the key read right after it says 404.
				for i := 0; i < 3; i++ {
					e.fake.FailAfter(2, 1, 404, "not_found", 0)
					e.sweep(t)
					e.later(outboxSweepEvery)
				}
			}
			if r := e.row(d); (voice && r.VoicePort == nil) || (!voice && r.Edition == "") || len(openExtrasOps(e.st)) != 0 {
				t.Fatalf("a 404 switched it off: row %+v, ops %v", r, openExtrasOps(e.st))
			}
		})
	}
}

// Second guard: the same final answer in two sweeps a full interval apart,
// for the unchanged row. One answer does nothing; a second one too soon does
// nothing; an answer in between that is not the same one starts over.
func TestReconcileRouteExtrasNeedsTheSameFinalAnswerTwice(t *testing.T) {
	setup := func(t *testing.T) (*extrasEnv, string) {
		e := newExtrasEnv(t)
		_, d := e.addServer(1)
		e.voiceOn(t, d)
		e.fake.DropRouteExtras(d)
		e.fake.SetVoiceRefusal("voice_not_allowed")
		return e, d
	}
	on := func(e *extrasEnv, d string) bool { return e.row(d).VoicePort != nil && len(openExtrasOps(e.st)) == 0 }

	t.Run("one answer, then one too soon", func(t *testing.T) {
		e, d := setup(t)
		e.sweep(t)
		e.later(outboxSweepEvery - time.Second)
		e.sweep(t)
		if !on(e, d) {
			t.Fatalf("switched off before a full interval: %+v", e.row(d))
		}
		e.later(time.Second)
		e.sweep(t)
		if on(e, d) {
			t.Fatal("the same answer a full interval apart did not switch it off")
		}
	})
	t.Run("a retryable answer in between starts over", func(t *testing.T) {
		e, d := setup(t)
		e.sweep(t)
		e.later(outboxSweepEvery)
		e.fake.FailAfter(1, 1, 500, "internal", 0)
		e.sweep(t)
		e.later(outboxSweepEvery)
		e.sweep(t) // the first strike again
		if !on(e, d) {
			t.Fatal("a 500 in between did not reset the count")
		}
		e.later(outboxSweepEvery)
		e.sweep(t)
		if on(e, d) {
			t.Fatal("two answers after the reset did not switch it off")
		}
	})
	t.Run("a row written in between starts over", func(t *testing.T) {
		e, d := setup(t)
		e.sweep(t)
		e.st.mu.Lock()
		r := e.st.routes[d]
		r.UpdatedAt = time.Now().Add(-time.Minute) // written since, long enough ago
		e.st.routes[d] = r
		e.st.mu.Unlock()
		e.later(outboxSweepEvery)
		e.sweep(t)
		if !on(e, d) {
			t.Fatal("an answer about the old row counted for the new one")
		}
	})
}

// Review I-1: a queued off is stale once the row has the feature again (every
// user off clears the column before it queues, every user on is refused while
// an item is open). The drain finishes it without telling the provider.
func TestOutboxDrainSkipsAStaleOff(t *testing.T) {
	ctx := context.Background()
	e := newExtrasEnv(t)
	srv, dv := e.addServer(1)
	port := e.voiceOn(t, dv)
	_, db := e.addServer(2)
	e.bedrockOn(t, srv, db, true)
	_, dOff := e.addServer(3)
	e.voiceOn(t, dOff)
	e.st.mu.Lock()
	r := e.st.routes[dOff]
	r.VoicePort = nil // a real user off: column first
	e.st.routes[dOff] = r
	e.st.mu.Unlock()
	for _, it := range []struct {
		op, d string
		p     any
	}{
		{OutboxOpVoice, dv, OutboxVoice{Enabled: false}},
		{OutboxOpBedrock, db, OutboxBedrock{Java: true, Bedrock: false}},
		{OutboxOpVoice, dOff, OutboxVoice{Enabled: false}},
	} {
		if err := e.st.EnqueueOutbox(it.op, it.d, it.p); err != nil {
			t.Fatal(err)
		}
	}
	before := len(e.fake.Calls())
	drainAt(ctx, e.st, e.c, time.Now())
	calls := strings.Join(e.fake.Calls()[before:], ",")
	if calls != "PUT /routes/"+dOff+"/voice" || len(openExtrasOps(e.st)) != 0 {
		t.Fatalf("calls %s, open %v", calls, openExtrasOps(e.st))
	}
	rs, err := e.c.Routes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rs {
		if p.Domain == dv && (p.Voice == nil || !p.Voice.Enabled || p.Voice.Port != port) {
			t.Fatalf("the provider lost the voice of %s: %+v", dv, p.Voice)
		}
	}
}

// Review mutation 1 / M-6: the "provider on, local off" rules decide on the
// FRESH row. Between the row read and the decision the user turned the
// feature on (the provider holds it): nothing is queued.
func TestReconcileRouteExtrasProviderOnDecidesOnTheFreshRow(t *testing.T) {
	for _, voice := range []bool{true, false} {
		t.Run(fmt.Sprintf("voice %v", voice), func(t *testing.T) {
			e := newExtrasEnv(t)
			srv, d := e.addServer(1)
			if voice {
				e.voiceOn(t, d)
			} else {
				e.bedrockOn(t, srv, d, true)
			}
			// The listed row says off; the fresh one says on again.
			e.st.mu.Lock()
			r := e.st.routes[d]
			listed := r
			listed.VoicePort, listed.Edition = nil, ""
			e.st.routes[d] = listed
			e.st.mu.Unlock()
			e.st.afterListRoutes = func() {
				e.st.mu.Lock()
				defer e.st.mu.Unlock()
				r.UpdatedAt = time.Now().Add(-time.Hour)
				e.st.routes[d] = r
				e.st.afterListRoutes = nil
			}
			e.sweep(t)
			if ops := openExtrasOps(e.st); len(ops) != 0 {
				t.Fatalf("queued %v from the listed row", ops)
			}
		})
	}
}

// The interleaving the drain guard closes: the sweep queued the off, then the
// user's ON (its preflight ran before the item existed) recorded the port.
// The drain must not take the provider's voice away.
func TestReconcileRouteExtrasInterleavedOnSurvivesTheDrain(t *testing.T) {
	ctx := context.Background()
	e := newExtrasEnv(t)
	_, d := e.addServer(1)
	v, err := e.c.SetVoice(ctx, d, true, VoiceInternalPort) // provider on, local off
	if err != nil {
		t.Fatal(err)
	}
	e.sweep(t)
	if ops := openExtrasOps(e.st); len(ops) != 1 {
		t.Fatalf("ops %v", ops)
	}
	e.setPort(d, v.Port) // the user's ON lands now
	before := len(e.fake.Calls())
	drainAt(ctx, e.st, e.c, time.Now())
	if n := extrasWrites(e.fake.Calls()[before:]); n != 0 || len(openExtrasOps(e.st)) != 0 {
		t.Fatalf("the drain sent %d writes, open %v", n, openExtrasOps(e.st))
	}
}

// Review I-2: ten addresses that keep getting no answer cannot hold the
// budget: with 30 mismatching addresses every one is tried within three
// sweeps.
func TestReconcileRouteExtrasRotatesTheWalk(t *testing.T) {
	e := newExtrasEnv(t)
	for i := 0; i < 30; i++ {
		_, d := e.addServer(i)
		e.voiceOn(t, d)
		e.fake.DropRouteExtras(d)
	}
	e.fake.SetVoiceRefusal("no_relay")
	tried := map[string]bool{}
	for i := 0; i < 3; i++ {
		before := len(e.fake.Calls())
		e.sweep(t)
		for _, c := range e.fake.Calls()[before:] {
			if strings.HasSuffix(c, "/voice") {
				tried[strings.TrimSuffix(strings.TrimPrefix(c, "PUT /routes/"), "/voice")] = true
			}
		}
		e.later(outboxSweepEvery)
	}
	if len(tried) != 30 {
		t.Fatalf("%d of 30 addresses tried in three sweeps", len(tried))
	}
}

// Review M-2: after a restart the key checks go to the rows that need them
// most: no stored key first, then the newest rows.
func TestReconcileRouteExtrasChecksMissingKeysFirst(t *testing.T) {
	e := newExtrasEnv(t)
	base := time.Now().Add(-2 * time.Hour)
	var newest string
	for i := 0; i < 15; i++ {
		srv, d := e.addServer(i)
		e.bedrockOn(t, srv, d, true)
		e.st.mu.Lock()
		r := e.st.routes[d]
		r.UpdatedAt = base.Add(time.Duration(i) * time.Minute)
		e.st.routes[d] = r
		e.st.mu.Unlock()
		if i == 12 {
			newest = d
		}
		if i >= 13 {
			delete(e.st.keys, srv.ID) // the two last in domain order
		}
	}
	before := len(e.fake.Calls())
	e.sweep(t)
	got := map[string]bool{}
	for _, c := range e.fake.Calls()[before:] {
		if strings.HasSuffix(c, "/floodgate-key") {
			got[strings.TrimSuffix(strings.TrimPrefix(c, "GET /routes/"), "/floodgate-key")] = true
		}
	}
	for _, d := range []string{"s13.example.com", "s14.example.com", newest} {
		if !got[d] {
			t.Fatalf("%s not checked in the first sweep: %v", d, got)
		}
	}
	for _, i := range []int{13, 14} {
		if _, ok := e.st.keys[100+i]; !ok {
			t.Fatalf("s%d: missing key not repaired in the first sweep", i)
		}
	}
}

// Review M-3: "written moments ago" is the database's own age of the row, not
// the replica's clock against the database's.
func TestReconcileRouteExtrasAgeIsTheDatabases(t *testing.T) {
	e := newExtrasEnv(t)
	_, d := e.addServer(1)
	e.voiceOn(t, d)
	e.fake.DropRouteExtras(d)
	e.st.mu.Lock()
	r := e.st.routes[d]
	r.UpdatedAt = time.Now().Add(-time.Hour) // this replica's clock says old
	r.Age, r.AgeKnown = 5*time.Second, true  // the database says just written
	e.st.routes[d] = r
	e.st.mu.Unlock()
	before := len(e.fake.Calls())
	e.sweep(t)
	if n := extrasWrites(e.fake.Calls()[before:]); n != 0 {
		t.Fatalf("%d writes for a row the database wrote 5 s ago", n)
	}
	if !writtenMomentsAgo(store.RemoteRoute{Age: 5 * time.Second, AgeKnown: true, UpdatedAt: time.Now().Add(-time.Hour)}) ||
		writtenMomentsAgo(store.RemoteRoute{Age: time.Hour, AgeKnown: true, UpdatedAt: time.Now()}) {
		t.Fatal("writtenMomentsAgo reads the replica's clock")
	}
}

// Re-review N-1: a strike is only "the same answer twice" while nothing in
// between showed the provider healthy, and only for a bounded time. Two
// final answers one interval apart still switch off; a healthy sweep or a
// long silence in between makes the next answer a first one again.
func TestReconcileRouteExtrasStrikeIsForgottenWhenHealthy(t *testing.T) {
	type feature struct {
		name   string
		strike func(t *testing.T, e *extrasEnv, srv models.Server, d string) // provider says off and refuses finally
		heal   func(t *testing.T, e *extrasEnv, srv models.Server, d string) // provider agrees with the row again
		on     func(e *extrasEnv, d string) bool
	}
	features := []feature{
		{"voice",
			func(t *testing.T, e *extrasEnv, _ models.Server, d string) {
				e.fake.DropRouteExtras(d)
				e.fake.SetVoiceRefusal("voice_not_allowed")
			},
			func(t *testing.T, e *extrasEnv, _ models.Server, d string) {
				e.fake.SetVoiceRefusal("")
				e.voiceOn(t, d)
			},
			func(e *extrasEnv, d string) bool { return e.row(d).VoicePort != nil },
		},
		{"Bedrock",
			func(t *testing.T, e *extrasEnv, _ models.Server, d string) {
				e.fake.DropRouteExtras(d)
				e.fake.SetBedrockRefusal("bedrock_disabled")
			},
			func(t *testing.T, e *extrasEnv, srv models.Server, d string) {
				e.fake.SetBedrockRefusal("")
				e.bedrockOn(t, srv, d, true)
			},
			func(e *extrasEnv, d string) bool { return e.row(d).Edition != "" },
		},
	}
	for _, f := range features {
		for _, tc := range []struct {
			name    string
			between func(t *testing.T, e *extrasEnv, srv models.Server, d string)
			wantOn  bool
		}{
			{"two answers one interval apart", func(t *testing.T, e *extrasEnv, _ models.Server, _ string) { e.later(outboxSweepEvery) }, false},
			{"a healthy sweep in between", func(t *testing.T, e *extrasEnv, srv models.Server, d string) {
				f.heal(t, e, srv, d)
				e.sweep(t)
				e.later(outboxSweepEvery)
			}, true},
			{"silence longer than the age limit", func(t *testing.T, e *extrasEnv, _ models.Server, _ string) {
				e.later(extrasVerdictMaxAge + time.Second)
			}, true},
		} {
			t.Run(f.name+", "+tc.name, func(t *testing.T) {
				e := newExtrasEnv(t)
				srv, d := e.addServer(1)
				if f.name == "voice" {
					e.voiceOn(t, d)
				} else {
					e.bedrockOn(t, srv, d, true)
				}
				f.strike(t, e, srv, d)
				e.sweep(t) // the first final answer
				tc.between(t, e, srv, d)
				if tc.name == "a healthy sweep in between" {
					f.strike(t, e, srv, d) // the provider is at fault again
				}
				e.sweep(t) // one final answer
				if got := f.on(e, d) && len(openExtrasOps(e.st)) == 0; got != tc.wantOn {
					t.Fatalf("still on = %v, want %v (row %+v, ops %v)", got, tc.wantOn, e.row(d), openExtrasOps(e.st))
				}
			})
		}
	}
}

// Re-review M-b: the Bedrock rows taken first (no key, key not compared yet)
// never fill the whole budget, so rows whose check keeps failing cannot hold
// the rest of the addresses back.
func TestOrderForExtrasLeavesRoomForTheRest(t *testing.T) {
	e := newExtrasEnv(t)
	servers := map[string]models.Server{}
	for i := 50; i < 50+routeExtrasPerSweep+2; i++ {
		srv, d := e.addServer(i)
		servers[srv.UUID] = srv
		e.st.mu.Lock()
		r := e.st.routes[d]
		r.Edition = "both" // Bedrock on, no key stored here
		e.st.routes[d] = r
		e.st.mu.Unlock()
	}
	for i := 1; i <= 3; i++ {
		srv, _ := e.addServer(i)
		servers[srv.UUID] = srv
	}
	e.st.mu.Lock()
	var rows []store.RemoteRoute
	for _, r := range e.st.routes {
		rows = append(rows, r)
	}
	e.st.mu.Unlock()
	order := orderForExtras(e.st, rows, servers)
	if len(order) != len(rows) {
		t.Fatalf("%d rows in, %d out", len(rows), len(order))
	}
	for _, r := range order[:routeExtrasPerSweep] {
		if r.Edition == "" {
			return
		}
	}
	t.Fatal("the first pass of the budget is all prioritised Bedrock rows")
}
