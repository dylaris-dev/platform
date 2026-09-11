package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"dylaris-pkg/nodeauth"
	pb "dylaris-proto/node"

	"google.golang.org/grpc"
)

func TestNodeKeyIsGeneratedOnceAndReused(t *testing.T) {
	// Not created yet, as on a first boot.
	dir := filepath.Join(t.TempDir(), "storage")
	k1, err := loadOrCreateNodeKey(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	k2, err := loadOrCreateNodeKey(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !k1.Equal(k2) {
		t.Fatal("a second boot generated a different key; the identity must survive a restart")
	}
	path := filepath.Join(dir, nodeKeyFileName)
	if runtime.GOOS != "windows" { // Windows has no Unix modes to check
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
			t.Errorf("mode = %v (%v), want 0600", info.Mode().Perm(), err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != hex.EncodeToString(k1.Seed()) {
		t.Errorf("the file does not hold the hex seed (%v)", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want only %s; a temp file was left behind", len(entries), nodeKeyFileName)
	}
}

// Overwriting a file the node cannot read would silently replace its identity.
func TestAMalformedNodeKeyIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, nodeKeyFileName)
	if err := os.WriteFile(path, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateNodeKey(dir); err == nil {
		t.Fatal("a malformed key file was accepted")
	}
	if b, _ := os.ReadFile(path); string(b) != "not a key" {
		t.Errorf("the malformed file was overwritten with %q", b)
	}
}

// A .node_id that is there but unusable must not read as "no identity": the
// fallback is dialling Core as somebody else.
func TestCachedNodeID(t *testing.T) {
	dir := t.TempDir()
	if id, err := cachedNodeID(dir); id != "" || err != nil {
		t.Errorf("no file = (%q, %v), want ('', nil)", id, err)
	}
	if err := saveNodeID(dir, "assigned-uuid"); err != nil {
		t.Fatal(err)
	}
	if id, err := cachedNodeID(dir); id != "assigned-uuid" || err != nil {
		t.Errorf("= (%q, %v), want the saved id", id, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".node_id"), []byte("  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cachedNodeID(dir); err == nil {
		t.Error("an empty .node_id read as no identity")
	}
	if _, ok := loadNodeID(dir); ok {
		t.Error("loadNodeID accepted an empty .node_id")
	}
}

// coreSide plays Core for recvAuthResult.
type coreSide struct {
	grpc.ClientStream
	in   []*pb.NodeMessage
	sent []*pb.NodeMessage
}

func (s *coreSide) Send(m *pb.NodeMessage) error { s.sent = append(s.sent, m); return nil }

func (s *coreSide) Recv() (*pb.NodeMessage, error) {
	if len(s.in) == 0 {
		return nil, io.EOF
	}
	m := s.in[0]
	s.in = s.in[1:]
	return m, nil
}

// The node cannot know which proof a Core reads - one that predates keys reads
// the HMAC, a current one the signature - so it answers with every proof it
// holds, in their own fields.
func TestRecvAuthResultAnswersEveryProofItHolds(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	_, key, _ := ed25519.GenerateKey(nil)
	const nonce = "a-core-nonce"
	script := func() []*pb.NodeMessage {
		return []*pb.NodeMessage{
			{Payload: &pb.NodeMessage_Challenge{Challenge: &pb.NodeChallenge{Nonce: nonce}}},
			{Payload: &pb.NodeMessage_AuthResult{AuthResult: &pb.AuthResult{Ok: true}}},
		}
	}
	for _, c := range []struct {
		name   string
		secret []byte
		key    ed25519.PrivateKey
	}{
		{"secret and key", secret, key},
		{"secret only, an image before keys", secret, nil},
		{"key only, a node that lost its secret", nil, key},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := &coreSide{in: script()}
			ar, err := recvAuthResult(s, "node-abc", c.secret, c.key)
			if err != nil || ar == nil || !ar.Ok {
				t.Fatalf("recvAuthResult = (%v, %v)", ar, err)
			}
			cr := s.sent[0].GetChallengeResponse()
			if want := ""; c.secret != nil {
				if cr.Response != aclChallengeResponse(c.secret, nonce) {
					t.Error("the HMAC answer is not the one an older Core verifies")
				}
			} else if cr.Response != want {
				t.Errorf("answered an HMAC %q without a secret", cr.Response)
			}
			if c.key != nil {
				if !nodeauth.VerifyChallenge(publicKeyOf(c.key), "node-abc", nonce, cr.Signature) {
					t.Error("the signature does not verify under the node's key and identity")
				}
			} else if len(cr.Signature) != 0 {
				t.Error("answered a signature without a key")
			}
		})
	}
	t.Run("neither", func(t *testing.T) {
		if _, err := recvAuthResult(&coreSide{in: script()}, "node-abc", nil, nil); err == nil {
			t.Error("answered a challenge with nothing to prove")
		}
	})
}

// A node dials every Core replica at once and each answers "rejected" for the
// same key. Only the first answer may replace it; a second replacement would
// register one key and present another.
func TestReplaceRejectedNodeKeyReplacesOnlyTheRejectedKey(t *testing.T) {
	dir := t.TempDir()
	t.Cleanup(func() { setNodeKey(nil) })
	k1, err := loadOrCreateNodeKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	setNodeKey(k1)
	rejected := publicKeyOf(k1)

	replaceRejectedNodeKey(dir, rejected)
	k2 := currentNodeKey()
	if k2.Equal(k1) {
		t.Fatal("the rejected key was kept")
	}
	if onDisk, err := loadOrCreateNodeKey(dir); err != nil || !onDisk.Equal(k2) {
		t.Fatalf("the new key is not the one on disk (%v)", err)
	}
	replaceRejectedNodeKey(dir, rejected)
	if !currentNodeKey().Equal(k2) {
		t.Error("a second rejection of the OLD key replaced the new one")
	}
}

// fakeCore answers each connection with the next step of its script and keeps
// what every connection presented.
type fakeCore struct {
	pb.UnimplementedNodeServiceServer
	mu     sync.Mutex
	script []func(*pb.NodeAuth, pb.NodeService_NodeConnectServer) error
	auths  []*pb.NodeAuth
}

func (c *fakeCore) NodeConnect(stream pb.NodeService_NodeConnectServer) error {
	m, err := stream.Recv()
	if err != nil {
		return err
	}
	c.mu.Lock()
	step := c.script[len(c.auths)]
	c.auths = append(c.auths, m.GetAuth())
	c.mu.Unlock()
	return step(m.GetAuth(), stream)
}

func (c *fakeCore) presented(i int) *pb.NodeAuth {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.auths[i]
}

func sendResult(stream pb.NodeService_NodeConnectServer, ar *pb.AuthResult) error {
	return stream.Send(&pb.NodeMessage{Payload: &pb.NodeMessage_AuthResult{AuthResult: ar}})
}

// acceptByKey admits a connection only on a valid signature by the key it
// presented, and hands it secret.
func acceptByKey(secret []byte) func(*pb.NodeAuth, pb.NodeService_NodeConnectServer) error {
	return func(auth *pb.NodeAuth, stream pb.NodeService_NodeConnectServer) error {
		const nonce = "nonce-from-the-fake-core"
		if err := stream.Send(&pb.NodeMessage{Payload: &pb.NodeMessage_Challenge{Challenge: &pb.NodeChallenge{Nonce: nonce}}}); err != nil {
			return err
		}
		m, err := stream.Recv()
		if err != nil {
			return err
		}
		if !nodeauth.VerifyChallenge(auth.GetNodePublicKey(), auth.GetNodeToken(), nonce, m.GetChallengeResponse().GetSignature()) {
			return sendResult(stream, &pb.AuthResult{Ok: false, Message: "bad challenge response"})
		}
		return sendResult(stream, &pb.AuthResult{Ok: true, NodeSecret: hex.EncodeToString(secret)})
	}
}

func rejectKey(_ *pb.NodeAuth, stream pb.NodeService_NodeConnectServer) error {
	return sendResult(stream, &pb.AuthResult{Ok: false, NodeKeyRejected: true, Message: "an operator replaced this node's key"})
}

// useFakeCore serves core on loopback and points the node's package state at
// it and at dir, restoring everything afterwards.
func useFakeCore(t *testing.T, core *fakeCore, dir string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterNodeServiceServer(srv, core)
	go func() { _ = srv.Serve(lis) }()

	prevDir, prevID, prevAddr, prevCluster, prevEnroll := nodeSecretDir, nodeID, coreGRPCAddr, clusterSecret, nodeEnrollToken
	t.Cleanup(func() {
		srv.Stop()
		nodeSecretDir, nodeID, coreGRPCAddr, clusterSecret, nodeEnrollToken = prevDir, prevID, prevAddr, prevCluster, prevEnroll
		setNodeKey(nil)
		setNodeSecretForTest(nil)
	})
	nodeSecretDir, coreGRPCAddr, clusterSecret, nodeEnrollToken = dir, lis.Addr().String(), "", ""
}

// Every connect presents the cached .node_id, never the configured NODE_ID or
// the hostname. And a node that lost its secret but kept its key gets the
// secret back by signing, with no secret proof sent.
func TestBootstrapPresentsTheCachedIdentityAndItsKey(t *testing.T) {
	dir := t.TempDir()
	if err := saveNodeID(dir, "assigned-uuid"); err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{0x5a}, 32)
	core := &fakeCore{script: []func(*pb.NodeAuth, pb.NodeService_NodeConnectServer) error{acceptByKey(secret)}}
	useFakeCore(t, core, dir)
	nodeID = "the-hostname"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if got := ensureNodeSecret(ctx); !bytes.Equal(got, secret) {
		t.Fatalf("ensureNodeSecret = %x, want Core's secret", got)
	}
	auth := core.presented(0)
	if auth.NodeToken != "assigned-uuid" {
		t.Errorf("presented %q, want the cached .node_id", auth.NodeToken)
	}
	if auth.SecretProof != "" {
		t.Error("a node with no cached secret sent a secret proof")
	}
	onDisk, err := loadOrCreateNodeKey(dir)
	if err != nil || !bytes.Equal(publicKeyOf(onDisk), auth.NodePublicKey) {
		t.Errorf("presented a key that is not the one on disk (%v)", err)
	}
}

// Told its key was replaced, the node makes a new one, keeps it, and the retry
// presents it.
func TestARejectedKeyIsReplacedAndTheRetryPresentsTheNewOne(t *testing.T) {
	dir := t.TempDir()
	secret := bytes.Repeat([]byte{0x5a}, 32)
	core := &fakeCore{script: []func(*pb.NodeAuth, pb.NodeService_NodeConnectServer) error{rejectKey, acceptByKey(secret)}}
	useFakeCore(t, core, dir)
	nodeID = "assigned-uuid"
	k1, err := loadOrCreateNodeKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	setNodeKey(k1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Every caller retries on an error; this is the loop's two turns.
	if _, err := bootstrapSecretViaGRPC(ctx, false); err == nil {
		t.Fatal("a rejected key reported success")
	}
	got, err := bootstrapSecretViaGRPC(ctx, false)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("retry = (%x, %v), want Core's secret", got, err)
	}
	first, second := core.presented(0).NodePublicKey, core.presented(1).NodePublicKey
	if !bytes.Equal(first, publicKeyOf(k1)) {
		t.Error("the first attempt did not present the node's key")
	}
	if bytes.Equal(second, first) {
		t.Fatal("the retry presented the rejected key again")
	}
	if onDisk, err := loadOrCreateNodeKey(dir); err != nil || !bytes.Equal(publicKeyOf(onDisk), second) {
		t.Errorf("the new key was not persisted (%v)", err)
	}
}
