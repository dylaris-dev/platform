package main

import (
	"testing"

	"github.com/docker/docker/api/types/container"
)

// TestLinkContainerCount covers the pure piece CountLinkContainers was split
// around: a successful, possibly-empty ContainerList result is the only case
// that may ever count as 0. The failure case - a list that errored - is
// CountLinkContainers' own ok=false branch, which the heartbeat (main.go)
// relies on to omit linkCount rather than report a false "no Link".
func TestLinkContainerCount(t *testing.T) {
	tests := []struct {
		name       string
		containers []container.Summary
		want       int
	}{
		{
			name:       "a successful empty list is 0, not unknown",
			containers: nil,
			want:       0,
		},
		{
			name: "one Link",
			containers: []container.Summary{
				{Names: []string{"/dylaris_link"}, Image: "ghcr.io/dylaris-dev/gateway-link:latest"},
			},
			want: 1,
		},
		{
			name: "two Links at once, start-first overlap",
			containers: []container.Summary{
				{Names: []string{"/dylaris_link"}, Image: "ghcr.io/dylaris-dev/gateway-link:latest"},
				{Names: []string{"/dylaris-prod_link.abc.xyz"}, Image: "ghcr.io/dylaris-dev/gateway-link@sha256:deadbeef"},
			},
			want: 2,
		},
		{
			name: "an mc server and the node itself do not count",
			containers: []container.Summary{
				{Names: []string{"/mc_7f3a"}, Image: "itzg/minecraft-server:latest"},
				{Names: []string{"/dylaris_node"}, Image: "ghcr.io/dylaris-dev/platform-node:latest"},
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := linkContainerCount(tt.containers); got != tt.want {
				t.Errorf("linkContainerCount(%v) = %d, want %d", tt.containers, got, tt.want)
			}
		})
	}
}
