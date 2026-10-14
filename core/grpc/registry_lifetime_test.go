package nodegrpc

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"

	pb "dylaris-proto/node"
)

// errSendStream is a NodeConnect stream whose Send always fails. The embedded
// interface is nil on purpose: SendRequestStreaming only ever calls Send on the
// stream, so no other method is reached, and this keeps the fake to one line of
// behaviour.
type errSendStream struct {
	pb.NodeService_NodeConnectServer
	err error
}

func (s errSendStream) Send(*pb.NodeMessage) error { return s.err }

// TestSendRequestStreamingCleansUpOnSendFailure pins the error path's
// housekeeping: when the send fails the pending entry must be removed and the
// channel must not be left reachable, or a later RouteResponse would send on a
// channel no reader owns.
func TestSendRequestStreamingCleansUpOnSendFailure(t *testing.T) {
	r := NewRegistry()
	conn := r.Register(1, "token", errSendStream{err: errors.New("node dropped")})

	ch, err := r.SendRequestStreaming(1, &pb.NodeMessage{RequestId: "req-1"})
	if err == nil {
		t.Fatal("SendRequestStreaming returned nil error after the send failed")
	}
	if ch != nil {
		t.Fatal("SendRequestStreaming returned a channel alongside an error")
	}

	conn.mu.Lock()
	_, present := conn.pending["req-1"]
	conn.mu.Unlock()
	if present {
		t.Fatal("the pending entry was left behind after a send failure")
	}
	if conn.RouteResponse(&pb.NodeMessage{RequestId: "req-1"}) {
		t.Fatal("RouteResponse routed to a request whose send failed")
	}
}

// TestSendRequestStreamingSendFailureDoesNotDoubleClose exercises the send-
// failure path against a concurrent Unregister of the same node, which is the
// interleaving the close-under-lock guard exists for: both paths close the
// pending channel, and closing an already-closed channel panics.
//
// A smoke check, not a proof - it only trips the broken version when the two
// goroutines interleave inside the window - but the panic is a hard crash, so a
// few hundred rounds make it reliable. What guarantees the fix is that both
// closers drop the map entry under conn.mu (TestPendingChannelsAreRemovedWhenClosed)
// and the send-failure path re-checks presence before closing. Run with -race.
func TestSendRequestStreamingSendFailureDoesNotDoubleClose(t *testing.T) {
	const rounds = 500
	for i := 0; i < rounds; i++ {
		r := NewRegistry()
		conn := r.Register(1, "token", errSendStream{err: errors.New("node dropped")})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.SendRequestStreaming(1, &pb.NodeMessage{RequestId: "req-1"})
		}()
		go func() {
			defer wg.Done()
			r.Unregister(1, conn)
		}()
		wg.Wait()
	}
}

// addPending registers a response channel the way SendRequest/SendRequestStreaming
// do. Those two cannot be used here because they call conn.Send, which needs a
// real gRPC stream; this test only exercises the channel lifetime, so it takes
// the same lock and writes the same map entry directly.
func addPending(conn *NodeConnection, requestID string) chan *pb.NodeMessage {
	ch := make(chan *pb.NodeMessage, 1)
	conn.mu.Lock()
	conn.pending[requestID] = ch
	conn.mu.Unlock()
	return ch
}

// TestPendingChannelsAreRemovedWhenClosed pins the contract that makes
// RouteResponse's lock discipline sufficient: every path that closes a pending
// channel must also make that channel unreachable through the map, under the
// same mutex. If a closer ever left a closed channel in pending, RouteResponse
// would find it and send on it, which panics.
//
// The three closers are covered as a table. Register appears because a node
// reconnecting under the same id closes the outgoing connection's channels from
// a DIFFERENT goroutine than the one routing on it, which is the case that made
// the send-after-unlock window reachable in the first place.
func TestPendingChannelsAreRemovedWhenClosed(t *testing.T) {
	tests := []struct {
		name  string
		close func(r *Registry, conn *NodeConnection)
	}{
		{
			name:  "reconnect under the same node id",
			close: func(r *Registry, _ *NodeConnection) { r.Register(1, "token", nil) },
		},
		{
			name:  "node disconnects",
			close: func(r *Registry, conn *NodeConnection) { r.Unregister(1, conn) },
		},
		{
			name:  "streaming transfer finishes",
			close: func(_ *Registry, conn *NodeConnection) { conn.CloseStreamingRequest("req-1") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			conn := r.Register(1, "token", nil)
			ch := addPending(conn, "req-1")

			tt.close(r, conn)

			// The channel must be closed: a caller ranging over it has to be
			// released, not left blocked forever.
			if _, open := <-ch; open {
				t.Fatal("pending channel = still open after close, want closed")
			}

			// And it must no longer be reachable, or RouteResponse would send
			// on it. This is the half that keeps the send safe under the lock.
			if conn.RouteResponse(&pb.NodeMessage{RequestId: "req-1"}) {
				t.Fatal("RouteResponse on a closed request = true, want false (channel still reachable through pending)")
			}
		})
	}
}

// TestRouteResponseSurvivesAReconnectRace exercises the concurrent shape that
// the lock discipline exists for: one goroutine routing a response while
// another reconnects the same node id and closes the channel out from under it.
//
// This is a smoke check, NOT a proof. It cannot fail on correct code, but it
// only catches the broken version when the two goroutines happen to interleave
// inside the window, so a single green run does not establish the fix. What
// establishes it is that every closer drops the map entry under conn.mu
// (pinned by TestPendingChannelsAreRemovedWhenClosed) and that RouteResponse
// now holds that mutex across its send. Run with -race.
func TestRouteResponseSurvivesAReconnectRace(t *testing.T) {
	const rounds = 500

	for i := 0; i < rounds; i++ {
		r := NewRegistry()
		conn := r.Register(1, "token", nil)
		addPending(conn, "req-1")

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			conn.RouteResponse(&pb.NodeMessage{RequestId: "req-1"})
		}()
		go func() {
			defer wg.Done()
			r.Register(1, "token", nil)
		}()
		wg.Wait()
	}
}

// TestUnregisterDoesNotEvictAReconnectedNode pins the identity check in
// Unregister.
//
// Two NodeConnect streams can hold the same node id at once. gRPC keepalive
// gives Core up to about 15 seconds to notice a stream is dead (Time 5s +
// Timeout 10s), and a node whose network blipped - or which was restarted -
// reconnects well inside that. Register handles the overlap: it closes the
// superseded connection's pending channels and takes over the map entry.
//
// The LATE teardown was the problem. When the old stream's Recv finally
// errored, its deferred Unregister deleted whatever sat under that node id,
// which by then was the new connection. The node was connected and Core
// believed it was not, so every command, transfer and tab-proxy request to it
// failed until the live stream itself died - with nothing in either log to say
// why, because both halves did exactly what they were told.
func TestUnregisterDoesNotEvictAReconnectedNode(t *testing.T) {
	r := NewRegistry()

	first := r.Register(1, "token", nil)
	second := r.Register(1, "token", nil) // the node reconnects

	// The stale stream's deferred teardown finally runs.
	r.Unregister(1, first)

	got, ok := r.GetConnection(1)
	if !ok {
		t.Fatal("the reconnected node was evicted by the dead stream's teardown; every request to it now fails with \"node not connected\"")
	}
	if got != second {
		t.Fatalf("registry holds the wrong connection: got %p, want the reconnected one %p", got, second)
	}

	// And the live connection must still be usable, not a husk whose pending
	// map was nil'd on the way past.
	if ch := addPending(second, "req-1"); ch == nil {
		t.Fatal("the surviving connection cannot take a pending request")
	}

	// The real teardown still works.
	r.Unregister(1, second)
	if _, ok := r.GetConnection(1); ok {
		t.Error("Unregister no longer removes the connection it was given")
	}
}

// TestAShortSecondConnectHandsTheEntryBack pins the restore in Unregister. A
// node re-bootstrapping its Redis ACL opens a one-shot NodeConnect beside its
// live stream; that connect took the entry and its teardown removed it, so the
// node was connected and unreachable until it happened to reconnect.
func TestAShortSecondConnectHandsTheEntryBack(t *testing.T) {
	key := ed25519.PublicKey(bytes.Repeat([]byte{1}, ed25519.PublicKeySize))
	r := NewRegistry()
	live := r.RegisterProved(1, "token", nil, key)
	oneShot := r.RegisterProved(1, "token", nil, key)
	r.Unregister(1, oneShot)

	got, ok := r.GetConnection(1)
	if !ok || got != live {
		t.Fatal("the live stream did not get the entry back after the one-shot connect ended")
	}
	ch := addPending(live, "req-1")
	if !live.RouteResponse(&pb.NodeMessage{RequestId: "req-1"}) {
		t.Fatal("the restored connection cannot route a reply")
	}
	if _, open := <-ch; !open {
		t.Fatal("the reply did not arrive")
	}

	r.Unregister(1, live)
	if _, ok := r.GetConnection(1); ok {
		t.Error("the restored connection's own teardown left it registered")
	}
}

// TestRestoreOnlyForTheSameLiveKey: what must never come back. A stream still
// being read after Reset pairing or Roll key logged in with the key those
// revoked; the stream that re-paired proves another one, or none.
func TestRestoreOnlyForTheSameLiveKey(t *testing.T) {
	k1 := ed25519.PublicKey(bytes.Repeat([]byte{1}, ed25519.PublicKeySize))
	k2 := ed25519.PublicKey(bytes.Repeat([]byte{2}, ed25519.PublicKeySize))
	tests := []struct {
		name string
		run  func(r *Registry)
	}{
		{"re-paired with another key", func(r *Registry) {
			r.RegisterProved(1, "token", nil, k1)
			r.Unregister(1, r.RegisterProved(1, "token", nil, k2))
		}},
		{"re-paired without a key", func(r *Registry) {
			r.RegisterProved(1, "token", nil, k1)
			r.Unregister(1, r.Register(1, "token", nil))
		}},
		{"both logged in by the secret", func(r *Registry) {
			r.Register(1, "token", nil)
			r.Unregister(1, r.Register(1, "token", nil))
		}},
		{"the replaced stream already ended", func(r *Registry) {
			dead := r.RegisterProved(1, "token", nil, k1)
			next := r.RegisterProved(1, "token", nil, k1)
			r.Unregister(1, dead)
			r.Unregister(1, next)
		}},
		{"only the stream directly replaced", func(r *Registry) {
			r.RegisterProved(1, "token", nil, k1)
			mid := r.RegisterProved(1, "token", nil, k1)
			last := r.RegisterProved(1, "token", nil, k1)
			r.Unregister(1, mid)
			r.Unregister(1, last)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			tt.run(r)
			if _, ok := r.GetConnection(1); ok {
				t.Fatal("a stream that must stay out got the entry back")
			}
		})
	}
}
