package console

import (
	"strings"
	"testing"
)

// Create normalized and checked its input inline; Update only TrimSpace'd the
// payload. So every rule below could be walked past with a PATCH on a task that
// a POST would have refused outright. Both now run the same two helpers, and
// these tests pin them.

// The payload is executed as "say "+payload on the server's stdin queue, so an
// embedded newline is a SECOND console command - and schedule.write is a
// separate capability from console.send, so holding only the former was never
// meant to reach the console. What kept the PATCH gap from being a live
// injection is that the log-shipper strips CR/LF again before writing to the
// JVM's stdin (log-shipper/main.go forwardInput). That is a single line in
// another service; the value must not carry a newline to begin with.
func TestNormalizeTaskPayloadRemovesEmbeddedLineBreaks(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello\nop attacker", "helloop attacker"},
		{"hello\r\nop attacker", "helloop attacker"},
		{"hello\rop attacker", "helloop attacker"},
		{"line1\nline2\nline3", "line1line2line3"},
		{"  padded  ", "padded"},
		// TrimSpace alone handled this one, which is why the gap was easy to
		// miss: a payload whose newline is at the END looks clean either way.
		{"trailing\n", "trailing"},
		{"plain message", "plain message"},
	}
	for _, c := range cases {
		if got := normalizeTaskPayload(c.in); got != c.want {
			t.Errorf("normalizeTaskPayload(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(normalizeTaskPayload(c.in), "\r\n") {
			t.Errorf("normalizeTaskPayload(%q) still contains a line break", c.in)
		}
	}
}

func TestValidateTaskFields(t *testing.T) {
	cases := []struct {
		name     string
		taskName string
		taskType string
		payload  string
		wantErr  string
	}{
		{"a valid say task", "announce", "say", "server restarting", ""},
		{"a valid restart task", "nightly", "restart", "", ""},
		{
			// Reachable only through Update: PATCH {"taskType":"say"} on a
			// restart task leaves Payload empty, and the executor then returns
			// "say task has empty payload" on every tick, forever.
			name: "flipping a restart task to say without a message", taskName: "nightly",
			taskType: "say", payload: "", wantErr: "Payload (message) required for 'say' task",
		},
		{
			name: "an over-long name", taskName: strings.Repeat("a", 129),
			taskType: "restart", payload: "", wantErr: "Name too long (max 128 characters)",
		},
		{"a name at the limit", strings.Repeat("a", 128), "restart", "", ""},
		{
			// Not caught anywhere downstream: the log-shipper truncates what it
			// writes to stdin at 1KB, but the DB row and the panel's task list
			// carry the whole blob.
			name: "an over-long payload", taskName: "spam", taskType: "say",
			payload: strings.Repeat("x", 513), wantErr: "Payload too long (max 512 characters)",
		},
		{"a payload at the limit", "ok", "say", strings.Repeat("x", 512), ""},
		{"an unknown task type", "weird", "rcon", "op me", "Unsupported task type"},
		{"an empty task type", "weird", "", "", "Unsupported task type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := validateTaskFields(c.taskName, c.taskType, c.payload)
			if got != c.wantErr {
				t.Errorf("validateTaskFields(%q, %q, len=%d) = %q, want %q",
					c.taskName, c.taskType, len(c.payload), got, c.wantErr)
			}
		})
	}
}
