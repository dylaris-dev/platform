package nodegrpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"strings"

	beamauth "dylaris-pkg/beam/auth"
	pb "dylaris-proto/node"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"time"
)

// NodeLookup interface for looking up Nodes by token.
// Implemented by the store.Store interface.
type NodeLookup interface {
	GetNodeByToken(token string) (*Node, error)
}

// ACLHandshake is the per-node Redis-ACL bootstrap run on every node connect.
// Implemented by *redisacl.Handshake, wired from main (always non-nil).
type ACLHandshake interface {
	EnsureExisting(ctx context.Context, nodeID int, token string) (secretHex string, err error)
	Enroll(ctx context.Context, token, enrollToken, address string) (assignedID string, nodeID int, secretHex string, err error)
	EnrollPlatform(ctx context.Context, token, address string) (assignedID string, nodeID int, secretHex string, err error)
	VerifyProof(ctx context.Context, nodeID int, token, proof string) (ok bool, err error)
	VerifyChallenge(ctx context.Context, nodeID int, nonce, response string) (ok bool, err error)
	VerifyClusterProof(token, proof string) bool
	HasSecret(ctx context.Context, nodeID int) (ok bool, err error)
}

// LinkCredSource supplies the per-node Link sidecar credentials (tunnel token +
// discovery proof), derived by the gateway from the cluster secret. Delivered in
// AuthResult so a secret-free node can spawn its Link sidecar without CLUSTER_SECRET.
// Satisfied structurally by services.GatewayProvider.
type LinkCredSource interface {
	LinkToken(nodeID string) string
	DiscoveryProof(nodeID string) string
}

// AdmissionChecker gates NEW node registrations (network + join) before enroll,
// and consumes the one-shot join slot after a successful enroll. nil =
// admission off. Satisfied structurally by *services.AdmissionGate.
type AdmissionChecker interface {
	// CheckNewRegistration is a PEEK run in the pre-check, before the enroll
	// token is even validated: is a new registration currently allowed?
	CheckNewRegistration(ctx context.Context, ip net.IP) (allowed bool, reason string, err error)
	// ConsumeJoinSlot is called ONLY after s.acl.Enroll succeeds, so a garbage
	// connection can never burn the one-shot slot before a real device enrolls.
	ConsumeJoinSlot(ctx context.Context) error
}

// peerIP returns the TCP source IP of the gRPC connection (NOT the self-reported
// auth.Ips value and NOT the overlay IP), or nil if it cannot be resolved.
// peerIPString is peerIP as text, empty when the address is unknown. An empty
// value never matches an approval: ConsumeNodeJoinApproval refuses one, so an
// unidentifiable caller cannot be let in by a blank on both sides.
func peerIPString(ctx context.Context) string {
	if ip := peerIP(ctx); ip != nil {
		return ip.String()
	}
	return ""
}

func peerIP(ctx context.Context) net.IP {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return net.ParseIP(p.Addr.String())
	}
	return net.ParseIP(host)
}

// JoinAttempt is one refused connection, as this layer sees it.
//
// Only PeerIP is observed; everything else is what the caller said about itself
// and travels solely so a human can recognise the machine in the panel. The
// struct lives here rather than in models because this package deliberately
// imports neither models nor store - main.go adapts it, the way StoreAdapter
// already does for node lookups.
type JoinAttempt struct {
	NodeToken      string
	PeerIP         string
	PublicIP       string
	PrivateIPs     string
	Hostname       string
	CPUCores       int
	CPUModel       string
	MemoryBytes    int64
	ReleaseVersion string
	Reason         string
}

// JoinAttemptRecorder makes a refusal visible and lets an operator undo it.
//
// It replaced RecoveryTokenConsumer, which existed for NODE_RECOVERY_TOKEN: an
// admin-minted token that had to be put in the node's own environment and the
// node restarted. On a Swarm stack that is an edit and a redeploy to re-admit
// one machine, and it could only be started from a screen that never showed the
// node was being refused in the first place. Re-admission is now decided in the
// panel, and nothing is set on the machine.
//
// nil = neither recording nor panel admission, which is what every test that
// does not care wants.
type JoinAttemptRecorder interface {
	RecordJoinAttempt(a JoinAttempt) error
	// ConsumeJoinApproval reports whether an operator has admitted this identity
	// FROM THIS ADDRESS, and closes the door behind it. Bounded by address
	// because the identity in a refused attempt is self-claimed.
	ConsumeJoinApproval(nodeToken, peerIP string) (bool, error)
	// ForgetJoinAttempts drops the record once the node is back, so the list is
	// of machines that need attention rather than a history.
	ForgetJoinAttempts(nodeToken string) error
}

// JoinAttemptFuncs adapts plain functions to JoinAttemptRecorder, so main.go can
// wire the store without this package importing it.
type JoinAttemptFuncs struct {
	Record  func(a JoinAttempt) error
	Consume func(nodeToken, peerIP string) (bool, error)
	Forget  func(nodeToken string) error
}

func (f *JoinAttemptFuncs) RecordJoinAttempt(a JoinAttempt) error { return f.Record(a) }
func (f *JoinAttemptFuncs) ConsumeJoinApproval(t, ip string) (bool, error) {
	return f.Consume(t, ip)
}
func (f *JoinAttemptFuncs) ForgetJoinAttempts(t string) error { return f.Forget(t) }

// Node is a minimal representation used by the gRPC layer.
// Matches the fields needed from models.Node.
type Node struct {
	ID    int
	Token string
}

// StoreAdapter wraps any store that has GetNodeByToken returning *models.Node.
type StoreAdapter struct {
	GetByToken func(token string) (id int, err error)
}

func (a *StoreAdapter) GetNodeByToken(token string) (*Node, error) {
	id, err := a.GetByToken(token)
	if err != nil {
		return nil, err
	}
	return &Node{ID: id, Token: token}, nil
}

// Server implements the NodeService gRPC server.
type Server struct {
	pb.UnimplementedNodeServiceServer
	registry   *Registry
	nodeLookup NodeLookup
	coreID     string
	acl        ACLHandshake
	linkCreds  LinkCredSource
	admission  AdmissionChecker
	joins      JoinAttemptRecorder
	// updateGate refuses or warns a node that has not applied a mandatory
	// update. Set after construction rather than as an eighth positional
	// argument; nil means neither, which is what every test wants.
	updateGate *UpdateGate
}

// SetUpdateGate installs the mandatory-update policy. Separate from NewServer so
// the seven-argument constructor does not grow an eighth, and so a test server
// is silent about updates unless it asks not to be.
func (s *Server) SetUpdateGate(g *UpdateGate) { s.updateGate = g }

// NewServer creates a new gRPC server for Node connections.
func NewServer(registry *Registry, lookup NodeLookup, coreID string, acl ACLHandshake, linkCreds LinkCredSource, admission AdmissionChecker, joins JoinAttemptRecorder) *Server {
	return &Server{
		registry:   registry,
		nodeLookup: lookup,
		coreID:     coreID,
		acl:        acl,
		linkCreds:  linkCreds,
		admission:  admission,
		joins:      joins,
	}
}

// NodeConnect handles a bidirectional stream from a Node.
// tokenPrefix returns up to the first 8 chars of a token for logging, without
// panicking on short/invalid tokens (e.g. a misconfigured node or a probe).
func tokenPrefix(t string) string {
	if len(t) > 8 {
		return t[:8]
	}
	return t
}

// newChallengeNonce returns a fresh 32-byte random nonce as lowercase hex for the
// per-connection reconnect challenge.
func newChallengeNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Flow: Node connects → sends NodeAuth → Core validates → stream stays open.
func (s *Server) NodeConnect(stream pb.NodeService_NodeConnectServer) error {
	// Step 1: Wait for auth message (first message must be NodeAuth)
	firstMsg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive auth: %w", err)
	}

	auth := firstMsg.GetAuth()
	if auth == nil {
		_ = stream.Send(&pb.NodeMessage{
			Payload: &pb.NodeMessage_AuthResult{
				AuthResult: &pb.AuthResult{Ok: false, Message: "first message must be NodeAuth"},
			},
		})
		return fmt.Errorf("first message was not NodeAuth")
	}

	// Step 2 + 3: Authenticate + send auth result. Redis ACL is mandatory: every
	// node connect mints/provisions per-node scoped Redis creds and enrolls unknown
	// BYON nodes (with a valid enroll token). There is no OFF path anymore.
	ctx := stream.Context()
	sendFail := func(msg string) {
		// The reason reaches the node and reached nothing else: every rejection
		// below returns a descriptive error into gRPC, which has no interceptor,
		// so the authority side logged nothing at all. A node locked out of the
		// cluster was undebuggable from Core, and someone probing enroll tokens
		// or challenge responses left no trace. Logged in the closure rather
		// than at the call sites so a future rejection path cannot forget it.
		// Token is prefixed, never whole - it is a credential.
		log.Printf("acl: node auth rejected (%s) for %s from %v", msg, tokenPrefix(auth.NodeToken), peerIP(ctx))
		_ = stream.Send(&pb.NodeMessage{Payload: &pb.NodeMessage_AuthResult{
			AuthResult: &pb.AuthResult{Ok: false, Message: msg},
		}})
	}

	// recordRefusal makes a rejection VISIBLE. sendFail above writes a line to
	// Core's stdout, which dies with the container - so a node hammering the door
	// for six hours left nothing an operator could find, and the panel showed a
	// machine that was simply "offline".
	//
	// Called only for identities Core already knows. An unknown claimant is not
	// approvable (Core will not mint a secret for an identity it has no row for),
	// so listing one would add nothing to act on and would let anyone who can
	// reach this port write rows into that table.
	//
	// Best-effort: a failure to record must never change whether a node is
	// admitted. It is a screen, not a gate.
	recordRefusal := func(reason string) {
		if s.joins == nil {
			return
		}
		a := JoinAttempt{
			NodeToken:      auth.NodeToken,
			PeerIP:         peerIPString(ctx),
			ReleaseVersion: auth.ReleaseVersion,
			Reason:         reason,
		}
		if ips := auth.GetIps(); ips != nil {
			a.PublicIP = ips.Public
			a.PrivateIPs = strings.Join(ips.Private, ",")
		}
		if id := auth.GetIdentity(); id != nil {
			a.Hostname = id.Hostname
			a.CPUCores = int(id.CpuCores)
			a.CPUModel = id.CpuModel
			a.MemoryBytes = id.MemoryBytes
		}
		if err := s.joins.RecordJoinAttempt(a); err != nil {
			log.Printf("acl: could not record the refused join for %s: %v", tokenPrefix(auth.NodeToken), err)
		}
	}

	// Mandatory-update gate, BEFORE any lookup or provisioning. A node that is
	// going to be refused should be refused without Core first minting it
	// credentials, and refusing early keeps the reason in one place.
	//
	// The version is self-reported. It can only ever cost this node access or
	// earn it a warning, never grant anything, so nothing is trusted here that
	// should not be - and an unstamped node reports nothing and is admitted.
	verdict := s.updateGate.Check("node", auth.GetReleaseVersion())
	if verdict.Refuse {
		sendFail(verdict.Message)
		return fmt.Errorf("node %s refused: %s", tokenPrefix(auth.NodeToken), verdict.Message)
	}

	var node *Node
	{
		address := ""
		if ips := auth.GetIps(); ips != nil {
			address = ips.GetPublic()
		}
		existing, lookErr := s.nodeLookup.GetNodeByToken(auth.NodeToken)
		if lookErr != nil {
			// P0b-5 admission gate: network + join, for NEW registrations only.
			// Runs BEFORE the enroll-token check (network -> join -> enroll token).
			// The known-node branch below is NEVER gated (paired nodes always reconnect).
			if s.admission != nil {
				ip := peerIP(ctx)
				allowed, reason, aerr := s.admission.CheckNewRegistration(ctx, ip)
				if aerr != nil {
					sendFail("admission unavailable")
					return status.Errorf(codes.Internal, "acl: admission check failed for %s: %v", tokenPrefix(auth.NodeToken), aerr)
				}
				if !allowed {
					sendFail(reason)
					return status.Errorf(codes.PermissionDenied, "acl: admission denied (%s) for %s from %v", reason, tokenPrefix(auth.NodeToken), ip)
				}
			}
			// Unknown node. Exactly two ways in, tried in this order:
			//  (1) a single-use enroll token -> BYON node, bound to that token's
			//      owner. This is the ONLY path that can produce an owned node.
			//  (2) a valid cluster_proof -> operator-owned platform node
			//      (owner_id NULL), no token needed. This is the pairing spec's
			//      "for platform nodes without an enroll token, a cluster_proof":
			//      a node already holding CLUSTER_SECRET is trusted infrastructure
			//      by definition, and that secret is strictly more powerful than
			//      an enroll token, so demanding both only forces a
			//      deploy -> mint -> redeploy round trip on the operator's own
			//      fleet. A BYON node never holds CLUSTER_SECRET, so (1) stays
			//      the only door for tenants and ownership binding is unchanged.
			// The enroll token wins when both are present: setting it is an
			// explicit request for an owned node.
			// A node that already HOLDS an identity is never a new node.
			//
			// secret_proof is only ever sent by a node with a cached secret, so
			// an unknown token carrying one means Core has LOST that node's row,
			// not that a machine turned up for the first time. Enrolling it
			// mints an identity the node then refuses to adopt - correctly,
			// because swapping identity at runtime orphans its servers and its
			// scoped Redis users - and it comes back thirty seconds later. That
			// ran on production on 2026-08-31 and left 249 node rows, 119 of
			// them within one hour, while the node stayed down throughout.
			//
			// An enroll or recovery token is the deliberate act that re-opens
			// the door, and it is checked first below, so this only closes the
			// automatic cluster-proof path.
			if auth.SecretProof != "" && auth.EnrollToken == "" {
				// Says what is actually REACHABLE from here, which the first
				// version of this message did not. A recovery token is minted
				// per node, from that node's row in the panel - and the branch
				// we are in is the one where Core has no row for it, so
				// "Reset pairing" points at a screen where the node is not
				// listed. That is a dead end an operator can only leave by
				// touching the node's disk, so the message has to say so.
				const msg = "Core has no record of this node. Re-admission is granted per " +
					"node and cannot be granted for one that is not listed, so clear the cached " +
					"identity on the node (.node_id and .node_secret in its storage directory) " +
					"and restart it; it will pair again. If the node IS listed in the panel, " +
					"admit it from Settings -> Nodes instead - nothing needs setting on the machine."
				sendFail(msg)
				return fmt.Errorf("acl: node %s presents a secret proof for an unknown identity; refusing to mint a new one", tokenPrefix(auth.NodeToken))
			}
			var assignedID, secretHex string
			var id int
			var eerr error
			switch {
			case auth.EnrollToken != "":
				assignedID, id, secretHex, eerr = s.acl.Enroll(ctx, auth.NodeToken, auth.EnrollToken, address)
			case s.acl.VerifyClusterProof(auth.NodeToken, auth.ClusterProof):
				assignedID, id, secretHex, eerr = s.acl.EnrollPlatform(ctx, auth.NodeToken, address)
			default:
				sendFail("unknown node and no enroll token or cluster proof")
				return fmt.Errorf("acl: unknown node %s without enroll token or cluster proof", tokenPrefix(auth.NodeToken))
			}
			if eerr != nil {
				sendFail("enrollment failed")
				return fmt.Errorf("acl: enroll failed for %s: %w", tokenPrefix(auth.NodeToken), eerr)
			}
			// Consume the one-shot join slot HERE, only after enrollment actually
			// validated the enroll token and created the node - never during the
			// admission pre-check peek (CheckNewRegistration). A garbage connection
			// with an invalid/empty enroll token never reaches this line, so it can
			// no longer burn the one-shot slot before a real device gets a chance.
			// Non-fatal: the node is already enrolled at this point, so a failure
			// here is logged loudly, not surfaced to the caller.
			if s.admission != nil {
				if cerr := s.admission.ConsumeJoinSlot(ctx); cerr != nil {
					log.Printf("acl: node %d (%s): consume one-shot join slot: %v", id, tokenPrefix(assignedID), cerr)
				}
			}
			node = &Node{ID: id, Token: assignedID}
			ar := &pb.AuthResult{Ok: true, CoreId: s.coreID, AclEnabled: true, NodeSecret: secretHex, AssignedId: assignedID}
			applyUpdateWarning(ar, verdict)
			if s.linkCreds != nil {
				ar.LinkSecret = s.linkCreds.LinkToken(assignedID)
				ar.LinkDiscoveryProof = s.linkCreds.DiscoveryProof(assignedID)
			}
			if err := stream.Send(&pb.NodeMessage{Payload: &pb.NodeMessage_AuthResult{AuthResult: ar}}); err != nil {
				return fmt.Errorf("failed to send auth result: %w", err)
			}
		} else {
			node = existing
			hasSecret, serr := s.acl.HasSecret(ctx, node.ID)
			if serr != nil {
				sendFail("acl state error")
				return fmt.Errorf("acl: secret-state lookup failed for node %d: %w", node.ID, serr)
			}
			if hasSecret {
				// A provisioned node MUST prove possession of its secret against a
				// fresh single-use nonce, so a captured proof cannot be replayed. We
				// never re-hand the secret to a bare token holder; a node that
				// genuinely lost its cached secret recovers via operator action, not
				// a silent re-issue.
				nonce, nerr := newChallengeNonce()
				if nerr != nil {
					sendFail("challenge init failed")
					return fmt.Errorf("acl: nonce generation failed for node %d: %w", node.ID, nerr)
				}
				if err := stream.Send(&pb.NodeMessage{Payload: &pb.NodeMessage_Challenge{
					Challenge: &pb.NodeChallenge{Nonce: nonce},
				}}); err != nil {
					return fmt.Errorf("failed to send challenge: %w", err)
				}
				respMsg, rerr := stream.Recv()
				if rerr != nil {
					return fmt.Errorf("failed to receive challenge response: %w", rerr)
				}
				cr := respMsg.GetChallengeResponse()
				if cr == nil {
					sendFail("challenge response required")
					return fmt.Errorf("acl: node %d sent no challenge response", node.ID)
				}
				ok, verr := s.acl.VerifyChallenge(ctx, node.ID, nonce, cr.Response)
				if verr != nil || !ok {
					// The node holds a secret and so does Core, and they differ.
					// Core will not re-hand the secret to a bare token holder, so
					// this repeats every thirty seconds until somebody acts - which
					// is exactly why it has to be written down where an operator
					// looks.
					recordRefusal("the node's secret and Core's do not match")
					sendFail("bad challenge response")
					return fmt.Errorf("acl: bad challenge response for node %d", node.ID)
				}
			} else {
				// First issuance for a known node. Accept EITHER:
				//  (1) a cluster_proof (HMAC under CLUSTER_SECRET) — the operator's
				//      own machines, which hold it and recover by themselves; or
				//  (2) an admission an operator granted in the panel, bound to the
				//      address this connection is actually coming from.
				// EnsureExisting below re-issues the secret + re-provisions the ACL under
				// the SAME token/id (node_secret_enc was cleared -> a fresh secret is minted).
				// Neither path is gated by admission control: this is a known id, not a
				// new registration.
				//
				// (2) replaced a single-use token an operator had to put in the node's
				// own environment and restart it for. The check is deliberately the
				// same SHAPE - one-shot, consumed here, refusable - and the address
				// binding is what a token in an env var could not offer: the identity
				// on a refused attempt is self-claimed, so "this id" alone would admit
				// whoever knocks with it next.
				if !s.acl.VerifyClusterProof(node.Token, auth.ClusterProof) {
					admitted := false
					if s.joins != nil {
						ok, aerr := s.joins.ConsumeJoinApproval(node.Token, peerIPString(ctx))
						if aerr != nil {
							sendFail("admission check failed")
							return fmt.Errorf("acl: node %d admission check failed: %w", node.ID, aerr)
						}
						admitted = ok
					}
					if !admitted {
						recordRefusal("waiting to be admitted: no cluster secret and no approval")
						sendFail("this node is not admitted; approve it in Settings -> Nodes")
						return fmt.Errorf("acl: node %d first-issuance without cluster proof or panel admission", node.ID)
					}
				}
			}
			secretHex, perr := s.acl.EnsureExisting(ctx, node.ID, node.Token)
			if perr != nil {
				sendFail("acl provision failed")
				return fmt.Errorf("acl: provision failed for node %d: %w", node.ID, perr)
			}
			res := &pb.AuthResult{Ok: true, CoreId: s.coreID, AclEnabled: true}
			applyUpdateWarning(res, verdict)
			if !hasSecret {
				// First-time issue for this known node (feature newly enabled, or
				// the secret was reset). Deliver once; later connects must prove it.
				res.NodeSecret = secretHex
			}
			if s.linkCreds != nil {
				res.LinkSecret = s.linkCreds.LinkToken(node.Token)
				res.LinkDiscoveryProof = s.linkCreds.DiscoveryProof(node.Token)
			}
			if err := stream.Send(&pb.NodeMessage{Payload: &pb.NodeMessage_AuthResult{AuthResult: res}}); err != nil {
				return fmt.Errorf("failed to send auth result: %w", err)
			}
			// It is in. The list is of machines needing attention, not a history,
			// so the row goes rather than lingering as a resolved-looking warning.
			if s.joins != nil {
				if err := s.joins.ForgetJoinAttempts(node.Token); err != nil {
					log.Printf("acl: could not clear the refused-join record for node %d: %v", node.ID, err)
				}
			}
		}
	}

	// Step 4: Register connection
	conn := s.registry.Register(node.ID, node.Token, stream)
	log.Printf("gRPC: Node %d connected (token=%s...)", node.ID, tokenPrefix(node.Token))

	defer func() {
		// Pass this stream's own connection: by the time a dead stream gets here
		// the node may already have reconnected and taken over the map entry, and
		// tearing that one down would leave a connected node unreachable.
		s.registry.Unregister(node.ID, conn)
		log.Printf("gRPC: Node %d disconnected", node.ID)
	}()

	// Step 5: Read loop — route incoming messages to waiting handlers
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("node %d stream error: %w", node.ID, err)
		}

		// Route response to the handler waiting on this request_id
		if !conn.RouteResponse(msg) {
			log.Printf("gRPC: Node %d sent unroutable message (request_id=%s)", node.ID, msg.RequestId)
		}

		// Close the streaming channel when the final TransferDone arrives.
		// Metadata TransferDone has Filename set and TotalBytes==0 (sent before chunks).
		// Final TransferDone has no Filename (TotalBytes>0 for non-empty, ==0 for empty files).
		if done := msg.GetTransferDone(); done != nil && (done.TotalBytes > 0 || done.Filename == "") {
			conn.CloseStreamingRequest(msg.RequestId)
		}
	}
}

// StartGRPCServer binds the port, starts serving in its own goroutine and
// returns the server so the caller can shut it down. When tlsEnabled is set, it
// presents the cluster-wide certificate derived from clusterSecret
// (CLUSTER_SECRET) so nodes can pin its fingerprint; otherwise it serves
// plaintext (unchanged behavior).
//
// Returning the *grpc.Server is the point of this signature: the server used to
// be a local here, so nothing outside could call GracefulStop and every node
// stream was severed by process exit instead of drained. Bind errors are
// returned synchronously rather than raised from inside a goroutine, so a port
// clash now fails the caller's boot sequence at a defined point.
func StartGRPCServer(port int, registry *Registry, lookup NodeLookup, coreID string, acl ACLHandshake, linkCreds LinkCredSource, admission AdmissionChecker, joins JoinAttemptRecorder, tlsEnabled bool, clusterSecret string) (*grpc.Server, error) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("failed to listen on port %d: %w", port, err)
	}

	opts := []grpc.ServerOption{
		// Detect a SILENTLY dead node - a frozen VM or a network blackhole that
		// never sends a TCP RST - within ~15s rather than ~40s. Time is how long
		// an idle connection sits before Core pings it; Timeout is how long it
		// then waits for the ack. A streaming request (file download/upload, the
		// tab-proxy bridge) blocks in `range ch` until this fires, so the sum is
		// how long a user waits on a vanished node before seeing an error.
		//
		// Only Time was lowered. Timeout stays at 10s on purpose: it bounds the
		// ack wait, and a node saturating its uplink with a large transfer can
		// legitimately delay a ping ack by a few seconds, so a tight Timeout
		// would kill the very upload in progress. Lowering Time (how OFTEN we
		// check) is the risk-free half; the ping is a tiny frame.
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    5 * time.Second,
			Timeout: 10 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.MaxRecvMsgSize(128 * 1024), // 128KB max message (64KB chunks + overhead)
	}

	if tlsEnabled {
		cert, fp, cerr := beamauth.DeriveClusterGRPCCert(clusterSecret)
		if cerr != nil {
			return nil, fmt.Errorf("derive cluster gRPC cert: %w", cerr)
		}
		opts = append(opts, grpc.Creds(credentials.NewServerTLSFromCert(&cert)))
		log.Printf("gRPC: NodeService TLS enabled (fingerprint pinning), cert fp=%s...", fp[:16])
	}

	grpcServer := grpc.NewServer(opts...)

	srv := NewServer(registry, lookup, coreID, acl, linkCreds, admission, joins)
	// The mandatory-update policy is installed for the REAL server only. Tests
	// construct Server directly and stay silent about updates unless they ask.
	srv.SetUpdateGate(NewUpdateGate())
	pb.RegisterNodeServiceServer(grpcServer, srv)

	log.Printf("gRPC: NodeService listening on :%d", port)
	go func() {
		// Serve returns nil after Stop/GracefulStop, so an orderly shutdown is
		// silent here and only a genuine serve failure is logged. It is logged
		// rather than fatal: by the time this can fail the caller owns the
		// process lifecycle and may already be shutting down.
		if err := grpcServer.Serve(lis); err != nil {
			log.Printf("gRPC: NodeService stopped serving: %v", err)
		}
	}()
	return grpcServer, nil
}
