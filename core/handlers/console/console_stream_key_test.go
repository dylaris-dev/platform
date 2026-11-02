package console

import (
	"testing"

	"dylaris-core/models"
)

// The read side of the console used to demand a sub-server name that the write
// side infers, so a caller who sent commands successfully got an empty 200 back
// when asking what the server said. Every row here is one of the four states
// that produced, and the one it must produce now.
func TestConsoleStreamKey(t *testing.T) {
	const uuid = "11111111-1111-4111-8111-111111111111"

	tests := []struct {
		name      string
		active    string
		requested string
		want      string
		why       string
	}{
		{
			name:      "an explicit sub-server wins",
			active:    "survival",
			requested: "creative",
			want:      "dylaris:server:" + uuid + ":logs:creative",
			why:       "a caller asking for one sub-server must not be answered with another",
		},
		{
			name:      "no sub-server asked for falls back to the running one",
			active:    "survival",
			requested: "",
			want:      "dylaris:server:" + uuid + ":logs:survival",
			why:       "this is the bug: the un-suffixed key does not exist for such a server, so the console read empty while the server was running",
		},
		{
			name:      "a server with no sub-server keeps the plain key",
			active:    "",
			requested: "",
			want:      "dylaris:server:" + uuid + ":logs",
			why:       "the fallback must not invent a sub-server that was never set up",
		},
		{
			name:      "an explicit sub-server still wins when none is active",
			active:    "",
			requested: "survival",
			want:      "dylaris:server:" + uuid + ":logs:survival",
			why:       "reading a stopped sub-server's history is a legitimate request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := consoleStreamKey(&models.Server{UUID: uuid, ActiveSubServer: tt.active}, tt.requested)
			if got != tt.want {
				t.Errorf("key = %q, want %q: %s", got, tt.want, tt.why)
			}
		})
	}
}
