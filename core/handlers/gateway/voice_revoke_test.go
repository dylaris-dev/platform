package gateway

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/services"
	"dylaris-core/store"

	pb "dylaris-proto/node"
)

// hungNode accepts every request and never answers one.
type hungNode struct {
	grpc.ServerStream
	mu    sync.Mutex
	reads int
}

func (s *hungNode) Context() context.Context       { return context.Background() }
func (s *hungNode) Recv() (*pb.NodeMessage, error) { select {} }
func (s *hungNode) Send(m *pb.NodeMessage) error {
	if m.GetReadReq() != nil {
		s.mu.Lock()
		s.reads++
		s.mu.Unlock()
	}
	return nil
}

func (s *hungNode) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// voiceRevokeStore holds one voice row for a server nothing allows voice.
type voiceRevokeStore struct {
	store.Store
	mu  sync.Mutex
	row *store.VoicePort
	srv *models.Server
}

func (f *voiceRevokeStore) ListVoicePorts() ([]store.VoicePort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.row == nil {
		return nil, nil
	}
	return []store.VoicePort{*f.row}, nil
}

func (f *voiceRevokeStore) GetVoicePort(int) (*store.VoicePort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.row == nil {
		return nil, nil
	}
	r := *f.row
	return &r, nil
}

func (f *voiceRevokeStore) DeleteVoicePort(int) error {
	f.mu.Lock()
	f.row = nil
	f.mu.Unlock()
	return nil
}

func (f *voiceRevokeStore) GetServerByID(int) (*models.Server, error) { return f.srv, nil }
func (f *voiceRevokeStore) GetVoicePermission(string) (*bool, error)  { return nil, nil }
func (f *voiceRevokeStore) GetSetting(string) (string, error)         { return "", sql.ErrNoRows }

// A revocation must not wait on the node: a BYON node that never answers the
// config read would otherwise stall the reconcile goroutine until Core restarts.
func TestReconcileRevocationDoesNotWaitOnAHungNode(t *testing.T) {
	old := voiceWriteBackTimeout
	voiceWriteBackTimeout = 100 * time.Millisecond
	t.Cleanup(func() { voiceWriteBackTimeout = old })

	row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
	fs := &voiceRevokeStore{row: &row, srv: &models.Server{ID: 7, UUID: "srv-7", NodeID: 7, ActiveSubServer: "survival"}}
	reg := nodegrpc.NewRegistry()
	node := &hungNode{}
	reg.Register(7, "tok", node)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	if err := services.PublishVoice(context.Background(), rdb, row); err != nil {
		t.Fatal(err)
	}

	svc := services.NewVoiceService(fs, rdb)
	svc.SetRevoke(func(ctx context.Context, srv *models.Server) error { return RevokeServerVoice(ctx, fs, rdb, reg, srv) })
	done := make(chan error, 1)
	go func() { done <- svc.Reconcile(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reconcile blocked on a node that never answers")
	}
	if fs.row != nil {
		t.Fatal("the voice row survived the revocation")
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("keys survived the revocation: %v", keys)
	}
	// The write-back did start: the hang is the node's, not a skipped call.
	deadline := time.Now().Add(2 * time.Second)
	for node.readCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if node.readCount() == 0 {
		t.Fatal("the config write-back never reached the node")
	}
}

type voiceWriteBackStore struct {
	store.Store
	row *store.VoicePort
}

func (f *voiceWriteBackStore) GetVoicePort(int) (*store.VoicePort, error) { return f.row, nil }

// A disable's config write-back can land long after the disable: the node may
// be slow. If voice was turned on again meanwhile, writing the "off" config
// would leave the server with a voice port and a mod that does not use it.
func TestALateWriteBackDoesNotUndoAReEnable(t *testing.T) {
	srv := &models.Server{ID: 7, UUID: "srv-7", NodeID: 7, ActiveSubServer: "survival"}
	path := "plugins/voicechat/voicechat-server.properties"
	cases := []struct {
		name      string
		row       *store.VoicePort
		wantWrite bool
	}{
		{"still off: the address is stripped", nil, true},
		{"on again: the config is left alone", &store.VoicePort{ServerID: 7, Region: "eu", Port: 26000}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, _, node := voiceNodeState(t, &voiceWriteBackStore{row: c.row}, path, "svc_after.properties")
			writeBackVoiceConfig(st.Store, st.GRPCRegistry, srv)
			wrote := node.WriteCount() > 0
			if wrote != c.wantWrite {
				t.Fatalf("wrote = %v, want %v", wrote, c.wantWrite)
			}
		})
	}
}

func (f *voiceRevokeStore) ListLinkRouteVoice() ([]store.LinkRouteVoice, error) { return nil, nil }

// The fall back's voice host write lands only while the row still says that
// host on that port: a name the customer activated meanwhile, voice turned
// off, or a move must win over a write decided before it.
func TestRewriteVoiceHostRereadsTheRow(t *testing.T) {
	srv := &models.Server{ID: 7, UUID: "srv-7", NodeID: 7, ActiveSubServer: "survival"}
	path := "plugins/voicechat/voicechat-server.properties"
	ours := "voice.eu.dylaris.com"
	row := func(name, state string, port int) *store.VoicePort {
		return &store.VoicePort{ServerID: 7, Region: "eu", Port: port, Host: ours, PublicHost: name, PublicState: state}
	}
	cases := []struct {
		name      string
		row       *store.VoicePort
		wantWrite bool
	}{
		{"still fallen back: our name is written", row("voice.play.example.com", store.VoicePublicFallenBack, 26000), true},
		{"another name active meanwhile", row("talk.play.example.com", store.VoicePublicActive, 26000), false},
		{"voice off meanwhile", nil, false},
		{"moved to another port", row("voice.play.example.com", store.VoicePublicFallenBack, 26001), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, _, node := voiceNodeState(t, &voiceWriteBackStore{row: c.row}, path, "svc_before.properties")
			rewriteVoiceHost(st.Store, st.GRPCRegistry, srv, ours, 26000)
			if wrote := node.WriteCount() > 0; wrote != c.wantWrite {
				t.Fatalf("wrote = %v, want %v", wrote, c.wantWrite)
			}
			if c.wantWrite && !strings.Contains(node.WrittenString(), ours) {
				t.Fatalf("written %q, want our name", node.WrittenString())
			}
		})
	}
}

// The write runs in the background: a node that never answers does not hold
// the caller (the reconcile goroutine).
func TestRewriteVoiceHostInBackgroundDoesNotWait(t *testing.T) {
	old := voiceWriteBackTimeout
	voiceWriteBackTimeout = 100 * time.Millisecond
	t.Cleanup(func() { voiceWriteBackTimeout = old })
	srv := &models.Server{ID: 7, UUID: "srv-7", NodeID: 7, ActiveSubServer: "survival"}
	reg := nodegrpc.NewRegistry()
	node := &hungNode{}
	reg.Register(7, "tok", node)
	fs := &voiceWriteBackStore{row: &store.VoicePort{ServerID: 7, Port: 26000, Host: "voice.eu.dylaris.com"}}
	start := time.Now()
	RewriteVoiceHostInBackground(fs, reg, srv, "voice.eu.dylaris.com", 26000)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the caller waited %s", d)
	}
	deadline := time.Now().Add(2 * time.Second)
	for node.readCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if node.readCount() == 0 {
		t.Fatal("the write never reached the node")
	}
}
