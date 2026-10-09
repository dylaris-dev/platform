package main

import (
	"testing"
)

func TestParseHeapAfterGC(t *testing.T) {
	cases := []struct {
		name string
		line string
		want int64
		ok   bool
	}{
		{
			name: "java17 g1 young pause MB",
			line: "[2026-05-25T13:01:51.000+0000][info][gc] GC(0) Pause Young (Normal) (G1 Evacuation Pause) 256M->50M(2048M) 12.345ms",
			want: 50,
			ok:   true,
		},
		{
			name: "java17 g1 mixed pause MB",
			line: "[2026-05-25T13:02:51.000+0000][info][gc] GC(12) Pause Young (Mixed) (G1 Preventive Collection) 1900M->1400M(2048M) 45.6ms",
			want: 1400,
			ok:   true,
		},
		{
			name: "java8 legacy KB",
			line: "[GC (Allocation Failure)  256000K->50000K(2048000K), 0.012345 secs]",
			want: 48, // 50000K / 1024 = 48 MB (rounded down)
			ok:   true,
		},
		{
			name: "non-gc info line",
			line: "[2026-05-25T13:01:51.000+0000][info][server] Done loading (5.2s)",
			want: 0,
			ok:   false,
		},
		{
			name: "minecraft chat line with arrow but no GC",
			line: "[Server thread/INFO]: <player> ->",
			want: 0,
			ok:   false,
		},
		{
			// A player types the shape and the panel charts it. The value sticks
			// until the next real GC, and the metric exists for idle servers -
			// which are exactly the ones that go minutes between young GCs.
			name: "a player cannot set the heap metric by typing a GC summary",
			line: "[12:00:00] [Server thread/INFO]: <Steve> 1M->9999M(9999M)",
			want: 0,
			ok:   false,
		},
		{
			// The Paper prefix is a different shape from vanilla's, which is why
			// the gate matches the two GC formats rather than trying to enumerate
			// what a Minecraft log line looks like.
			name: "nor through the paper log format",
			line: "[12:00:00 INFO]: <Steve> 2048000K->1500000K(2048000K)",
			want: 0,
			ok:   false,
		},
		{
			// A death message, a sign, an entity name - all player-authored, all
			// through the same logger.
			name: "nor by naming a pet after a GC summary",
			line: "[12:00:00] [Server thread/INFO]: Steve was slain by 9M->4000M(4096M)",
			want: 0,
			ok:   false,
		},
		{
			// The Java 8 record with the stamps -XX:+PrintGCDateStamps and
			// +PrintGCTimeStamps put in front of it. Still the JVM's own stdout,
			// so it still has to parse.
			name: "java8 legacy with date and uptime stamps",
			line: "2026-08-27T12:00:00.000+0000: 1.234: [GC (Allocation Failure)  256000K->50000K(2048000K), 0.012345 secs]",
			want: 48,
			ok:   true,
		},
		{
			name: "after-zero is allowed (post-clear)",
			line: "[2026-05-25T13:09:51.000+0000][info][gc] GC(99) Pause Full (System.gc()) 1M->0M(2048M) 200ms",
			want: 0,
			ok:   false, // we reject zero — would be misleading as a live metric
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseHeapAfterGC(c.line)
			if ok != c.ok {
				t.Fatalf("ok mismatch: got %v want %v (got=%d)", ok, c.ok, got)
			}
			if ok && got != c.want {
				t.Fatalf("value mismatch: got %d want %d", got, c.want)
			}
		})
	}
}

func TestIsUnifiedGCLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "g1 young pause",
			line: "[2026-05-26T16:13:18.366+0000][info][gc] GC(16) Pause Young (Concurrent Start) (Metadata GC Threshold) 434M->179M(2048M) 52.013ms",
			want: true,
		},
		{
			name: "concurrent mark cycle (no heap value)",
			line: "[2026-05-26T16:13:18.366+0000][info][gc] GC(17) Concurrent Mark Cycle",
			want: true,
		},
		{
			name: "gc heap subtag",
			line: "[2026-05-26T16:13:18.612+0000][info][gc,heap] Some heap-related info",
			want: true,
		},
		{
			name: "paper info line (not gc)",
			line: "[16:13:18 INFO]: Starting Minecraft server on *:25565",
			want: false,
		},
		{
			name: "paper warn timings",
			line: "[16:13:18 WARN]: [!] The timings profiler has been enabled but has been scheduled for removal from Paper in the future.",
			want: false,
		},
		{
			name: "chat with brackets that look gc-ish but not the tag form",
			line: "[Server thread/INFO]: <player> [gc] is not a real player",
			want: false,
		},
		{
			name: "legacy java8 GC format passes through",
			line: "[GC (Allocation Failure)  256000K->50000K(2048000K), 0.012345 secs]",
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isUnifiedGCLine(c.line)
			if got != c.want {
				t.Fatalf("got %v want %v for %q", got, c.want, c.line)
			}
		})
	}
}

func TestUUIDRegex(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"84087ad8-e332-44f2-adec-75869b8c1ee1", true},            // canonical UUID
		{"d10612d3-5a6c-4dce-99a3-18644d192bb4_xijavblknx", true}, // panel <ownerUUID>_<random>
		{"84087ad8e33244f2adec75869b8c1ee1", true},                // no-dash id
		{"a:b:c:d:e:f:gg", false},                                 // colon = Redis-key injection
		{"survival/../etc", false},                                // slash / traversal
		{"has space here", false},                                 // whitespace
		{"short", false},                                          // < 8 chars
		{"", false},                                               // empty
	}
	for _, c := range cases {
		if got := uuidRegex.MatchString(c.in); got != c.want {
			t.Errorf("uuidRegex.MatchString(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The panel polls RCON continuously, and Minecraft logs a thread start and a
// shutdown for every one of those connections. At the polling rate that pushes
// the server's actual output out of the stream's line cap.
func TestIsRconNoiseLine(t *testing.T) {
	noise := []string{
		"[12:00:00] [RCON Listener #1/INFO]: Thread RCON Client /172.18.0.1 started",
		"[12:00:00] [RCON Client /172.18.0.1 #4/INFO]: Thread RCON Client /172.18.0.1 shutting down",
		"[12:00:01] [RCON Listener #1/INFO]: RCON running on 0.0.0.0:25575",
	}
	for _, line := range noise {
		if !isRconNoiseLine(line) {
			t.Errorf("not filtered: %q", line)
		}
	}

	// Matched on the logger THREAD, not the message: a player saying the word in
	// chat, or the server reporting a real RCON problem from another thread, is
	// content the operator asked for.
	keep := []string{
		"[12:00:00] [Server thread/INFO]: <Steve> how do I set up rcon?",
		"[12:00:00] [Server thread/WARN]: Failed to bind rcon port",
		"[12:00:00] [Server thread/INFO]: Done (5.2s)! For help, type \"help\"",
		"",
	}
	for _, line := range keep {
		if isRconNoiseLine(line) {
			t.Errorf("wrongly filtered: %q", line)
		}
	}
}

// The filter is opt-in. A zero-value flag must let everything through, or a
// server whose Redis key was never written would silently lose console lines.
func TestRconFilterDefaultsOff(t *testing.T) {
	var f rconFilterFlag
	if f.on.Load() {
		t.Fatal("the RCON filter is on before anything set it")
	}
}
