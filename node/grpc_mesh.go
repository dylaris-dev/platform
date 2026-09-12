package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
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
}

// send serializes all writes to this Core stream. gRPC streams are not safe
// for concurrent Send; the WS bridge introduced the first background sender.
func (cc *coreConnection) send(msg *pb.NodeMessage) error {
	cc.sendMu.Lock()
	defer cc.sendMu.Unlock()
	return cc.stream.Send(msg)
}

// pendingWrite tracks an in-flight file write operation (WriteReq → Chunks → TransferDone).
// Chunks are written directly to a temp file on disk (zero RAM buffering).
type pendingWrite struct {
	serverUUID string
	path       string
	tempFile   *os.File
	tempPath   string
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
			os.Remove(pw.tempPath)
			delete(m.pendingWrites, reqID)
		}
	}
}

func (m *MeshManager) scanCores(ctx context.Context) {
	// SCAN, not KEYS: the node's scoped Redis ACL grants SCAN but denies KEYS
	// (it is in @dangerous). KEYS here returned NOPERM, so Core discovery never
	// ran and the node<->Core gRPC connection was never established — file
	// transfers to the node then fail with "Node not connected".
	var keys []string
	var cursor uint64
	for {
		batch, next, err := m.rdb.Scan(ctx, cursor, "dylaris:core:*", 100).Result()
		if err != nil {
			log.Printf("gRPC Mesh: Redis scan error: %v", err)
			return
		}
		keys = append(keys, batch...)
		cursor = next
		if cursor == 0 {
			break
		}
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

	// Register connection
	cc := &coreConnection{conn: conn, stream: stream, cancel: cancel, handler: m.handler}
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
				os.Remove(pw.tempPath)
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
			pw.tempFile.Close()
			// Truncate to exact size (file may be sparse from WriteAt)
			if done.TotalBytes > 0 {
				os.Truncate(pw.tempPath, done.TotalBytes)
			}
			finalPath, err := m.handler.resolveFinalPath(pw.serverUUID, pw.path)
			if err != nil {
				log.Printf("gRPC Mesh: Resolve path failed (request_id=%s): %v", msg.RequestId, err)
				os.Remove(pw.tempPath)
				cc.send(errorMsg(msg.RequestId, 500, err.Error()))
			} else if err := os.Rename(pw.tempPath, finalPath); err != nil {
				log.Printf("gRPC Mesh: Move file failed (request_id=%s): %v", msg.RequestId, err)
				os.Remove(pw.tempPath)
				if errors.Is(err, syscall.EDQUOT) {
					cc.send(errorMsg(msg.RequestId, 413, "Speicherlimit erreicht"))
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
		tempFile, err := m.handler.createUploadTemp(msg.ServerUuid, writeReq.Path)
		if err != nil {
			log.Printf("gRPC Mesh: Failed to create temp file (request_id=%s): %v", msg.RequestId, err)
			// Still let Handle() process to send error response
		} else {
			m.writeMu.Lock()
			m.pendingWrites[msg.RequestId] = &pendingWrite{
				serverUUID: msg.ServerUuid,
				path:       writeReq.Path,
				tempFile:   tempFile,
				tempPath:   tempFile.Name(),
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
		go m.handler.handleHTTPProxy(reqID, serverUUID, proxyReq, cc.send)
		return
	}

	// ReadReq / SelectiveReadReq / BackupOpenReq: streaming downloads
	// (io.Pipe / file read, constant ~128KB RAM regardless of size).
	if msg.GetReadReq() != nil || msg.GetSelectiveReadReq() != nil || msg.GetBackupOpenReq() != nil {
		m.handler.HandleStreaming(msg, cc.send)
		return
	}

	// Normal request handling (List, Write, Create, Delete, Rename, Copy)
	responses := m.handler.Handle(msg)
	for _, resp := range responses {
		if err := cc.send(resp); err != nil {
			log.Printf("gRPC Mesh: Failed to send response (request_id=%s): %v", msg.RequestId, err)
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
