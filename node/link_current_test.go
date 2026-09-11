package main

import (
	"sort"
	"strings"
	"testing"
)

// currentLink is the state of a Link that must be left alone: running, on the
// network, same image, same environment. Each test below breaks exactly one of
// those, so a case that goes green for the wrong reason is visible.
func currentLink() (runningLink, string, []string, string) {
	env := []string{
		"NODE_ID=node-1",
		"LINK_SECRET=tunnel-1",
		"LINK_DISCOVERY_PROOF=proof-1",
		"REDIS_ADDR=10.0.0.9:6379",
		"REDIS_USER=node-node-1-link",
		"REDIS_PASS=pass-1",
		"REDIS_DB=0",
		"LINK_EXTERNAL=false",
	}
	have := runningLink{
		exists:  true,
		running: true,
		imageID: "sha256:aaaa",
		// A real container also carries the IMAGE's own environment. Comparing
		// the two slices wholesale would never match, which is why linkEnvKeys
		// exists; this entry is here so a test that "passes" by comparing
		// everything would fail.
		env:      append([]string{"PATH=/usr/bin", "HOME=/root"}, env...),
		networks: []string{"bridge", "dylaris_net"},
	}
	return have, "sha256:aaaa", env, "dylaris_net"
}

// TestLinkIsCurrentLeavesAHealthyLinkAlone is the case the whole change exists
// for. Destroying the Link ends every player session on the host and every Beam
// transfer through it, and before this check a node process start always looked
// like a change - so a stack deploy of a mode:global node service dropped every
// gateway-routed player on every host, whether or not anything had changed.
func TestLinkIsCurrentLeavesAHealthyLinkAlone(t *testing.T) {
	have, imageID, env, net := currentLink()
	if !linkIsCurrent(have, imageID, env, net) {
		t.Fatal("a running, correctly configured Link was marked for recreation")
	}
}

func TestLinkIsCurrentRecreates(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*runningLink, *string, *[]string, *string)
		current bool
	}{
		{
			name:   "no container at all",
			mutate: func(h *runningLink, _ *string, _ *[]string, _ *string) { *h = runningLink{} },
		},
		{
			name:   "container exists but is stopped",
			mutate: func(h *runningLink, _ *string, _ *[]string, _ *string) { h.running = false },
		},
		{
			// A credential rotation. This one MUST still recreate: leaving it
			// alone would keep a Link running on access Core has withdrawn.
			name: "the tunnel token was rotated",
			mutate: func(_ *runningLink, _ *string, want *[]string, _ *string) {
				(*want)[1] = "LINK_SECRET=tunnel-2"
			},
		},
		{
			name: "the redis address changed",
			mutate: func(_ *runningLink, _ *string, want *[]string, _ *string) {
				(*want)[3] = "REDIS_ADDR=10.0.0.10:6379"
			},
		},
		{
			// The case a subset check misses: we stopped sending a variable, the
			// container still has it. Without linkEnvKeys spelled out, this is
			// invisible.
			name: "a variable we no longer send is still on the container",
			mutate: func(_ *runningLink, _ *string, want *[]string, _ *string) {
				*want = (*want)[:len(*want)-1] // drop LINK_EXTERNAL
			},
		},
		{
			name: "the image moved",
			mutate: func(_ *runningLink, wantImage *string, _ *[]string, _ *string) {
				*wantImage = "sha256:bbbb"
			},
		},
		{
			name: "not attached to the wanted network",
			mutate: func(h *runningLink, _ *string, _ *[]string, _ *string) {
				h.networks = []string{"bridge"}
			},
		},
		{
			// Unknown is NOT drift. A registry hiccup, or an image still running
			// but no longer tagged, must not destroy a healthy Link - the same
			// rule LinkImageStatus is documented with.
			name: "the image reference does not resolve locally",
			mutate: func(_ *runningLink, wantImage *string, _ *[]string, _ *string) {
				*wantImage = ""
			},
			current: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			have, imageID, env, net := currentLink()
			tt.mutate(&have, &imageID, &env, &net)
			if got := linkIsCurrent(have, imageID, env, net); got != tt.current {
				t.Errorf("linkIsCurrent = %v, want %v", got, tt.current)
			}
		})
	}
}

// TestLinkEnvKeysMatchBuildLinkEnv keeps the compared key list and the builder
// in step. Without it, a variable added to buildLinkEnv would simply never be
// compared, and a Link would keep running with a stale value of it - silently,
// which is the failure mode this whole block is about.
func TestLinkEnvKeysMatchBuildLinkEnv(t *testing.T) {
	origRedisDB, origNodeSecret := redisDB, nodeSecret
	origClusterSecret, origNodeExternal := clusterSecret, nodeExternal
	t.Cleanup(func() {
		redisDB, nodeSecret = origRedisDB, origNodeSecret
		clusterSecret, nodeExternal = origClusterSecret, origNodeExternal
	})
	redisDB = 0
	nodeSecret = []byte("unit-test-secret-for-link-env-keys")
	clusterSecret = "unit-test-cluster-secret"
	nodeExternal = false

	built := buildLinkEnv("node-1", "tunnel-1", "proof-1", "10.0.0.9:6379")

	var builtKeys []string
	for _, e := range built {
		key, _, found := strings.Cut(e, "=")
		if !found {
			t.Fatalf("buildLinkEnv produced %q, which is not KEY=VALUE", e)
		}
		builtKeys = append(builtKeys, key)
	}

	want := append([]string(nil), linkEnvKeys...)
	got := append([]string(nil), builtKeys...)
	sort.Strings(want)
	sort.Strings(got)

	if len(want) != len(got) {
		t.Fatalf("linkEnvKeys has %d entries %v, buildLinkEnv produces %d %v",
			len(want), want, len(got), got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("linkEnvKeys = %v, buildLinkEnv produces %v", want, got)
		}
	}
}

// TestIsLinkContainer pins the matcher against the image strings every
// deployment path actually uses. The previous test was for "dylaris-link",
// which is the Go MODULE name and occurs in none of them, so the count was 0 on
// every node no matter how many Links ran.
func TestIsLinkContainer(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		image string
		want  bool
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
			// An operator may point LINK_IMAGE at their own registry, where only
			// the fixed container name identifies the node-managed sidecar.
			name:  "a custom image under the node-managed name",
			names: []string{"/dylaris_link"},
			image: "registry.example.test/private/link:2026.09",
			want:  true,
		},
		{
			name:  "the edge is not a link",
			names: []string{"/dylaris-splice"},
			image: "ghcr.io/dylaris-dev/gateway-edge:splice-0.17.0",
			want:  false,
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
			if got := isLinkContainer(tt.names, tt.image); got != tt.want {
				t.Errorf("isLinkContainer(%v, %q) = %v, want %v", tt.names, tt.image, got, tt.want)
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
		name       string
		cname      string
		hostname   string
		image      string
		labels     map[string]string
		configured string
		want       bool
	}{
		{
			name:       "the sidecar this node created, as on eu-node-00",
			cname:      "/dylaris_link",
			hostname:   "dylaris_link",
			image:      published,
			configured: published,
			want:       true,
		},
		{
			name:       "created on a private registry the node is still configured for",
			cname:      "/dylaris_link",
			hostname:   "dylaris_link",
			image:      "registry.example.test/private/link:2026.09",
			configured: "registry.example.test/private/link:2026.09",
			want:       true,
		},
		{
			name:       "created from the published image before LINK_IMAGE changed",
			cname:      "/dylaris_link",
			hostname:   "dylaris_link",
			image:      "ghcr.io/dylaris-dev/gateway-link:sha-abc123",
			configured: "registry.example.test/private/link:2026.09",
			want:       true,
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
			configured: published,
			want:       false,
		},
		{
			name:       "an operator's own docker run under the same name",
			cname:      "/dylaris_link",
			hostname:   "5a6b7c8d9e0f",
			image:      published,
			configured: published,
			want:       false,
		},
		{
			name:       "a compose container_name that also set the hostname",
			cname:      "/dylaris_link",
			hostname:   "dylaris_link",
			image:      published,
			labels:     map[string]string{"com.docker.compose.project": "gateway"},
			configured: published,
			want:       false,
		},
		{
			name:       "a swarm task that somehow carries the name",
			cname:      "/dylaris_link",
			hostname:   "dylaris_link",
			image:      published,
			labels:     map[string]string{"com.docker.swarm.service.id": "svc1"},
			configured: published,
			want:       false,
		},
		{
			name:       "the name reused for something that is not a Link",
			cname:      "/dylaris_link",
			hostname:   "dylaris_link",
			image:      "nginx:alpine",
			configured: published,
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOwnLinkContainer(tt.cname, tt.hostname, tt.image, tt.labels, tt.configured); got != tt.want {
				t.Errorf("isOwnLinkContainer(%q, %q, %q, %v, %q) = %v, want %v",
					tt.cname, tt.hostname, tt.image, tt.labels, tt.configured, got, tt.want)
			}
		})
	}
}
