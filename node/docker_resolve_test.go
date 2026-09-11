package main

import (
	"testing"

	"github.com/docker/docker/api/types/network"
)

func TestPickContainerIP(t *testing.T) {
	ep := func(ip string) *network.EndpointSettings { return &network.EndpointSettings{IPAddress: ip} }

	tests := []struct {
		name string
		nets map[string]*network.EndpointSettings
		want string
	}{
		{
			name: "prefers dylaris_net over bridge",
			nets: map[string]*network.EndpointSettings{
				"bridge":      ep("172.17.0.2"),
				"dylaris_net": ep("172.20.0.5"),
			},
			want: "172.20.0.5",
		},
		{
			name: "prefers a compose-prefixed *_dylaris_net",
			nets: map[string]*network.EndpointSettings{
				"bridge":               ep("172.17.0.2"),
				"platform_dylaris_net": ep("10.0.16.9"),
			},
			want: "10.0.16.9",
		},
		{
			name: "skips a dylaris endpoint with no IP, uses the next with one",
			nets: map[string]*network.EndpointSettings{
				"dylaris_net": ep(""),
				"bridge":      ep("172.17.0.9"),
			},
			want: "172.17.0.9",
		},
		{
			name: "no IP anywhere (stopped) -> nil",
			nets: map[string]*network.EndpointSettings{
				"dylaris_net": ep(""),
				"bridge":      nil,
			},
			want: "",
		},
		{
			name: "empty map -> nil",
			nets: map[string]*network.EndpointSettings{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := pickContainerIP(tc.nets)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("got %v, want nil", got)
				}
				return
			}
			if got == nil || got.String() != tc.want {
				t.Fatalf("got %v, want %s", got, tc.want)
			}
		})
	}
}
