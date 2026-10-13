package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeRedis speaks just enough RESP for the shipper: every command is
// recorded and answered +OK, except HELLO, which is refused so the client
// stays on RESP2. hang makes it accept connections and never answer, which is
// what a Redis behind a link that swallows packets looks like.
type fakeRedis struct {
	ln   net.Listener
	hang bool
	mu   sync.Mutex
	cmds [][]string
}

func newFakeRedis(t *testing.T, hang bool) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{ln: ln, hang: hang}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeRedis) serve(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		cmd, err := readCommand(r)
		if err != nil {
			return
		}
		if f.hang {
			continue
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, cmd)
		f.mu.Unlock()
		reply := "+OK\r\n"
		if strings.EqualFold(cmd[0], "hello") {
			reply = "-ERR unknown command\r\n"
		}
		if _, err := io.WriteString(c, reply); err != nil {
			return
		}
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	head, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(head, "*")))
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := range args {
		size, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(size, "$")))
		if err != nil {
			return nil, err
		}
		b := make([]byte, l+2)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		args[i] = string(b[:l])
	}
	return args, nil
}

// shipped returns the console lines XADDed so far, in order.
func (f *fakeRedis) shipped() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.cmds {
		if strings.EqualFold(c[0], "xadd") {
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

// The invariant that keeps a log outage from freezing the game: the reader
// side, which is all that drains the JVM's stdout, never waits on Redis. With
// the write in the reader loop, a Redis that never answers held each flush for
// the client's timeouts and the second batch of 50 lines already blocked.
func TestShipLogsNeverWaitsOnRedis(t *testing.T) {
	f := newFakeRedis(t, true)
	rdb := redis.NewClient(&redis.Options{Addr: f.ln.Addr().String(), MaxRetries: -1})
	defer rdb.Close()

	ctx, cancel := context.WithCancel(context.Background())
	lineCh := make(chan string, lineChanSize)
	done := make(chan struct{})
	go func() {
		shipLogs(ctx, rdb, "logs", "heap", lineCh, nil)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for i := range 5 * lineChanSize {
		select {
		case lineCh <- fmt.Sprintf("[20:51:01] [DH-World Gen Thread[0]/WARN]: spam %d", i):
		case <-deadline:
			t.Fatalf("the reader stalled after %d lines while Redis hung", i)
		}
	}
	cancel()
	<-done
}

func TestShipLogsDeliversInOrder(t *testing.T) {
	f := newFakeRedis(t, false)
	rdb := redis.NewClient(&redis.Options{Addr: f.ln.Addr().String()})
	defer rdb.Close()

	lineCh := make(chan string, lineChanSize)
	done := make(chan struct{})
	go func() {
		shipLogs(context.Background(), rdb, "logs", "", lineCh, nil)
		close(done)
	}()
	var want []string
	for i := range 3*batchSize + 7 {
		want = append(want, fmt.Sprintf("line %d", i))
		lineCh <- want[i]
	}
	close(lineCh)
	<-done // the final flush has run

	got := f.shipped()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("shipped %d lines, want %d in order:\n%v", len(got), len(want), got)
	}
}

// A failed write puts its lines back ahead of what arrived meanwhile, so the
// console keeps its order across an outage, and evictions on either side are
// still counted.
func TestReplayBuffer_PutBackKeepsTheOrder(t *testing.T) {
	var r replayBuffer
	r.add([]string{"a", "b"})
	taken := r
	r.clear()
	r.add([]string{"c"})
	r.dropped = 2
	r.putBack(taken)
	if got := strings.Join(r.lines, ","); got != "a,b,c" || r.dropped != 2 || r.bytes != 3 {
		t.Fatalf("lines %q dropped %d bytes %d, want a,b,c 2 3", got, r.dropped, r.bytes)
	}
}

// The process's output is read to the end after it exits. With StdoutPipe,
// Wait closed the pipe at exit and the unread tail - the end of a crash - was
// lost; a child left holding the pipe open is cut off after the grace.
func TestDrainAfterExit(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// A slow consumer, as under a spam: most of the output is still in the
	// pipe when the process exits.
	ch := make(chan string, 8)
	got := make(chan int)
	go func() {
		n := 0
		for range ch {
			if n++; n%100 == 0 {
				time.Sleep(5 * time.Millisecond)
			}
		}
		got <- n
	}()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); scanLines(pr, ch) }()
	for i := range 2000 {
		fmt.Fprintf(pw, "line %d\n", i)
	}
	pw.Close()
	drainAfterExit(&wg, 5*time.Second, pr)
	close(ch)
	if n := <-got; n != 2000 {
		t.Fatalf("read %d of 2000 lines", n)
	}

	// A writer that never closes: drain must still return after the grace.
	pr2, pw2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pw2.Close()
	var wg2 sync.WaitGroup
	wg2.Add(1)
	go func() { defer wg2.Done(); scanLines(pr2, make(chan string, 1)) }()
	start := time.Now()
	drainAfterExit(&wg2, 100*time.Millisecond, pr2)
	if time.Since(start) > 2*time.Second {
		t.Fatal("drain waited on a pipe that never closes")
	}
}
