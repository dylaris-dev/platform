package main

import (
	"bytes"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// collect drains ch until scanLines returns.
func collect(t *testing.T, in string) []string {
	t.Helper()
	ch := make(chan string, 64)
	go func() {
		scanLines(strings.NewReader(in), ch)
		close(ch)
	}()
	var out []string
	for l := range ch {
		out = append(out, l)
	}
	return out
}

// The reason this file exists. bufio.Scanner fails PERMANENTLY on a line longer
// than its buffer, so the old reader returned at the first oversized line and
// never read the pipe again. This process is PID 1 of the Minecraft container:
// once nothing drains stdout the JVM blocks on its next log write and the
// server hangs, with the console frozen on the last thing it managed to ship.
func TestScanLinesKeepsReadingAfterAnOversizedLine(t *testing.T) {
	huge := strings.Repeat("x", maxLineBytes+5000)
	got := collect(t, "before\n"+huge+"\nafter\n")

	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3 (the oversized line must not end the stream)", len(got))
	}
	if got[0] != "before" || got[2] != "after" {
		t.Fatalf("lines around the oversized one were mangled: %q / %q", got[0], got[2])
	}
	if !strings.HasSuffix(got[1], lineTruncatedMarker) {
		t.Errorf("the oversized line is not marked as truncated: ...%q", got[1][len(got[1])-40:])
	}
	if n := len(got[1]) - len(lineTruncatedMarker); n != maxLineBytes {
		t.Errorf("truncated to %d bytes, want %d", n, maxLineBytes)
	}
}

// Ordinary behaviour the rewrite must not have changed: empty lines are lines,
// a final line without a newline still ships, and CRLF does not leave a \r.
func TestScanLinesOrdinaryOutput(t *testing.T) {
	got := collect(t, "one\n\ntwo\r\nthree")
	want := []string{"one", "", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestApplyCarriageReturns(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain line", "plain line"},
		{"", ""},
		{"abc\rd", "dbc"},                  // overwrite from column 0, keep the tail
		{"abc\rdefg", "defg"},              // longer segment replaces everything
		{"abc\r", "abc"},                   // trailing CR moves the cursor and writes nothing
		{"\rabc", "abc"},                   // leading CR
		{"loading\r100%\rdone", "doneing"}, // progress bar: the tail survives
		{"a\r\rb", "b"},                    // consecutive CRs
	}
	for _, c := range cases {
		if got := applyCarriageReturns(c.in); got != c.want {
			t.Errorf("applyCarriageReturns(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// applyCarriageReturnsSplit is the implementation this replaced, kept here as
// the reference: the rewrite was for speed, so it has to agree everywhere.
func applyCarriageReturnsSplit(line string) string {
	if !strings.ContainsRune(line, '\r') {
		return line
	}
	parts := strings.Split(line, "\r")
	result := ""
	for _, part := range parts {
		if len(part) >= len(result) {
			result = part
		} else {
			result = part + result[len(part):]
		}
	}
	return result
}

func TestApplyCarriageReturnsMatchesTheImplementationItReplaced(t *testing.T) {
	inputs := []string{
		"", "\r", "\r\r\r", "no cr at all",
		"abc\rd", "abc\rdefg", "abc\r", "\rabc", "a\r\rb",
		"loading\r100%\rdone",
		"[12:00:00] [Server thread/INFO]: Preparing spawn area: 45%\rPreparing spawn area: 90%\rDone",
		"\rshort\rlonger\rl\rlongest of them all\rx",
		strings.Repeat("ab\r", 50) + "tail",
	}
	for _, in := range inputs {
		if got, want := applyCarriageReturns(in), applyCarriageReturnsSplit(in); got != want {
			t.Errorf("applyCarriageReturns(%q) = %q, the old implementation said %q", in, got, want)
		}
	}
}

// The split-and-concatenate version was quadratic: a long run followed by many
// one-character runs rebuilt the whole string per run. At a few hundred KB that
// is tens of gigabytes of copying inside the goroutine whose only job is to keep
// draining the JVM's stdout - so a single crafted log line stalled the server.
//
// Sized well above the 64KB line cap, so the quadratic version is unmistakable.
// The split version copies 500KB per one-character segment - 125GB in total,
// tens of seconds; the in-place one touches each byte once and is done in about
// a millisecond. The 3s budget sits between those, far from either.
func TestApplyCarriageReturnsIsNotQuadratic(t *testing.T) {
	line := strings.Repeat("x", 500_000) + strings.Repeat("\ra", 250_000)

	done := make(chan string, 1)
	go func() { done <- applyCarriageReturns(line) }()
	select {
	case got := <-done:
		if len(got) != 500_000 || got[0] != 'a' {
			t.Fatalf("wrong result: len=%d first=%q", len(got), got[0])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("applyCarriageReturns did not finish in 3s; it is quadratic again")
	}
}

// The supervisor is PID 1, so Go's default SIGTERM action (terminate) took the
// JVM down with it - SIGKILLed, mid-write, no save. requestStop turns the signal
// into the shutdown the server understands, and marks the exit as wanted BEFORE
// asking, so the restart loop cannot race it into a restart.
func TestRequestStopAsksTheServerToQuitAndMarksTheExitAsWanted(t *testing.T) {
	var stopping atomic.Bool
	var stdin bytes.Buffer

	requestStop(&stopping, &stdin)

	if !stopping.Load() {
		t.Error("the stop flag is not set; the restart loop would treat the exit as a crash")
	}
	if got := stdin.String(); got != "stop\n" {
		t.Errorf("wrote %q to stdin, want %q", got, "stop\n")
	}
}

// requestStop being correct is worth nothing if nothing calls it, and an
// unwired handler compiles: the restart loop would simply relaunch the JVM the
// operator just stopped. Both halves have to be present.
func TestTheStopSignalIsActuallyWiredUp(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"signal.Notify(sigCh, syscall.SIGTERM)", // the signal is claimed at all
		"requestStop(&stopping, stdinPipe)",     // and reaches the RUNNING process's stdin
		"stopping.Load()",                       // and the restart loop honours it
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("main.go no longer contains %q; the stop signal is back to killing the JVM", want)
		}
	}
}
