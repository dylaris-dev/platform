package main

import (
	"log"
	"strings"
	"sync"

	"dylaris-pkg/errlog"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/status"
)

// The node's error stream, read by the panel through
// services.ErrorStreamServices ("node") and surfaced on the admin Status page.
//
// Why the node needs one at all: when its control channel to Core breaks, Core
// learns nothing, because the channel it would have learned it from is the one
// that broke. The node shows up as "offline, last seen ..." and the reason -
// a certificate pin that does not match, a refused proof, an address that no
// longer routes - exists only in the node's own container log, on a machine the
// operator may not even own. Redis is a SEPARATE channel with separate
// credentials the node holds locally, so it stays reachable across exactly the
// failures that take gRPC down, and it is the only way that reason reaches the
// panel.
var (
	coreErrLogMu sync.RWMutex
	coreErrLog   *errlog.Logger

	// Last message written, so a dial that fails every few seconds does not
	// fill a 500-entry stream with one repeated line and push out the history
	// that explains how it started.
	lastCoreErr string

	// Whether the last thing this PROCESS reported about the control channel was
	// success. Starts false so the first successful connect always writes a line.
	//
	// Deciding "did we recover" from lastCoreErr alone was wrong, and the live
	// test caught it: fixing a TLS mismatch means changing an env var, which
	// restarts the container, so the new process starts with no memory of the
	// failure and stays silent on reconnect. The stale ERROR then remained the
	// newest entry in the stream, and the Status page went on calling a healthy
	// node degraded until the one-hour age bound expired. The recovery has to be
	// stated on its own, not inferred from having personally witnessed the break.
	linkHealthy bool
)

// initNodeErrLog wires the node's error stream once Redis is authenticated.
// Called after the ACL bootstrap, because the stream key is scoped to this
// node's token and the grant comes with the node's own ACL user.
func initNodeErrLog(rdb *redis.Client, nodeToken string) {
	coreErrLogMu.Lock()
	defer coreErrLogMu.Unlock()
	coreErrLog = errlog.New(rdb, "node", nodeToken)
	log.Printf("diagnostics: node errors are mirrored to %s and shown on the panel's Status page", coreErrLog.StreamKey())
}

// reportCoreProblem records a control-plane failure to the local log AND to the
// node's Redis error stream, with the same text in both places.
//
// source is the stage that failed ("grpc-dial", "grpc-stream", "grpc-auth"), so
// the panel row says WHICH step broke rather than only that something did.
func reportCoreProblem(source, detail string) {
	log.Printf("core-link: %s failed: %s", source, detail)

	// State first, THEN the nil check. Updating it after the early return tied
	// "the channel is broken" to "we happened to have somewhere to write it",
	// so a failure during boot - before initNodeErrLog runs - left the channel
	// still marked healthy and the next success unannounced.
	msg := source + ": " + detail
	coreErrLogMu.Lock()
	repeat := msg == lastCoreErr
	lastCoreErr = msg
	linkHealthy = false
	l := coreErrLog
	coreErrLogMu.Unlock()

	if l == nil {
		return // pre-bootstrap; the local log above is all there is
	}
	if repeat {
		return
	}
	l.Error("core-link", detail+" "+coreLinkHint(detail))
}

// reportCoreRecovered records that the control channel is up, so a stream whose
// newest entry is an error means the node is STILL broken and one whose newest
// entry is this means it healed. The Status page reads exactly that newest
// entry.
//
// It fires on the first success of the process, not only after an error this
// process saw. Fixing a control-channel problem usually means changing an env
// var, which restarts the container - so the process that reconnects is almost
// never the one that failed, and inferring recovery from having witnessed the
// break left every real fix unannounced.
func reportCoreRecovered(coreID string) {
	coreErrLogMu.Lock()
	already := linkHealthy
	linkHealthy = true
	lastCoreErr = ""
	l := coreErrLog
	coreErrLogMu.Unlock()
	if already || l == nil {
		return
	}
	l.Info("core-link", "control channel to Core "+coreID+" is up")
}

// coreLinkHint turns a transport error into the next thing to check.
//
// The raw gRPC text is accurate and useless to whoever has to fix it: "connection
// error: desc = transport: authentication handshake failed" names no cause and
// no action. These are the failures the node<->Core channel actually has, and
// each one has exactly one likely fix.
func coreLinkHint(detail string) string {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "fingerprint mismatch"):
		return "[Core is serving a different certificate than this node pins. In-cluster nodes derive it from CLUSTER_SECRET, so the two ends hold different values; a BYON node pins GRPC_TLS_FINGERPRINT, so re-copy the deploy snippet from the panel.]"
	case strings.Contains(d, "no certificate pin"):
		return "[GRPC_TLS_ENABLED is on but this node has nothing to pin against. Set CLUSTER_SECRET (in-cluster) or GRPC_TLS_FINGERPRINT (BYON), or set GRPC_TLS_ENABLED=false on Core AND every node.]"
	// Both directions of a TLS/plaintext split, which do NOT look alike:
	//   - TLS node -> plaintext Core: "first record does not look like a TLS handshake"
	//   - plaintext node -> TLS Core: "error reading server preface: EOF"
	// The second was measured on the live cluster and matched none of the words
	// this looked for, so the one mismatch an operator is most likely to create
	// while flipping the flag arrived with no hint at all.
	case strings.Contains(d, "handshake failed"), strings.Contains(d, "tls"),
		strings.Contains(d, "first record does not look like"),
		strings.Contains(d, "server preface"):
		return "[TLS mismatch. One side has GRPC_TLS_ENABLED on and the other off - a TLS listener refuses a plaintext dial, and a TLS dial against a plaintext listener fails the same way. Both ends must hold the same value; it defaults to true.]"
	case strings.Contains(d, "connection refused"):
		return "[Nothing is listening on that address. Check Core is up and that this machine reaches its gRPC port.]"
	case strings.Contains(d, "i/o timeout"), strings.Contains(d, "deadline exceeded"), strings.Contains(d, "context deadline"):
		return "[The address routes nowhere or is filtered. On a BYON machine this is usually the warp tunnel being down - check the warp container first.]"
	case strings.Contains(d, "no such host"):
		return "[The hostname does not resolve from this machine.]"
	case strings.Contains(d, "unauthenticated"), strings.Contains(d, "permission denied"), strings.Contains(d, "proof"):
		return "[Core rejected this node's identity proof. The node secret and Core disagree - admit this node from Settings -> Nodes in the panel; nothing needs changing here.]"
	default:
		return ""
	}
}

// grpcErrText renders a gRPC error for humans: the status message alone where
// there is one, because status.Error's String() prefixes a code and a wrapper
// that push the actual cause off the end of a panel row.
func grpcErrText(err error) string {
	if err == nil {
		return ""
	}
	if st, ok := status.FromError(err); ok && st.Message() != "" {
		return st.Message()
	}
	return err.Error()
}
