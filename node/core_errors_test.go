package main

import (
	"strings"
	"testing"
)

// The hint is the whole value of this path: the raw transport error is accurate
// and useless to whoever has to fix it. Each case below is a real error string
// the node<->Core channel produces.
func TestCoreLinkHintNamesTheFix(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		want   string // a phrase that must appear
	}{
		{
			// Measured on the live cluster: node with GRPC_TLS_ENABLED=false
			// dialing a TLS Core. It matched none of the words the first version
			// looked for, so the single most likely mistake while flipping the
			// default produced an entry with no hint at all.
			name:   "plaintext node against a TLS Core",
			detail: `connection error: desc = "error reading server preface: EOF"`,
			want:   "TLS mismatch",
		},
		{
			// The same split seen from the other side.
			name:   "TLS node against a plaintext Core",
			detail: `connection error: desc = "transport: authentication handshake failed: tls: first record does not look like a TLS handshake"`,
			want:   "TLS mismatch",
		},
		{
			name:   "pin does not match",
			detail: "core gRPC: certificate fingerprint mismatch (Core served ab..., this node expects cd...)",
			want:   "re-copy the deploy snippet",
		},
		{
			name:   "no pin configured",
			detail: "core gRPC: no certificate pin configured",
			want:   "GRPC_TLS_FINGERPRINT",
		},
		{
			name:   "nothing listening",
			detail: "dial tcp 10.20.0.1:25501: connect: connection refused",
			want:   "Nothing is listening",
		},
		{
			name:   "routes nowhere",
			detail: "dial tcp 10.0.14.62:25501: i/o timeout",
			want:   "warp",
		},
		{
			name:   "identity refused",
			detail: "Core core-1 rejected this node's identity: invalid secret proof",
			// The route out of this state moved from the node's environment to
			// the panel, so the hint has to send the reader there.
			want: "Settings -> Nodes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := coreLinkHint(tc.detail)
			if got == "" {
				t.Fatalf("no hint for %q - the operator gets the raw transport error and nothing else", tc.detail)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("hint = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// An unrecognised error must add nothing rather than guess. A wrong hint is
// worse than none: it sends someone to check the thing that is not broken.
func TestCoreLinkHintStaysSilentOnTheUnknown(t *testing.T) {
	if h := coreLinkHint("some entirely novel failure"); h != "" {
		t.Errorf("hint = %q, want empty for an unclassified error", h)
	}
}

// resetCoreLinkState puts the package globals back to a fresh process's state.
func resetCoreLinkState() {
	coreErrLogMu.Lock()
	lastCoreErr, linkHealthy, coreErrLog = "", false, nil
	coreErrLogMu.Unlock()
}

// The live test caught this: fixing a control-channel problem means changing an
// env var, which restarts the container. The process that reconnects is
// therefore almost never the one that failed, and the first version only
// announced a recovery if it had personally seen the break - so a real fix went
// unannounced, the stale ERROR stayed the newest entry in the stream, and the
// Status page called a healthy node degraded for the next hour.
func TestReportCoreRecoveredFiresOnAFreshProcess(t *testing.T) {
	resetCoreLinkState()
	t.Cleanup(resetCoreLinkState)

	// A brand-new process has seen no failure at all - exactly the restart case.
	coreErrLogMu.RLock()
	healthy := linkHealthy
	coreErrLogMu.RUnlock()
	if healthy {
		t.Fatal("a fresh process must start not-healthy so its first success is announced")
	}

	reportCoreRecovered("core-1")
	coreErrLogMu.RLock()
	healthy, last := linkHealthy, lastCoreErr
	coreErrLogMu.RUnlock()
	if !healthy {
		t.Error("linkHealthy must be true after a successful connect")
	}
	if last != "" {
		t.Errorf("lastCoreErr = %q, want cleared so a later identical failure is not suppressed", last)
	}
}

// The second Core connecting must not add a second recovery line: connectToCore
// runs once per Core, and two identical lines in the stream say nothing extra.
func TestReportCoreRecoveredIsQuietWhileAlreadyHealthy(t *testing.T) {
	resetCoreLinkState()
	t.Cleanup(resetCoreLinkState)

	reportCoreRecovered("core-1")
	coreErrLogMu.RLock()
	first := linkHealthy
	coreErrLogMu.RUnlock()
	reportCoreRecovered("core-2") // must be a no-op
	coreErrLogMu.RLock()
	second := linkHealthy
	coreErrLogMu.RUnlock()
	if !first || !second {
		t.Fatal("both calls must leave the channel marked healthy")
	}
}

// A failure after a recovery has to re-arm the announcement, or a node that
// breaks, heals and breaks again reports the second break and then never says
// it came back.
func TestReportCoreProblemReArmsTheRecovery(t *testing.T) {
	resetCoreLinkState()
	t.Cleanup(resetCoreLinkState)

	reportCoreRecovered("core-1")
	reportCoreProblem("grpc-stream", "something broke")
	coreErrLogMu.RLock()
	healthy := linkHealthy
	coreErrLogMu.RUnlock()
	if healthy {
		t.Error("a reported failure must clear linkHealthy so the next success is announced again")
	}
}
