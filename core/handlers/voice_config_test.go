package handlers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	nodegrpc "dylaris-core/grpc"
	"dylaris-core/models"
	"dylaris-core/store"

	pb "dylaris-proto/node"
)

func readVoiceFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "voice", name))
	if err != nil {
		t.Fatal(err)
	}
	// core.autocrlf may check the fixtures out with CRLF; the writers emit LF.
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func TestDetectVoiceMod(t *testing.T) {
	svcMod := "config/voicechat/voicechat-server.properties"
	svcPlugin := "plugins/voicechat/voicechat-server.properties"
	plasmoPlugin := "plugins/PlasmoVoice/config.toml"
	cases := []struct {
		name     string
		found    []string
		wantMod  string
		wantPath string
		wantErr  error
	}{
		{"no config at all is a missing mod", nil, "", "", errVoiceModMissing},
		{"one svc config is svc", []string{svcPlugin}, "svc", svcPlugin, nil},
		{"one plasmo config is plasmo", []string{plasmoPlugin}, "plasmo", plasmoPlugin, nil},
		{"svc next to plasmo is ambiguous", []string{svcMod, plasmoPlugin}, "", "", errVoiceModAmbiguous},
		{"both svc paths are ambiguous", []string{svcMod, svcPlugin}, "", "", errVoiceModAmbiguous},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mod, path, err := detectVoiceMod(c.found)
			if !errors.Is(err, c.wantErr) || mod != c.wantMod || path != c.wantPath {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", mod, path, err, c.wantMod, c.wantPath, c.wantErr)
			}
			if c.wantErr == errVoiceModAmbiguous {
				for _, f := range c.found {
					if !strings.Contains(err.Error(), f) {
						t.Fatalf("error %q does not name %s", err, f)
					}
				}
			}
		})
	}
}

func TestSvcVoiceGolden(t *testing.T) {
	before := readVoiceFixture(t, "svc_before.properties")
	after := readVoiceFixture(t, "svc_after.properties")

	if got := svcVoiceEnable(before, "a.eu.dylaris.com", 26000); got != after {
		t.Fatalf("enable:\n%s\nwant:\n%s", got, after)
	}
	if got := svcVoiceDisable(after); got != before {
		t.Fatalf("disable did not give the default back:\n%s", got)
	}
	// A server that shared the game port gets the in-container port back.
	moved := strings.Replace(before, "port=24454", "port=-1", 1)
	if got := svcVoiceEnable(moved, "a.eu.dylaris.com", 26000); got != after {
		t.Fatalf("enable on port=-1:\n%s", got)
	}
}

func TestPlasmoVoiceGolden(t *testing.T) {
	def := readVoiceFixture(t, "plasmo_default.toml")
	after := readVoiceFixture(t, "plasmo_after.toml")
	existing := readVoiceFixture(t, "plasmo_existing_public.toml")
	disabled := readVoiceFixture(t, "plasmo_disabled.toml")

	if got := plasmoVoiceEnable(def, "a.eu.dylaris.com", 26000); got != after {
		t.Fatalf("enable on the default:\n%s\nwant:\n%s", got, after)
	}
	if got := plasmoVoiceEnable(existing, "a.eu.dylaris.com", 26000); got != after {
		t.Fatalf("enable on an active [host.public]:\n%s\nwant:\n%s", got, after)
	}
	if got := plasmoVoiceDisable(after); got != disabled {
		t.Fatalf("disable:\n%s\nwant:\n%s", got, disabled)
	}
	if !strings.Contains(disabled, "#[host.public]\n#ip = \"127.0.0.1\"\n#port = 0\n") {
		t.Fatal("the null block is no longer a comment")
	}
}

func TestPlasmoVoiceEnableWithoutHostAppendsBoth(t *testing.T) {
	in := "debug = false\n\n[voice]\nmtu_size = 1024\n"
	want := in + "\n[host]\nport = 24454\n\n[host.public]\nip = \"a.eu.dylaris.com\"\nport = 26000\n"
	if got := plasmoVoiceEnable(in, "a.eu.dylaris.com", 26000); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlasmoVoiceEnableInsertsAfterHostSubtables(t *testing.T) {
	in := "[host]\nport = 0\n\n[host.extra]\nx = 1\n\n[voice]\nmtu_size = 1024\n"
	want := "[host]\nport = 24454\n\n[host.extra]\nx = 1\n\n[host.public]\nip = \"a.eu.dylaris.com\"\nport = 26000\n\n[voice]\nmtu_size = 1024\n"
	if got := plasmoVoiceEnable(in, "a.eu.dylaris.com", 26000); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlasmoMergeIsIdempotent(t *testing.T) {
	for _, name := range []string{"plasmo_default.toml", "plasmo_after.toml", "plasmo_existing_public.toml"} {
		t.Run(name, func(t *testing.T) {
			in := readVoiceFixture(t, name)
			once := plasmoVoiceEnable(in, "a.eu.dylaris.com", 26000)
			if twice := plasmoVoiceEnable(once, "a.eu.dylaris.com", 26000); twice != once {
				t.Fatalf("second merge changed the file:\n%s", twice)
			}
			off := plasmoVoiceDisable(once)
			if again := plasmoVoiceDisable(off); again != off {
				t.Fatalf("second disable changed the file:\n%s", again)
			}
		})
	}
}

type voiceSyncFakeStore struct {
	store.Store
	row   *store.VoicePort
	calls int
}

func (f *voiceSyncFakeStore) GetVoicePort(int) (*store.VoicePort, error) {
	f.calls++
	return f.row, nil
}

func TestSyncVoiceWithoutRowTouchesNothing(t *testing.T) {
	fs := &voiceSyncFakeStore{}
	// A nil GRPCRegistry fails any node call, so true means none was made.
	h := NewServerHandler(&AppState{Store: fs})
	if !h.syncVoiceToSubServer(&models.Server{ID: 7, UUID: "u"}, "survival") {
		t.Fatal("a server without a voice row reported a failed sync")
	}
	if fs.calls != 1 {
		t.Fatalf("voice row read %d times, want 1", fs.calls)
	}
}

func TestSyncVoiceWithRowReachesForTheNode(t *testing.T) {
	fs := &voiceSyncFakeStore{row: &store.VoicePort{ServerID: 7, Region: "eu", Port: 26000, Host: "a.eu.dylaris.com"}}
	h := NewServerHandler(&AppState{Store: fs})
	if h.syncVoiceToSubServer(&models.Server{ID: 7, UUID: "u"}, "survival") {
		t.Fatal("a voice row with no reachable node reported success")
	}
}

// voiceNode serves one voice config at path and records what is written, so a
// sync gets past the node and reaches the store and Redis.
type voiceNode struct {
	grpc.ServerStream
	mu      sync.Mutex
	conn    *nodegrpc.NodeConnection
	path    string
	also    string // a second path served with the same file
	file    string
	written strings.Builder
	// failWrite answers every write request with an error.
	failWrite bool
	writes    int // write requests seen
}

func (s *voiceNode) Context() context.Context       { return context.Background() }
func (s *voiceNode) Recv() (*pb.NodeMessage, error) { select {} }
func (s *voiceNode) Send(m *pb.NodeMessage) error {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	id := m.RequestId
	var replies []*pb.NodeMessage
	switch {
	case m.GetChunk() != nil:
		s.mu.Lock()
		s.written.Write(m.GetChunk().Data)
		s.mu.Unlock()
	case m.GetReadReq() != nil && (strings.HasSuffix(m.GetReadReq().Path, "/"+s.path) || (s.also != "" && strings.HasSuffix(m.GetReadReq().Path, "/"+s.also))):
		replies = []*pb.NodeMessage{
			{RequestId: id, Payload: &pb.NodeMessage_Chunk{Chunk: &pb.DataChunk{Data: []byte(s.file)}}},
			{RequestId: id, Payload: &pb.NodeMessage_TransferDone{TransferDone: &pb.TransferDone{TotalBytes: int64(len(s.file))}}},
		}
	case m.GetReadReq() != nil:
		replies = []*pb.NodeMessage{{RequestId: id, Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 404, Message: "not found"}}}}
	case m.GetWriteReq() != nil && s.countWrite() && s.failWrite:
		replies = []*pb.NodeMessage{{RequestId: id, Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 500, Message: "disk full"}}}}
	case m.GetWriteReq() != nil, m.GetTransferDone() != nil:
		replies = []*pb.NodeMessage{{RequestId: id, Payload: &pb.NodeMessage_Result{Result: &pb.OpResult{Message: "ok"}}}}
	}
	go func() {
		for _, r := range replies {
			conn.RouteResponse(r)
		}
		// The server's read loop closes a finished read stream; do the same.
		if m.GetReadReq() != nil {
			conn.CloseStreamingRequest(id)
		}
	}()
	return nil
}

func (s *voiceNode) countWrite() bool {
	s.mu.Lock()
	s.writes++
	s.mu.Unlock()
	return true
}

type voiceRaceStore struct {
	store.Store
	row     store.VoicePort  // what the sync reads
	current *store.VoicePort // what the table holds by the time it writes; nil = deleted
	updates int
	allocs  int
	mod     string
}

func (f *voiceRaceStore) GetVoicePort(int) (*store.VoicePort, error) {
	r := f.row
	return &r, nil
}

func (f *voiceRaceStore) UpdateVoicePortMod(serverID int, region string, port int, mod string, internalPort int, host string) (bool, error) {
	f.updates++
	f.mod = mod
	c := f.current
	return c != nil && c.ServerID == serverID && c.Region == region && c.Port == port, nil
}

func (f *voiceRaceStore) AllocateVoicePort(store.VoicePort, int, int) (store.VoicePort, error) {
	f.allocs++
	return store.VoicePort{}, nil
}

func voiceRaceState(t *testing.T, fs *voiceRaceStore) (*AppState, *miniredis.Miniredis) {
	t.Helper()
	st, mr, _ := voiceNodeState(t, fs, "config/voicechat/voicechat-server.properties", "svc_before.properties")
	return st, mr
}

func voiceNodeState(t *testing.T, fs store.Store, path, fixture string) (*AppState, *miniredis.Miniredis, *voiceNode) {
	t.Helper()
	reg := nodegrpc.NewRegistry()
	node := &voiceNode{path: path, file: readVoiceFixture(t, fixture)}
	conn := reg.Register(7, "tok", node)
	node.mu.Lock()
	node.conn = conn
	node.mu.Unlock()
	mr := miniredis.RunT(t)
	return &AppState{Store: fs, GRPCRegistry: reg, Redis: redis.NewClient(&redis.Options{Addr: mr.Addr()})}, mr, node
}

func TestSyncVoiceRaceNeverCreatesOrPublishes(t *testing.T) {
	row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
	moved, movedPort, same := row, row, row
	moved.Region = "us"
	movedPort.Port = 26001
	cases := []struct {
		name        string
		current     *store.VoicePort
		wantPublish bool
	}{
		{"a row deleted by a disable after the read is not resurrected", nil, false},
		{"a row moved to another region after the read is not reverted", &moved, false},
		{"a row moved to another port after the read is not reverted", &movedPort, false},
		{"a row that is still there is refreshed and published", &same, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := &voiceRaceStore{row: row, current: c.current}
			st, mr := voiceRaceState(t, fs)
			ok := NewServerHandler(st).syncVoiceToSubServer(&models.Server{ID: 7, UUID: "srv-7", NodeID: 7}, "survival")
			if ok != c.wantPublish {
				t.Fatalf("sync = %v, want %v", ok, c.wantPublish)
			}
			if fs.allocs != 0 {
				t.Fatalf("AllocateVoicePort called %d times; the re-assert must never insert or move a row", fs.allocs)
			}
			if fs.updates != 1 {
				t.Fatalf("UpdateVoicePortMod called %d times, want 1", fs.updates)
			}
			if got := len(mr.Keys()) > 0; got != c.wantPublish {
				t.Fatalf("published = %v (keys %v), want %v", got, mr.Keys(), c.wantPublish)
			}
		})
	}
}

func TestSyncVoiceWritesTheDetectedModsMerge(t *testing.T) {
	cases := []struct {
		name, path, before, after, mod string
	}{
		{"svc gets the properties merge", "plugins/voicechat/voicechat-server.properties", "svc_before.properties", "svc_after.properties", "svc"},
		{"plasmo gets the toml merge", "config/plasmovoice/config.toml", "plasmo_default.toml", "plasmo_after.toml", "plasmo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := store.VoicePort{ServerID: 7, ServerUUID: "srv-7", Region: "eu", Port: 26000, Host: "a.eu.dylaris.com", UplinkKey: make([]byte, 32)}
			fs := &voiceRaceStore{row: row, current: &row}
			st, _, node := voiceNodeState(t, fs, c.path, c.before)
			if !NewServerHandler(st).syncVoiceToSubServer(&models.Server{ID: 7, UUID: "srv-7", NodeID: 7}, "survival") {
				t.Fatal("sync failed")
			}
			node.mu.Lock()
			got := node.written.String()
			node.mu.Unlock()
			if want := readVoiceFixture(t, c.after); got != want {
				t.Fatalf("wrote:\n%s\nwant:\n%s", got, want)
			}
			if fs.mod != c.mod {
				t.Fatalf("stored mod %q, want %q", fs.mod, c.mod)
			}
		})
	}
}
