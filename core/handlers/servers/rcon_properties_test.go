package servers

import "testing"

func TestMergeServerProperties(t *testing.T) {
	tests := []struct {
		name     string
		existing string
		kv       map[string]string
		want     string
	}{
		{
			name:     "empty appends keys in sorted order",
			existing: "",
			kv:       map[string]string{"enable-rcon": "true", "rcon.port": "25575", "rcon.password": "secret"},
			want:     "enable-rcon=true\nrcon.password=secret\nrcon.port=25575\n",
		},
		{
			name:     "replaces value in place and preserves other keys and order",
			existing: "motd=Hello\nenable-rcon=false\nmax-players=20\n",
			kv:       map[string]string{"enable-rcon": "true"},
			want:     "motd=Hello\nenable-rcon=true\nmax-players=20\n",
		},
		{
			name:     "replaces present key and appends absent keys sorted",
			existing: "enable-rcon=false\n",
			kv:       map[string]string{"enable-rcon": "true", "rcon.port": "25580", "rcon.password": "pw"},
			want:     "enable-rcon=true\nrcon.password=pw\nrcon.port=25580\n",
		},
		{
			name:     "preserves comments and blank lines",
			existing: "#Minecraft server properties\n\nmotd=Hi\n",
			kv:       map[string]string{"enable-rcon": "true"},
			want:     "#Minecraft server properties\n\nmotd=Hi\nenable-rcon=true\n",
		},
		{
			name:     "normalizes CRLF to LF",
			existing: "motd=Hi\r\nenable-rcon=false\r\n",
			kv:       map[string]string{"enable-rcon": "true"},
			want:     "motd=Hi\nenable-rcon=true\n",
		},
		{
			name:     "does not grow a blank line when input lacks trailing newline",
			existing: "motd=Hi",
			kv:       map[string]string{"enable-rcon": "true"},
			want:     "motd=Hi\nenable-rcon=true\n",
		},
		{
			name:     "preserves a line without an equals sign",
			existing: "garbage-line\nmotd=Hi\n",
			kv:       map[string]string{"motd": "Bye"},
			want:     "garbage-line\nmotd=Bye\n",
		},
		{
			name:     "matches key with surrounding whitespace after trimming",
			existing: "  enable-rcon = false \n",
			kv:       map[string]string{"enable-rcon": "true"},
			want:     "enable-rcon=true\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeServerProperties(tt.existing, tt.kv)
			if got != tt.want {
				t.Errorf("mergeServerProperties()\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}
