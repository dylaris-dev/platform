package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestReplayBuffer_KeepsEverythingUnderTheCaps(t *testing.T) {
	var r replayBuffer
	r.add([]string{"one", "two"})
	r.add([]string{"three"})

	got := r.pending()
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("pending() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pending()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if r.dropped != 0 {
		t.Errorf("dropped = %d, want 0", r.dropped)
	}
}

func TestReplayBuffer_EmptyUntilSomethingHappens(t *testing.T) {
	var r replayBuffer
	if !r.empty() {
		t.Error("a fresh buffer should be empty")
	}
	r.add([]string{"a"})
	if r.empty() {
		t.Error("a buffer holding a line is not empty")
	}
	r.clear()
	if !r.empty() {
		t.Error("a cleared buffer should be empty")
	}
}

// The newest lines are what tell a reader what state the server is in after an
// outage, so the OLDEST are the ones evicted.
func TestReplayBuffer_LineCapDropsOldestFirst(t *testing.T) {
	var r replayBuffer
	for i := range replayMaxLines + 50 {
		r.add([]string{fmt.Sprintf("line-%d", i)})
	}
	if r.dropped != 50 {
		t.Errorf("dropped = %d, want 50", r.dropped)
	}
	if len(r.lines) != replayMaxLines {
		t.Fatalf("held %d lines, want the cap of %d", len(r.lines), replayMaxLines)
	}
	if want := "line-50"; r.lines[0] != want {
		t.Errorf("oldest surviving line = %q, want %q", r.lines[0], want)
	}
	if want := fmt.Sprintf("line-%d", replayMaxLines+49); r.lines[len(r.lines)-1] != want {
		t.Errorf("newest line = %q, want %q", r.lines[len(r.lines)-1], want)
	}
}

// The byte cap is the one that protects the JVM: lines vary in size, so the
// line cap alone does not bound the memory the buffer takes.
func TestReplayBuffer_ByteCapBitesBeforeTheLineCap(t *testing.T) {
	var r replayBuffer
	big := strings.Repeat("x", 1<<20)
	for range 20 {
		r.add([]string{big})
	}
	if r.bytes > replayMaxBytes {
		t.Errorf("buffer holds %d bytes, over the cap of %d", r.bytes, replayMaxBytes)
	}
	if len(r.lines) >= replayMaxLines {
		t.Errorf("held %d lines - the byte cap should have bitten long before the line cap of %d",
			len(r.lines), replayMaxLines)
	}
	if r.dropped == 0 {
		t.Error("dropped = 0, want the evicted lines counted")
	}
}

// Losing lines is survivable; losing them silently is the bug this guards.
func TestReplayBuffer_PendingPrependsTheLossMarker(t *testing.T) {
	var r replayBuffer
	for i := range replayMaxLines + 3 {
		r.add([]string{fmt.Sprintf("line-%d", i)})
	}
	got := r.pending()
	if len(got) != replayMaxLines+1 {
		t.Fatalf("pending() returned %d lines, want %d plus one marker", len(got), replayMaxLines)
	}
	if want := fmt.Sprintf(lossMarker, 3); got[0] != want {
		t.Errorf("first line = %q, want the marker %q", got[0], want)
	}
	if got[1] != "line-3" {
		t.Errorf("line after the marker = %q, want %q", got[1], "line-3")
	}
}

func TestReplayBuffer_NoMarkerWhenNothingWasLost(t *testing.T) {
	var r replayBuffer
	r.add([]string{"a", "b"})
	if got := r.pending(); got[0] != "a" {
		t.Errorf("first line = %q, want %q - no marker belongs here", got[0], "a")
	}
}

// pending() must not consume the buffer: the caller clears only after Redis has
// accepted the write, so a failed attempt keeps everything for the next one.
func TestReplayBuffer_PendingDoesNotConsume(t *testing.T) {
	var r replayBuffer
	r.add([]string{"a", "b"})
	first := r.pending()
	second := r.pending()
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("pending() returned %d then %d lines, want 2 both times", len(first), len(second))
	}
	r.clear()
	if got := r.pending(); len(got) != 0 {
		t.Errorf("after clear(), pending() = %v, want nothing", got)
	}
}

// TestHeapLatest_CoalescesToTheNewestReading pins the coalescing that moved the
// heap write off the line-reading path.
//
// It used to be one synchronous rdb.Set per GC line, on the loop whose only job
// is to keep draining lineCh. With Redis unreachable that stalls the reader:
// lineCh fills, the scanners block, the JVM's stdout pipe fills, and the
// Minecraft server blocks writing a log line. Now the line path only records,
// and flush publishes at most one value per batch - the newest, since the key
// holds exactly one.
func TestHeapLatest_CoalescesToTheNewestReading(t *testing.T) {
	var h heapLatest

	if _, ok := h.take(); ok {
		t.Error("take() reported a reading before any GC line arrived")
	}

	h.note(512)
	h.note(480)
	h.note(497)

	mb, ok := h.take()
	if !ok {
		t.Fatal("take() reported nothing after three GC lines")
	}
	if mb != 497 {
		t.Errorf("take() = %d, want 497 (the newest reading)", mb)
	}

	// A second flush with no GC activity in between must issue no write at all,
	// rather than re-writing the value the key already holds.
	if _, ok := h.take(); ok {
		t.Error("take() reported a reading twice; a quiet flush would re-write the key")
	}
}

// TestHeapLatest_IgnoresANonPositiveReading is the guard parseHeapAfterGC
// already applies at its own end: a 0 MB heap right after a Full GC would
// publish a misleading dip to zero. Pinned here too so the coalescing layer
// cannot reintroduce it.
func TestHeapLatest_IgnoresANonPositiveReading(t *testing.T) {
	var h heapLatest
	h.note(0)
	if _, ok := h.take(); ok {
		t.Error("a zero reading was published")
	}
	h.note(-1)
	if _, ok := h.take(); ok {
		t.Error("a negative reading was published")
	}
}
