package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	pb "dylaris-proto/node"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

// CoreInfo is the heartbeat payload written by each Core instance to Redis.
type CoreInfo struct {
	ID       string `json:"id"`
	GRPCAddr string `json:"grpc_addr"`
	IPs      struct {
		Public  string   `json:"public"`
		Private []string `json:"private"`
	} `json:"ips"`
}

// coreConnection holds the gRPC client connection and stream to one Core.
type coreConnection struct {
	conn    *grpc.ClientConn
	stream  pb.NodeService_NodeConnectClient
	cancel  context.CancelFunc
	handler *StreamHandler
	sendMu  sync.Mutex // serializes stream.Send across the read loop + WS pumps

	// pending maps the request_id of a node-initiated request (Request) to its
	// waiter; nil once the connection is gone. See node_request.go.
	pending   map[string]chan *pb.NodeMessage
	pendingMu sync.Mutex

	// flows holds the credit of each streaming request Core opened with a
	// flow window; see flow_credit.go.
	flows  map[string]*flowCredit
	flowMu sync.Mutex
}

// send serializes all writes to this Core stream. gRPC streams are not safe
// for concurrent Send; the WS bridge introduced the first background sender.
func (cc *coreConnection) send(msg *pb.NodeMessage) error {
	msg = boundForCore(msg)
	// Before the lock: a transfer waiting for its reader must not hold up
	// anyone else's messages.
	if err := cc.awaitCredit(msg); err != nil {
		if errors.Is(err, errFlowStalled) {
			// Uncounted, so it is not held: without it the reader on Core,
			// which has no deadline of its own, waits for this transfer forever.
			cc.sendMu.Lock()
			_ = cc.stream.Send(errorMsg(msg.RequestId, 504, "transfer stalled: the reader took nothing for too long"))
			cc.sendMu.Unlock()
		}
		return err
	}
	cc.sendMu.Lock()
	defer cc.sendMu.Unlock()
	return cc.stream.Send(msg)
}

// coreMaxMessage is what Core's gRPC server accepts in one message (128 KB,
// core/grpc/server.go), less room for the envelope.
const coreMaxMessage = 120 * 1024

// boundForCore replaces a message Core would refuse with an error for the same
// request. Over the limit, Core does not refuse the one message: the whole
// stream fails, and every transfer, console and tab of every tenant on this
// node with it. A container that answered a tab request with 200 KB of headers
// - or an error text quoting them - did exactly that, as often as it liked.
func boundForCore(msg *pb.NodeMessage) *pb.NodeMessage {
	if n := proto.Size(msg); n > coreMaxMessage {
		log.Printf("gRPC Mesh: a reply of %d bytes for %s is over Core's limit, sending an error instead", n, msg.GetRequestId())
		return errorMsg(msg.GetRequestId(), 502, fmt.Sprintf("the reply was too large to forward (%d bytes)", n))
	}
	return msg
}

// pendingWrite tracks an in-flight file write operation (WriteReq → Chunks → TransferDone).
// Chunks are written directly to a temp file on disk (zero RAM buffering).
type pendingWrite struct {
	serverUUID string
	path       string
	tempFile   *os.File
	tempName   string // relative to the server directory's Root
	lastActive time.Time
}

// MeshManager discovers Core instances via Redis and maintains outbound
// gRPC connections to each one. This creates the full-mesh topology.
type MeshManager struct {
	nodeToken string
	rdb       *redis.Client
	handler   *StreamHandler

	connections   map[string]*coreConnection // coreID → connection
	mu            sync.Mutex
	pendingWrites map[string]*pendingWrite // requestID → write buffer
	writeMu       sync.Mutex
	wsBridges     map[string]*wsBridge // requestID -> open WS bridge (WS5)
	wsMu          sync.Mutex
}

func NewMeshManager(nodeToken string, rdb *redis.Client, handler *StreamHandler) *MeshManager {
	return &MeshManager{
		nodeToken:     nodeToken,
		rdb:           rdb,
		handler:       handler,
		connections:   make(map[string]*coreConnection),
		pendingWrites: make(map[string]*pendingWrite),
		wsBridges:     make(map[string]*wsBridge),
	}
}

// Run starts the discovery loop. Scans Redis every 10s for active Cores.
func (m *MeshManager) Run(ctx context.Context) {
	log.Println("gRPC Mesh: Starting Core discovery loop")

	// Immediate first scan
	m.scanCores(ctx)

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	cleanupTicker := time.NewTicker(60 * time.Second)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case <-ticker.C:
			m.scanCores(ctx)
		case <-cleanupTicker.C:
			m.cleanupStalePendingWrites()
		}
	}
}

// cleanupStalePendingWrites removes pending writes that have been inactive for > 5 minutes.
// This prevents memory/disk leaks from aborted uploads.
func (m *MeshManager) cleanupStalePendingWrites() {
	threshold := time.Now().Add(-5 * time.Minute)
	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	for reqID, pw := range m.pendingWrites {
		if pw.lastActive.Before(threshold) {
			log.Printf("gRPC Mesh: Cleaning up stale upload (request_id=%s, path=%s)", reqID, pw.path)
			pw.tempFile.Close()
			m.handler.removeUploadTemp(pw.serverUUID, pw.path, pw.tempName)
			delete(m.pendingWrites, reqID)
		}
	}
}

// coreIndexKey is Core's set of heartbeating Core ids (services.CoreIndexKey).
const coreIndexKey = "dylaris:core:index"

// coreHeartbeatKeys finds the Cores' heartbeat keys from Core's index, plus a
// keyspace walk for as long as the node may still walk.
//
// The index is what the node will rely on once its login loses SCAN, which
// lists every key NAME on the platform whatever the ACL's key patterns say
// (a link token is all the edge asks of a tunnel). Until then the two are
// UNIONED rather than tried in turn: the index alone hides every Core too old
// to write it, which is half the Cores in the middle of a rolling update and
// all of them after a rollback that left only dead ids behind - and a node
// that sees no Core disconnects from every one it had.
func coreHeartbeatKeys(ctx context.Context, rdb *redis.Client) ([]string, error) {
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	ids, ierr := rdb.SMembers(ctx, coreIndexKey).Result()
	for _, id := range ids {
		add("dylaris:core:" + id)
	}
	var cursor uint64
	for {
		batch, next, serr := rdb.Scan(ctx, cursor, "dylaris:core:*", 100).Result()
		if serr != nil {
			if ierr != nil {
				return nil, serr
			}
			return keys, nil // no walk any more: the index is the answer
		}
		for _, k := range batch {
			add(k)
		}
		cursor = next
		if cursor == 0 {
			return keys, nil
		}
	}
}

func (m *MeshManager) scanCores(ctx context.Context) {
	keys, err := coreHeartbeatKeys(ctx, m.rdb)
	if err != nil {
		log.Printf("gRPC Mesh: no Core index and no scan: %v", err)
		return
	}

	activeCores := make(map[string]bool)

	for _, key := range keys {
		val, err := m.rdb.Get(ctx, key).Result()
		if err != nil {
			continue
		}

		var info CoreInfo
		if err := json.Unmarshal([]byte(val), &info); err != nil {
			continue
		}

		activeCores[info.ID] = true

		m.mu.Lock()
		_, exists := m.connections[info.ID]
		m.mu.Unlock()

		if !exists {
			go m.connectToCore(ctx, info)
		}
	}

	// Remove connections to Cores that are no longer active
	m.mu.Lock()
	for coreID, conn := range m.connections {
		if !activeCores[coreID] {
			log.Printf("gRPC Mesh: Core %s disappeared, disconnecting", coreID)
			conn.cancel()
			conn.conn.Close()
			delete(m.connections, coreID)
		}
	}
	m.mu.Unlock()
}

func (m *MeshManager) connectToCore(parentCtx context.Context, info CoreInfo) {
	// IP Pairing: try private IPs first (500ms timeout), fallback to advertised gRPC addr
	targetAddr := m.resolveAddr(info)

	log.Printf("gRPC Mesh: Connecting to Core %s at %s", info.ID, targetAddr)

	conn, err := grpc.NewClient(targetAddr,
		coreDialCreds(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                20 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		reportCoreProblem("grpc-dial", "cannot build a connection to Core "+info.ID+" at "+targetAddr+": "+grpcErrText(err))
		return
	}

	ctx, cancel := context.WithCancel(parentCtx)

	client := pb.NewNodeServiceClient(conn)
	// grpc.NewClient above is lazy, so this is the first call that actually
	// touches the network - and therefore where a TLS pin mismatch, a refused
	// port or a dead tunnel first appears. Reporting here rather than at the
	// dial is what makes the difference between "node offline" and a named
	// cause reaching the panel.
	stream, err := client.NodeConnect(ctx)
	if err != nil {
		reportCoreProblem("grpc-stream", "cannot open the control stream to Core "+info.ID+" at "+targetAddr+": "+grpcErrText(err))
		cancel()
		conn.Close()
		return
	}

	// Step 1: Send auth
	nodeIPs := getNodeIPs()
	auth := &pb.NodeAuth{
		NodeToken: m.nodeToken,
		Ips: &pb.NodeIPs{
			Public:  nodeIPs.Public,
			Private: nodeIPs.Private,
		},
		// Sent on every connect, not only while bootstrapping: this is the
		// stream that starts failing when the two secrets diverge, and it is
		// the only place Core learns what the machine calls itself.
		Identity: machineIdentity(),
	}
	// The node already holds a secret by the time the mesh runs, so it MUST present
	// a proof. Core refuses an empty proof for a node with a stored secret.
	currentSecret := getNodeSecret()
	if currentSecret != nil {
		auth.AclSupported = true
		auth.SecretProof = aclProof(currentSecret, m.nodeToken)
	}
	// Prove we hold CLUSTER_SECRET so Core will issue this known node its secret
	// on first ACL enablement. Harmless when the node already has a secret.
	if clusterSecret != "" {
		auth.ClusterProof = aclClusterProof(clusterSecret, m.nodeToken)
	}
	// The node's login key; see node_key.go. Presented on every connect so a
	// Core whose row has no key yet can register it.
	key := currentNodeKey()
	auth.NodePublicKey = publicKeyOf(key)
	// Which release this image was built from, so Core can answer a
	// mandatory-update deadline at CONNECT time rather than a heartbeat later.
	// Empty on an unstamped build, which Core reads as unknown and admits.
	if v := nodeReleaseVersion(); !v.IsZero() {
		auth.ReleaseVersion = v.String()
	}
	if err := stream.Send(&pb.NodeMessage{
		Payload: &pb.NodeMessage_Auth{Auth: auth},
	}); err != nil {
		log.Printf("gRPC Mesh: Failed to send auth to Core %s: %v", info.ID, err)
		cancel()
		conn.Close()
		return
	}

	// Step 2: Wait for auth result (answering a challenge nonce if Core sends one)
	authResult, err := recvAuthResult(stream, auth.NodeToken, currentSecret, key)
	if err != nil {
		log.Printf("gRPC Mesh: Failed to receive auth result from Core %s: %v", info.ID, err)
		cancel()
		conn.Close()
		return
	}

	if authResult == nil || !authResult.Ok {
		msg := "unknown"
		if authResult != nil {
			msg = authResult.Message
			// The discovery loop redials within ten seconds with the new key.
			if authResult.NodeKeyRejected {
				replaceRejectedNodeKey(nodeSecretDir, auth.NodePublicKey)
			}
		}
		// Reported like the transport failures: a node rejected at the auth step
		// is online, reachable and still absent from the panel, which from the
		// outside is indistinguishable from an unplugged machine.
		reportCoreProblem("grpc-auth", "Core "+info.ID+" rejected this node's identity: "+msg)
		cancel()
		conn.Close()
		return
	}

	noteUpdateRequirement(authResult)

	log.Printf("gRPC Mesh: Connected to Core %s ✓", info.ID)
	reportCoreRecovered(info.ID)

	// Defensive: persist a refreshed secret if Core handed one back (e.g. ACL
	// newly enabled for this known node, or a reset). No-op when ACL is off.
	// Routed through the guarded setter, not a direct global write: this runs
	// in a per-Core-connection goroutine, so an unsynchronized write here could
	// race (and previously DID race) the watchdog's own read-modify-write and
	// blind it to a real rotation - see redisacl_bootstrap.go. setNodeSecret
	// applies the same change-detection + restart rule no matter which caller
	// triggers it.
	if authResult.NodeSecret != "" {
		if raw, derr := hex.DecodeString(authResult.NodeSecret); derr == nil && len(raw) == 32 {
			setNodeSecret(raw, true)
		}
	}
	// Core's Redis address rides on every auth result. Off this goroutine: a
	// different answer is validated against Redis first, and the read loop below
	// must not wait on that. Every Core replica sends it, and a repeat of the
	// current address is a no-op.
	go noteCoreRedisAddr(parentCtx, authResult.RedisAddr, getNodeSecret())

	// Core's own public host rides on the same message. A pack built in the
	// panel is downloaded from it, and the installer refuses every host it was
	// not told about.
	setCoreMirrorHost(authResult.ModpackMirrorHost)

	// Register connection
	cc := &coreConnection{conn: conn, stream: stream, cancel: cancel, handler: m.handler, pending: make(map[string]chan *pb.NodeMessage)}
	m.mu.Lock()
	m.connections[info.ID] = cc
	m.mu.Unlock()

	// Step 3: Read loop — handle incoming requests from Core
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("gRPC Mesh: Stream error from Core %s: %v", info.ID, err)
			}
			break
		}

		// Core's answer to a request this node started. Never a request for the
		// handler: without this check it would reach Handle's default branch and
		// be answered with "unknown request type".
		if msg.NodeRequest {
			cc.routeNodeResponse(msg)
			continue
		}
		if cc.takeFlowCredit(msg) {
			continue
		}

		// Handled on the read loop to preserve per-request_id ordering
		// (WriteReq -> Chunks -> TransferDone; WsFrame delivery). handleRequest
		// dispatches only the two blocking container dials (HttpProxyReq's Do,
		// WsOpen's handshake) onto their own goroutines so a slow container
		// cannot stall this shared loop - see the notes at those two branches.
		m.handleRequest(cc, msg)
	}

	// Cleanup
	m.mu.Lock()
	delete(m.connections, info.ID)
	m.mu.Unlock()
	// After the map delete, so a new Request cannot pick this connection; one
	// that already did finds pending nil and moves on to another Core.
	cc.closePending()
	cc.closeFlows()
	cancel()
	conn.Close()
	// WS5 I1: tear down every WS bridge this dying connection owned instead
	// of leaving it to leak until its own next I/O error (or forever, on an
	// idle silent container).
	m.closeWSBridgesForConn(cc)
	log.Printf("gRPC Mesh: Disconnected from Core %s", info.ID)
}

func (m *MeshManager) handleRequest(cc *coreConnection, msg *pb.NodeMessage) {
	// WS bridge (WS5): open, or route a frame/close to an existing bridge.
	if open := msg.GetWsOpen(); open != nil {
		cc.openFlow(msg.RequestId, msg.FlowWindow)
		m.handleWSOpen(cc, msg.RequestId, msg.ServerUuid, open)
		return
	}
	if msg.GetWsFrame() != nil || msg.GetWsClose() != nil {
		m.routeWSInbound(msg.RequestId, msg)
		return
	}

	// Handle write data chunks — write directly to temp file on disk
	if chunk := msg.GetChunk(); chunk != nil {
		m.writeMu.Lock()
		if pw, ok := m.pendingWrites[msg.RequestId]; ok {
			pw.lastActive = time.Now()
			if _, err := pw.tempFile.WriteAt(chunk.Data, chunk.Offset); err != nil {
				log.Printf("gRPC Mesh: Write chunk to disk failed (request_id=%s): %v", msg.RequestId, err)
				// Any write failure corrupts the upload. Abort and clean up
				// instead of falling through, which would let a later
				// TransferDone report success on a partial/corrupt file.
				pw.tempFile.Close()
				m.handler.removeUploadTemp(pw.serverUUID, pw.path, pw.tempName)
				delete(m.pendingWrites, msg.RequestId)
				m.writeMu.Unlock()
				if errors.Is(err, syscall.EDQUOT) {
					cc.send(errorMsg(msg.RequestId, 413, "Storage quota exceeded"))
				} else {
					cc.send(errorMsg(msg.RequestId, 500, "Failed to write upload to disk"))
				}
				return
			}
		}
		m.writeMu.Unlock()
		return
	}

	// Handle transfer complete — close temp file, move to final path
	if done := msg.GetTransferDone(); done != nil {
		m.writeMu.Lock()
		pw, ok := m.pendingWrites[msg.RequestId]
		delete(m.pendingWrites, msg.RequestId)
		m.writeMu.Unlock()
		if ok {
			// Truncate to exact size (file may be sparse from WriteAt). On the
			// open file, not by name: the name is the tenant's to swap.
			if done.TotalBytes > 0 {
				pw.tempFile.Truncate(done.TotalBytes)
			}
			pw.tempFile.Close()
			if err := m.handler.commitUpload(pw.serverUUID, pw.path, pw.tempName); err != nil {
				log.Printf("gRPC Mesh: Move file failed (request_id=%s): %v", msg.RequestId, err)
				m.handler.removeUploadTemp(pw.serverUUID, pw.path, pw.tempName)
				if errors.Is(err, syscall.EDQUOT) {
					cc.send(errorMsg(msg.RequestId, 413, "Storage limit reached"))
				} else {
					cc.send(errorMsg(msg.RequestId, 500, err.Error()))
				}
			} else {
				cc.send(&pb.NodeMessage{
					RequestId: msg.RequestId,
					Payload:   &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "written"}},
				})
			}
		}
		return
	}

	// If this is a WriteReq, create temp file and register pending write
	// BEFORE sending response so that chunks arriving immediately after won't be dropped.
	if writeReq := msg.GetWriteReq(); writeReq != nil {
		tempFile, tempName, err := m.handler.createUploadTemp(msg.ServerUuid, writeReq.Path)
		if err != nil {
			log.Printf("gRPC Mesh: Failed to create temp file (request_id=%s): %v", msg.RequestId, err)
			// Still let Handle() process to send error response
		} else {
			m.writeMu.Lock()
			m.pendingWrites[msg.RequestId] = &pendingWrite{
				serverUUID: msg.ServerUuid,
				path:       writeReq.Path,
				tempFile:   tempFile,
				tempName:   tempName,
				lastActive: time.Now(),
			}
			m.writeMu.Unlock()
		}
	}

	// HttpProxyReq (WS5): stream the container HTTP response back over the mesh.
	// The whole request (incl. body) is self-contained in this one message and
	// the node only STREAMS the response back - there are no follow-up inbound
	// messages keyed by this request_id - so dispatch the blocking Do in its own
	// goroutine per request_id (WS5 I3). Otherwise one slow/hung container would
	// stall the shared read loop and every other tenant's messages (uploads,
	// RCON, other tabs) behind it. cc.send is serialized by sendMu, so response
	// streams from concurrent proxy requests never interleave a single message.
	if proxyReq := msg.GetHttpProxyReq(); proxyReq != nil {
		reqID, serverUUID := msg.RequestId, msg.ServerUuid
		cc.openFlow(reqID, msg.FlowWindow)
		go func() {
			defer cc.closeFlow(reqID)
			m.handler.handleHTTPProxy(reqID, serverUUID, proxyReq, cc.send)
		}()
		return
	}

	// ReadReq / SelectiveReadReq / BackupOpenReq: streaming downloads
	// (io.Pipe / file read, constant ~128KB RAM regardless of size).
	//
	// On their own goroutine, like the proxy above. They ran on this shared
	// read loop for the whole transfer, so while one download streamed the node
	// read nothing else from Core - and with flow control the loop has to stay
	// free to read the very grants the download is waiting for. A download
	// takes no further inbound messages under its request_id, so nothing is
	// reordered by moving it.
	if msg.GetReadReq() != nil || msg.GetSelectiveReadReq() != nil || msg.GetBackupOpenReq() != nil {
		reqID, slots := msg.RequestId, downloadSlotsFor(msg.ServerUuid)
		cc.openFlow(reqID, msg.FlowWindow)
		go func() {
			defer cc.closeFlow(reqID)
			slots <- struct{}{}
			defer func() { <-slots }()
			m.handler.HandleStreaming(msg, cc.send)
		}()
		return
	}

	// A WriteReq stays on the loop: the chunks that follow it under its
	// request_id must find the pending write registered above, in order.
	if msg.GetWriteReq() != nil {
		m.sendResponses(cc, msg.RequestId, m.handler.Handle(msg))
		return
	}

	// Everything else (List, Create, Delete, Rename, Copy, hashing, RCON...)
	// is one message in, one answer out, so it runs off the loop. On it, a
	// copy of a large tree, a delete of millions of files, a listing that sums
	// a whole world, or an open that blocked, stopped this node reading
	// anything from that Core - every other server's requests and the flow
	// credits running downloads wait on - until it returned.
	slots := requestSlotsFor(msg.ServerUuid)
	go func() {
		// Bounded: Core gives up on a request after its own timeout, and one
		// that ran anyway once a slot freed would be a delete or a copy the
		// user saw fail, retried, and then got twice.
		wait := time.NewTimer(requestSlotWait)
		defer wait.Stop()
		select {
		case slots <- struct{}{}:
		case <-wait.C:
			m.sendResponses(cc, msg.RequestId, []*pb.NodeMessage{errorMsg(msg.RequestId, 503, "the node is busy with other file operations on this server, try again")})
			return
		}
		defer func() { <-slots }()
		m.sendResponses(cc, msg.RequestId, m.handler.Handle(msg))
	}()
}

func (m *MeshManager) sendResponses(cc *coreConnection, reqID string, responses []*pb.NodeMessage) {
	for _, resp := range responses {
		if err := cc.send(resp); err != nil {
			log.Printf("gRPC Mesh: Failed to send response (request_id=%s): %v", reqID, err)
			return
		}
	}
}

// resolveAddr tries private IPs first (500ms dial timeout), falls back to gRPC addr.
func (m *MeshManager) resolveAddr(info CoreInfo) string {
	for _, ip := range info.IPs.Private {
		// Extract port from the advertised gRPC addr
		_, port, err := net.SplitHostPort(info.GRPCAddr)
		if err != nil {
			continue
		}
		addr := net.JoinHostPort(ip, port)
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			log.Printf("gRPC Mesh: Using private IP %s for Core %s", addr, info.ID)
			return addr
		}
	}
	return info.GRPCAddr
}

func (m *MeshManager) closeAll() {
	m.mu.Lock()
	for id, conn := range m.connections {
		conn.cancel()
		conn.conn.Close()
		delete(m.connections, id)
	}
	m.mu.Unlock()
	// WS5 I1: shutdown must also reap every open WS bridge, not just the
	// Core connections themselves.
	m.closeAllWSBridges()
}

// NodeIPInfo holds the Node's network addresses for auth.
type NodeIPInfo struct {
	Public  string
	Private []string
}

func getNodeIPs() NodeIPInfo {
	info := NodeIPInfo{}

	// Public IP via outbound connection
	if conn, err := net.Dial("udp", "8.8.8.8:80"); err == nil {
		info.Public = conn.LocalAddr().(*net.UDPAddr).IP.String()
		conn.Close()
	} else {
		info.Public, _ = os.Hostname()
	}

	// Private IPs — shared RFC1918 enumeration (privateIPv4s in main.go).
	info.Private = privateIPv4s()

	return info
}
