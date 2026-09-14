package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "dylaris-proto/node"
)

// scriptedCore is one fake Core replica. answer decides what it replies to a
// node request; nil means it stays silent, which is what an old Core does.
// Replies arrive the way the read loop delivers them: on another goroutine,
// through routeNodeResponse.
type scriptedCore struct {
	fakeCoreStream
	cc     *coreConnection
	answer func(req *pb.NodeMessage) *pb.NodeMessage
	// afterSend, when set, runs after the request is recorded and answered.
	afterSend func(*scriptedCore)
}

func (s *scriptedCore) Send(m *pb.NodeMessage) error {
	_ = s.fakeCoreStream.Send(m)
	if r := s.answer(m); r != nil {
		r.RequestId = m.RequestId
		r.NodeRequest = true
		go s.cc.routeNodeResponse(r)
	}
	if s.afterSend != nil {
		s.afterSend(s)
	}
	return nil
}

func meshWithCores(answers map[string]func(*pb.NodeMessage) *pb.NodeMessage) (*MeshManager, map[string]*scriptedCore) {
	m := NewMeshManager("node-abc", nil, nil)
	cores := make(map[string]*scriptedCore)
	for id, answer := range answers {
		sc := &scriptedCore{answer: answer}
		sc.cc = &coreConnection{stream: sc, pending: make(map[string]chan *pb.NodeMessage)}
		cores[id] = sc
		m.connections[id] = sc.cc
	}
	return m, cores
}

func silent(*pb.NodeMessage) *pb.NodeMessage { return nil }

func restoreURL(url string) *pb.NodeMessage {
	return &pb.NodeMessage{Payload: &pb.NodeMessage_RestoreUrlResponse{RestoreUrlResponse: &pb.RestoreUrlResponse{Url: url}}}
}

func restoreReq(id string) *pb.NodeMessage {
	return &pb.NodeMessage{Payload: &pb.NodeMessage_RestoreUrlRequest{RestoreUrlRequest: &pb.RestoreUrlRequest{RestoreId: id}}}
}

const testAttempt = 150 * time.Millisecond

// The first replica asked does not serve the request - silent like an old Core,
// or refusing it explicitly - and the retry has to land on the OTHER one. Which
// replica goes first is random, so the fake fails whichever is asked first.
func TestRequestRetriesOnAnotherCore(t *testing.T) {
	cases := []struct {
		name  string
		first func(*pb.NodeMessage) *pb.NodeMessage
	}{
		{"silent old Core", silent},
		{"unsupported", func(*pb.NodeMessage) *pb.NodeMessage {
			return &pb.NodeMessage{Payload: &pb.NodeMessage_Error{Error: &pb.OpError{Code: 501, Message: "unsupported"}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var asked atomic.Int32
			answer := func(req *pb.NodeMessage) *pb.NodeMessage {
				if asked.Add(1) == 1 {
					return tc.first(req)
				}
				return restoreURL("https://signed")
			}
			m, cores := meshWithCores(map[string]func(*pb.NodeMessage) *pb.NodeMessage{"core-a": answer, "core-b": answer})

			resp, err := m.Request(context.Background(), restoreReq("r-1"), testAttempt)
			if err != nil {
				t.Fatalf("Request: %v", err)
			}
			if got := resp.GetRestoreUrlResponse().GetUrl(); got != "https://signed" {
				t.Errorf("url = %q", got)
			}
			for id, c := range cores {
				sent := c.messages()
				if len(sent) != 1 {
					t.Fatalf("%s was asked %d times, want once each (the retry must go to the other Core)", id, len(sent))
				}
				if !sent[0].NodeRequest || sent[0].RequestId == "" {
					t.Errorf("%s got request_id %q node_request=%v", id, sent[0].RequestId, sent[0].NodeRequest)
				}
			}
		})
	}
}

func TestRequestFailsWhenEveryCoreFails(t *testing.T) {
	m, cores := meshWithCores(map[string]func(*pb.NodeMessage) *pb.NodeMessage{"core-a": silent, "core-b": silent, "core-c": silent})
	start := time.Now()
	if _, err := m.Request(context.Background(), restoreReq("r-1"), testAttempt); err == nil {
		t.Fatal("Request succeeded with no Core answering")
	}
	if elapsed := time.Since(start); elapsed > 4*testAttempt {
		t.Errorf("took %s: more than the one retry", elapsed)
	}
	total := 0
	for _, c := range cores {
		total += len(c.messages())
	}
	if total != 2 {
		t.Errorf("%d attempts, want exactly 2", total)
	}

	one, _ := meshWithCores(map[string]func(*pb.NodeMessage) *pb.NodeMessage{"core-a": silent})
	if _, err := one.Request(context.Background(), restoreReq("r-1"), testAttempt); err == nil {
		t.Fatal("Request succeeded against a single silent Core")
	}
	if _, err := NewMeshManager("node-abc", nil, nil).Request(context.Background(), restoreReq("r-1"), testAttempt); err != errNoCoreConnection {
		t.Errorf("no connections: err = %v, want errNoCoreConnection", err)
	}
}

// A connection that dies ends the attempt at once rather than at the timeout.
func TestRequestRetriesAtOnceWhenTheConnectionCloses(t *testing.T) {
	var asked atomic.Int32
	answer := func(req *pb.NodeMessage) *pb.NodeMessage {
		if asked.Add(1) == 1 {
			return nil
		}
		return restoreURL("https://signed")
	}
	m, cores := meshWithCores(map[string]func(*pb.NodeMessage) *pb.NodeMessage{"core-a": answer, "core-b": answer})
	// Whichever Core is asked first loses its stream right after the send.
	for _, c := range cores {
		c.afterSend = func(c *scriptedCore) {
			if len(c.messages()) == 1 && asked.Load() == 1 {
				c.cc.closePending()
			}
		}
	}
	start := time.Now()
	resp, err := m.Request(context.Background(), restoreReq("r-1"), 5*time.Second)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.GetRestoreUrlResponse().GetUrl() != "https://signed" {
		t.Errorf("resp = %v", resp)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s: the closed connection was waited out instead of failing fast", elapsed)
	}
}

// An answer nobody waits for (a late one after a timeout, or a stray id) is
// dropped without disturbing the request that IS waiting, and concurrent
// requests on one connection each get their own answer.
func TestRequestAnswersDoNotCross(t *testing.T) {
	var m *MeshManager
	var cores map[string]*scriptedCore
	m, cores = meshWithCores(map[string]func(*pb.NodeMessage) *pb.NodeMessage{"core-a": func(req *pb.NodeMessage) *pb.NodeMessage {
		cc := cores["core-a"].cc
		cc.routeNodeResponse(&pb.NodeMessage{RequestId: "nobody-" + req.RequestId, NodeRequest: true, Payload: restoreURL("stray").Payload})
		time.Sleep(time.Duration(req.RequestId[len(req.RequestId)-1]%5) * time.Millisecond)
		return restoreURL("for-" + req.GetRestoreUrlRequest().GetRestoreId())
	}})

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("r-%d", i)
			resp, err := m.Request(context.Background(), restoreReq(id), 2*time.Second)
			if err != nil {
				errs <- fmt.Errorf("%s: %w", id, err)
				return
			}
			if got := resp.GetRestoreUrlResponse().GetUrl(); got != "for-"+id {
				errs <- fmt.Errorf("%s got %q", id, got)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	cc := cores["core-a"].cc
	cc.pendingMu.Lock()
	left := len(cc.pending)
	cc.pendingMu.Unlock()
	if left != 0 {
		t.Errorf("%d pending entries left behind", left)
	}
	if n := len(cores["core-a"].messages()); n != 50 {
		t.Errorf("Core was asked %d times, want 50 (no retries on success)", n)
	}
}
