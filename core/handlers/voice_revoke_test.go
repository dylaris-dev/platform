package handlers

import (
	"context"
	"database/sql"
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
	st := &AppState{Store: fs, GRPCRegistry: reg, Redis: rdb}
	if err := services.PublishVoice(context.Background(), rdb, row); err != nil {
		t.Fatal(err)
	}

	svc := services.NewVoiceService(fs, rdb)
	svc.SetRevoke(func(ctx context.Context, srv *models.Server) error { return RevokeServerVoice(ctx, st, srv) })
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
			writeBackVoiceConfig(st, srv)
			node.mu.Lock()
			wrote := node.writes > 0
			node.mu.Unlock()
			if wrote != c.wantWrite {
				t.Fatalf("wrote = %v, want %v", wrote, c.wantWrite)
			}
		})
	}
}
