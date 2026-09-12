package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"dylaris-pkg/retry"
	pb "dylaris-proto/node"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
)

// nodeSecret is the node's per-node Redis secret once obtained. nil until
// ensureNodeSecret completes the bootstrap. Written from three independent
// call sites - main's startup path, the ACL watchdog's re-bootstrap, and the
// gRPC mesh's per-Core-connection reconnect handler - so every read and write
// goes through the guarded accessors below (getNodeSecret/setNodeSecret),
// never this global directly.
var nodeSecret []byte

// nodeSecretMu guards nodeSecret. Before
// this existed, grpc_mesh's per-reconnect handler wrote nodeSecret directly
// with no synchronization AND with no change-detection, which could blind the
// ACL watchdog: the watchdog's own "prev := nodeSecret" read would observe
// the already-updated value, bytes.Equal would be true, no restart would
// fire, and the node would be stranded on a stale rdb password until a
// manual restart. Every writer now goes through setNodeSecret, which applies
// the SAME change-detection + restart rule regardless of caller.
var nodeSecretMu sync.Mutex

// getNodeSecret returns the current per-node secret (nil until the startup
// bootstrap completes). Safe for concurrent use.
func getNodeSecret() []byte {
	nodeSecretMu.Lock()
	defer nodeSecretMu.Unlock()
	return nodeSecret
}

// waitForNodeSecret blocks until the per-node secret is available, the context
// is cancelled, or the timeout elapses. Returns ok=false in the latter two.
//
// The secret arrives over the authenticated gRPC bootstrap, so anything keyed on
// it that starts concurrently with boot can observe it as empty. Polling beats
// reading it once: a caller that gave up would stay disabled for the whole
// process lifetime even though the secret showed up a second later.
func waitForNodeSecret(ctx context.Context, timeout time.Duration) ([]byte, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if s := getNodeSecret(); len(s) > 0 {
			return s, true
		}
		if time.Now().After(deadline) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(time.Second):
		}
	}
}

// setNodeSecret installs a freshly obtained per-node secret. Applies the SAME
// rule no matter which caller triggers it: the FIRST install (prev is nil,
// e.g. the startup bootstrap or a cache load) never restarts; a genuine
// CHANGE from an already-loaded secret (a real pairing rotation, whether
// detected by the watchdog or by the gRPC mesh's reconnect handler) always
// logs loud and log.Fatal's, so the proven startup path rebuilds rdb from the
// new secret on restart. A re-confirmation of the SAME secret is a no-op
// restart-wise. persist=false skips the disk write for a caller that already
// has the value on disk (e.g. loading the cache at startup); every other
// caller passes true.
func setNodeSecret(s []byte, persist bool) {
	nodeSecretMu.Lock()
	prev := nodeSecret
	nodeSecret = s
	nodeSecretMu.Unlock()
	if persist {
		if werr := saveNodeSecret(nodeSecretDir, s); werr != nil {
			log.Printf("redisacl: WARN failed to persist node secret: %v", werr)
		}
	}
	if prev != nil && !bytes.Equal(prev, s) {
		// MC server containers are separate Docker containers and are
		// unaffected by this process restarting; only the node management
		// plane briefly blips.
		log.Fatal("redisacl: per-node secret rotated; restarting node agent to adopt new Redis credentials")
	}
}

// ensureNodeSecret returns the per-node secret. Uses the
// cached .node_secret when present (no Core contact needed — resilience); else
// bootstraps it from Core via a one-shot gRPC handshake. Loops until success or
// ctx cancel. Fatal only when .node_id exists and cannot be read: see below.
func ensureNodeSecret(ctx context.Context) []byte {
	// Adopt a previously assigned identity before any credential/key derivation.
	// Every connect from here on presents it. A file that is there but cannot be
	// read stops the node rather than letting it dial as NODE_ID or the hostname:
	// that is another identity, which Core enrols as a NEW node when the node
	// holds CLUSTER_SECRET, and its servers stay behind on the old one.
	id, idErr := cachedNodeID(nodeSecretDir)
	if idErr != nil {
		log.Fatalf("redisacl: %v. That file holds this node's identity, and dialling Core without it "+
			"would present a different one. Repair it and restart. To pair this machine as a NEW node instead, "+
			"delete .node_id, .node_secret AND .node_key together from %s and restart: with .node_secret left "+
			"behind, Core refuses the node as presenting a secret proof for an identity it does not know.", idErr, nodeSecretDir)
	}
	hadAssignedID := id != ""
	if hadAssignedID {
		nodeID = id
	}
	// The key before anything dials, for the same reason. A key this node cannot
	// read is not replaced; the node goes on without one, which Core admits by
	// the secret only while its row has never held a key.
	if k, err := loadOrCreateNodeKey(nodeSecretDir); err != nil {
		log.Printf("nodekey: WARN %v. This node presents no key: Core admits it by its secret while its "+
			"row holds no key, and refuses it once it does.", err)
	} else {
		setNodeKey(k)
	}
	if s, ok := loadNodeSecret(nodeSecretDir); ok {
		setNodeSecret(s, false) // already on disk; first install, never restarts
		log.Println("redisacl: using cached node secret")
		return s
	}
	// A node that already holds a server-assigned identity but has no cached
	// secret must NOT silently re-pair as if it were new. That guard stays; what
	// changed is what happens when it has no credential to offer.
	//
	// It used to STOP here. That is what made re-pairing an on-machine job: a
	// node that never dials is a node Core cannot see, so the only way back was
	// to put a token in its environment and restart it. It now keeps dialling
	// with the identity it claims and no proof. Core refuses that - it will not
	// mint a secret for an unproven caller - but it RECORDS the attempt, and an
	// operator can admit the machine from Settings -> Nodes without touching it.
	//
	// This does not weaken the guard, because the guard was never about the
	// refusal. It was written after a node re-registered as NEW and produced 249
	// node rows in an afternoon, orphaning the servers on the old id. Dialling
	// with an existing identity and no proof cannot do that: Core refuses to
	// mint for an identity it does not know, and allowIdentityChange still
	// governs adopting a different one.
	//
	// With a key it usually needs no operator at all: Core hands the secret it
	// already holds to a node that signs with the key registered for it.
	if hadAssignedID && nodeEnrollToken == "" && clusterSecret == "" {
		log.Printf("redisacl: paired node id %s has no cached secret. Dialling Core with its key and no "+
			"secret proof; Core hands the secret back if it holds this key, and otherwise refuses it until "+
			"an operator admits it in Settings -> Nodes, where this node's connection attempts are listed.", nodeID)
	}
	// Shared reconnect schedule: 12x5s, then every 30s. Never gives up - a node
	// whose Core is briefly away has to come back on its own.
	var bo retry.Backoff
	for {
		// The one caller allowed to adopt a server-assigned identity: nothing
		// else is running yet, and re-pairing is what this function is for.
		s, err := bootstrapSecretViaGRPC(ctx, true)
		if err == nil && len(s) == 32 {
			setNodeSecret(s, true) // first install (nodeSecret was nil until now), never restarts
			log.Println("redisacl: obtained node secret via gRPC bootstrap")
			return s
		}
		wait := bo.Next()
		log.Printf("redisacl: secret bootstrap failed (retry in %s): %v", wait, err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// bootstrapSecretViaGRPC does a one-shot NodeConnect to CORE_GRPC_ADDR. It
// presents whatever bootstrapCreds says it holds - a proof of the cached secret,
// a recovery/enroll token, or both - and Core answers with the existing secret
// re-applied or a freshly minted one. Always contacts Core (used for first boot
// AND for re-confirm after a Redis auth failure).
// allowIdentityChange is passed true by exactly one caller, ensureNodeSecret,
// and false by the two that run later.
//
// Two reasons, and either alone is enough. Adopting a server-assigned identity
// writes the package-level nodeID, which by then is being read by the
// heartbeat, the SFTP listener, the beam server, the stats collector and the
// gRPC mesh - a plain string written under no lock while ten goroutines read
// it. And a paired node quietly re-pairing under a NEW identity in the
// background is the exact thing ensureNodeSecret's hard guard exists to
// prevent: it orphans the old node row and its three scoped Redis ACL users,
// and every server assigned to the old id is suddenly on a node that no longer
// exists. That path is reachable from the watchdog whenever the startup
// persist failed, since saveNodeSecret only WARNs.
//
// Refusing loses nothing: re-pairing is a startup operation, and a restart goes
// through ensureNodeSecret, which demands a recovery or enroll token first.
// identityChange decides what to do with the identity Core returned. adopt is
// true only when Core named a DIFFERENT id and the caller is allowed to take
// it; an id that matches, or an empty one, is simply nothing to do.
func identityChange(assignedID, currentID string, allow bool) (adopt bool, err error) {
	if assignedID == "" || assignedID == currentID {
		return false, nil
	}
	if !allow {
		return false, fmt.Errorf(
			"Core assigned identity %s but this node is already running as %s; "+
				"refusing to change identity outside startup. Restart the node; if Core "+
				"still refuses it, admit it from Settings -> Nodes",
			assignedID, currentID)
	}
	return true, nil
}

// bootstrapCreds decides what a bootstrap NodeAuth carries: a proof of the
// cached secret, and - INDEPENDENTLY - the enroll token that lets Core issue a
// first one.
//
// The independence is the whole point. These used to be one else-chain, so a
// cached secret suppressed the token entirely. Reset pairing wipes Core's copy
// of the secret and DELUSERs the node's three Redis users, but it cannot touch
// .node_secret on the node's own disk - so a reset node still has a cache, sent
// only the now-worthless proof, and never sent the token that would have let it
// back in. Core answered "cluster proof required", the bootstrap loop retried
// forever, and the documented recovery was completable only by deleting
// .node_secret by hand, which nothing told the operator to do.
//
// There used to be a THIRD credential here, NODE_RECOVERY_TOKEN, and it is gone
// on purpose. Re-pairing meant editing the environment of the node and
// restarting it, which on a five-machine Swarm stack means touching the stack
// for one host. Re-admission is now decided in the panel: the node keeps
// dialling, Core records the refused attempt, and an operator approves it from
// Settings -> Nodes. Nothing has to be set on the machine.
//
// Sending both is safe on every Core branch: with a secret on file Core runs
// the challenge and ignores the token; without one the token is the only way in.
func bootstrapCreds(hasCached bool, enrollToken string) (sendProof bool, token string) {
	return hasCached, enrollToken
}

func bootstrapSecretViaGRPC(ctx context.Context, allowIdentityChange bool) ([]byte, error) {
	if coreGRPCAddr == "" {
		return nil, fmt.Errorf("CORE_GRPC_ADDR not set")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(coreGRPCAddr, coreDialCreds())
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	stream, err := pb.NewNodeServiceClient(conn).NodeConnect(dialCtx)
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}

	auth := &pb.NodeAuth{NodeToken: nodeID, AclSupported: true, Identity: machineIdentity()}
	// See grpc_mesh.go: Core needs this at connect time to answer a
	// mandatory-update deadline. Empty on an unstamped build.
	if v := nodeReleaseVersion(); !v.IsZero() {
		auth.ReleaseVersion = v.String()
	}
	cached, hasCached := loadNodeSecret(nodeSecretDir)
	sendProof, token := bootstrapCreds(hasCached, nodeEnrollToken)
	if sendProof {
		auth.SecretProof = aclProof(cached, nodeID)
	}
	auth.EnrollToken = token
	if clusterSecret != "" {
		auth.ClusterProof = aclClusterProof(clusterSecret, nodeID)
	}
	key := currentNodeKey()
	auth.NodePublicKey = publicKeyOf(key)
	ips := getNodeIPs()
	auth.Ips = &pb.NodeIPs{Public: ips.Public, Private: ips.Private}

	if err := stream.Send(&pb.NodeMessage{Payload: &pb.NodeMessage_Auth{Auth: auth}}); err != nil {
		return nil, fmt.Errorf("send auth: %w", err)
	}
	res, err := recvAuthResult(stream, auth.NodeToken, cached, key)
	if err != nil {
		return nil, fmt.Errorf("recv auth result: %w", err)
	}
	if res == nil || !res.Ok {
		msg := "rejected"
		if res != nil {
			msg = res.Message
			// Every caller retries on an error, and the retry presents the new
			// key, which lands in the branch that asks for a cluster proof or
			// the admission the operator armed.
			if res.NodeKeyRejected {
				replaceRejectedNodeKey(nodeSecretDir, auth.NodePublicKey)
			}
		}
		return nil, fmt.Errorf("auth rejected: %s", msg)
	}
	adopt, ierr := identityChange(res.AssignedId, nodeID, allowIdentityChange)
	if ierr != nil {
		return nil, ierr
	}
	if adopt {
		// Distinguish the two cases this branch covers. They look identical in a
		// log and mean opposite things: a first pairing is routine, while
		// REPLACING an identity the node already held means the old node row and
		// its three scoped Redis ACL users have just been orphaned, and any
		// server assigned to the old id is now on a node that no longer exists.
		// One line for both read as the benign one and hid that for a whole
		// investigation.
		previous := nodeID
		nodeID = res.AssignedId
		if werr := saveNodeID(nodeSecretDir, res.AssignedId); werr != nil {
			log.Printf("redisacl: WARN failed to persist assigned node id: %v", werr)
		}
		if previous == "" {
			log.Printf("redisacl: paired, Core assigned node identity %s", res.AssignedId)
		} else {
			log.Printf("redisacl: WARN identity REPLACED: was %s, Core assigned %s. "+
				"The previous node row and its Redis ACL users are now orphaned; "+
				"this happens when the cached secret is missing at boot.",
				previous, res.AssignedId)
		}
	}
	var secret []byte
	switch {
	case res.NodeSecret != "":
		raw, derr := hex.DecodeString(res.NodeSecret)
		if derr != nil || len(raw) != 32 {
			return nil, fmt.Errorf("bad node secret in auth result")
		}
		secret = raw
	case hasCached:
		secret = cached // Core re-applied ACL for our existing secret
	default:
		return nil, fmt.Errorf("no secret returned and none cached")
	}
	// Core's Redis address rides on every auth result, this one included. It is
	// validated with the secret Core just confirmed, which is the login the
	// node's own client uses; at a boot that knew no address it is simply taken.
	noteCoreRedisAddr(ctx, res.RedisAddr, secret)
	return secret, nil
}

// redisACLWatchdog re-bootstraps the node's ACL over gRPC when Redis auth is
// sustainedly failing (NOAUTH/NOPERM/WRONGPASS), e.g. after a Valkey restart that
// dropped the aclfile. It does NOT rebuild rdb: the per-node secret is cached on
// disk and stable, so bootstrapSecretViaGRPC sends a proof, Core re-provisions the
// identical user + password, and the existing client re-auths transparently on its
// next command. This also breaks the mesh discovery chicken-and-egg (Cores are read
// from a now-authenticated Redis). Backoff-capped, throttled logs, never falls open.
//
// Deliberately NOT on the shared retry schedule (dylaris-pkg/retry): the base
// interval here is a health PROBE against a Redis this node is already connected
// to, not a reconnect, and the 5-minute ceiling exists so a node cannot hammer a
// Core that is already down. Do not "align" it with the reconnect loops.
func redisACLWatchdog(ctx context.Context, rdb *redis.Client) {
	const (
		probeEvery = 15 * time.Second
		failsToAct = 2 // consecutive auth failures before acting (~30s)
		maxBackoff = 5 * time.Minute
		logEvery   = 2 * time.Minute
	)
	backoff := probeEvery
	consecutive := 0
	var lastLog time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		err := rdb.Ping(ctx).Err()
		if err == nil {
			consecutive = 0
			backoff = probeEvery
			continue
		}
		if !isRedisAuthError(err) {
			// Redis unreachable (not an auth problem): a re-bootstrap would not
			// help and Core may be down too. Keep probing at the base interval.
			consecutive = 0
			backoff = probeEvery
			continue
		}
		consecutive++
		if time.Since(lastLog) >= logEvery {
			lastLog = time.Now()
			log.Printf("redisacl: WARNING sustained Redis auth failure (%v); re-bootstrapping ACL with Core", err)
		}
		if consecutive < failsToAct {
			continue
		}
		if s, berr := bootstrapSecretViaGRPC(ctx, false); berr == nil && len(s) == 32 {
			// setNodeSecret applies the change-detection + restart rule itself
			// (log.Fatal if this differs from the currently loaded secret), so
			// the lines below only run when the secret is unchanged (Core just
			// re-applied the same ACL after Valkey lost its aclfile).
			setNodeSecret(s, true)
			consecutive = 0
			backoff = probeEvery
			log.Println("redisacl: re-bootstrap OK; Core re-applied the node ACL")
		} else if berr != nil {
			// Core unreachable: stay fail-closed on Redis, back off and retry.
			if backoff < maxBackoff {
				backoff *= 2
			}
			log.Printf("redisacl: re-bootstrap failed (retry in %s): %v", backoff, berr)
		}
	}
}

// isRedisAuthError reports whether a Redis error is an ACL/auth rejection
// (NOAUTH/NOPERM/WRONGPASS) rather than a connectivity failure.
func isRedisAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "NOAUTH") || strings.Contains(s, "NOPERM") || strings.Contains(s, "WRONGPASS")
}
