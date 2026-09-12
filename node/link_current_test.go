package main

import (
	"testing"
)

// TestIsLinkContainer pins the matcher against the image strings every
// deployment path actually uses. The previous test was for "dylaris-link",
// which is the Go MODULE name and occurs in none of them, so the count was 0 on
// every node no matter how many Links ran.
func TestIsLinkContainer(t *testing.T) {
	tests := []struct {
		name   string
		names  []string
		image  string
		labels map[string]string
		want   bool
	}{
		{
			name:  "the node's built-in default image",
			names: []string{"/dylaris_link"},
			image: "ghcr.io/dylaris-dev/gateway-link:latest",
			want:  true,
		},
		{
			name:  "a pinned tag",
			names: []string{"/gw_link_1"},
			image: "ghcr.io/dylaris-dev/gateway-link:sha-abc123",
			want:  true,
		},
		{
			name:  "a manually deployed link under a compose name",
			names: []string{"/dylaris-gateway_link_1"},
			image: "ghcr.io/dylaris-dev/gateway-link:latest",
			want:  true,
		},
		{
			// The name fallback, and it still earns its place: a Link an older
			// node built from a private image is one this node refuses to REMOVE
			// (it cannot prove it made it), so it may still be running - and a
			// Link the network policy does not recognise is a Link every MC
			// server on the host drops, which locks out every player behind it.
			name:  "a private image under the name an older node used",
			names: []string{"/dylaris_link"},
			image: "registry.example.test/private/link:2026.09",
			want:  true,
		},
		{
			// The label, and the only test that cannot be passed by the two
			// above it: an operator who mirrors our image into their own
			// registry and lets an orchestrator name the container matches
			// neither the image string nor the legacy name. The label is set by
			// the image itself (gateway's root Dockerfile) and survives a
			// retag, so it is the one thing that travels with such a copy.
			name:   "a mirrored image under an orchestrator-generated name",
			names:  []string{"/prod_link.abc.xyz"},
			image:  "registry.example.test/private/link:2026.09",
			labels: map[string]string{"com.dylaris.role": "link"},
			want:   true,
		},
		{
			// Same container without the label, so the case above is pinned on
			// the label rather than on the name happening to contain "link".
			name:  "the same one with no label is not recognised",
			names: []string{"/prod_link.abc.xyz"},
			image: "registry.example.test/private/link:2026.09",
			want:  false,
		},
		{
			name:  "the edge is not a link",
			names: []string{"/dylaris-splice"},
			image: "ghcr.io/dylaris-dev/gateway-edge:splice-0.17.0",
			want:  false,
		},
		{
			// A future service built from the same shared Dockerfile: it takes
			// the label with a role of its own (SERVICE_ROLE), and admitting it
			// to every game server would be exactly the leak the ARG exists to
			// prevent.
			name:   "another service built from the shared Dockerfile",
			names:  []string{"/prod_something.abc.xyz"},
			image:  "ghcr.io/dylaris-dev/gateway-something:latest",
			labels: map[string]string{"com.dylaris.role": "something"},
			want:   false,
		},
		{
			name:  "the node is not a link",
			names: []string{"/dylaris_node"},
			image: "ghcr.io/dylaris-dev/platform-node:latest",
			want:  false,
		},
		{
			name:  "a customer's minecraft server is not a link",
			names: []string{"/mc_7f3a"},
			image: "itzg/minecraft-server:latest",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLinkContainer(tt.names, tt.image, tt.labels); got != tt.want {
				t.Errorf("isLinkContainer(%v, %q, %v) = %v, want %v", tt.names, tt.image, tt.labels, got, tt.want)
			}
		})
	}
}

// TestIsOwnLinkContainer pins what a node that stopped managing the Link may
// remove at startup. Only the removal side is dangerous: the Link is the only
// way in for a player, so a false "yes" disconnects everyone on the host from
// a Link the node never owned, while a false "no" leaves one idle container.
func TestIsOwnLinkContainer(t *testing.T) {
	const published = "ghcr.io/dylaris-dev/gateway-link:latest"
	tests := []struct {
		name     string
		cname    string
		hostname string
		image    string
		labels   map[string]string
		want     bool
	}{
		{
			name:     "the sidecar this node created, as on eu-node-00",
			cname:    "/dylaris_link",
			hostname: "dylaris_link",
			image:    published,
			want:     true,
		},
		{
			// LINK_IMAGE is gone with the rest of the node-managed Link, so
			// there is no longer anything that could name a private image. This
			// container is left running and logged, which is the safe direction:
			// never remove one we cannot prove we made.
			name:     "one made from a private image the node can no longer name",
			cname:    "/dylaris_link",
			hostname: "dylaris_link",
			image:    "registry.example.test/private/link:2026.09",
			want:     false,
		},
		{
			name:     "an older tag of the published image",
			cname:    "/dylaris_link",
			hostname: "dylaris_link",
			image:    "ghcr.io/dylaris-dev/gateway-link:sha-abc123",
			want:     true,
		},
		{
			name:     "a stack task of the Link service",
			cname:    "/dylaris-prod_link.x1y2z3.a4b5c6",
			hostname: "3f9c2d1e0a7b",
			image:    published,
			labels: map[string]string{
				"com.docker.swarm.service.id":   "svc1",
				"com.docker.swarm.service.name": "dylaris-prod_link",
			},
			want: false,
		},
		{
			name:     "an operator's own docker run under the same name",
			cname:    "/dylaris_link",
			hostname: "5a6b7c8d9e0f",
			image:    published,
			want:     false,
		},
		{
			name:     "a compose container_name that also set the hostname",
			cname:    "/dylaris_link",
			hostname: "dylaris_link",
			image:    published,
			labels:   map[string]string{"com.docker.compose.project": "gateway"},
			want:     false,
		},
		{
			name:     "a swarm task that somehow carries the name",
			cname:    "/dylaris_link",
			hostname: "dylaris_link",
			image:    published,
			labels:   map[string]string{"com.docker.swarm.service.id": "svc1"},
			want:     false,
		},
		{
			name:     "the name reused for something that is not a Link",
			cname:    "/dylaris_link",
			hostname: "dylaris_link",
			image:    "nginx:alpine",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOwnLinkContainer(tt.cname, tt.hostname, tt.image, tt.labels); got != tt.want {
				t.Errorf("isOwnLinkContainer(%q, %q, %q, %v) = %v, want %v",
					tt.cname, tt.hostname, tt.image, tt.labels, got, tt.want)
			}
		})
	}
}
