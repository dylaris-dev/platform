package handlers

import "testing"

type imgSettings map[string]string

func (s imgSettings) GetSetting(k string) (string, error) { return s[k], nil }

// A server's runtime image was taken from any registry; an image with a setuid
// binary was root in the container and could rewrite the node's files.
func TestOnlyAllowedRuntimeImagesAreTaken(t *testing.T) {
	for _, tc := range []struct {
		img   string
		extra string
		want  bool
	}{
		{"ghcr.io/dylaris-dev/platform-mc-java21:latest", "", true},
		{"", "", true}, // keeps the stored image
		{"evil.example/x:1", "", false},
		{"ghcr.io/dylaris-dev/platform-mc-java21:latest evil", "", false},
		{"ghcr.io/dylaris-dev/platform-mc-javaX@sha256:abc", "", true},
		{"registry.example/approved/java:21", "registry.example/approved/", true},
		{"registry.example/other/java:21", "registry.example/approved/", false},
	} {
		if got := javaImageAllowed(imgSettings{"runtime.allowed_images": tc.extra}, tc.img); got != tc.want {
			t.Errorf("%q (extra %q) = %v, want %v", tc.img, tc.extra, got, tc.want)
		}
	}
}

func (f *runtimeFakeStore) GetSetting(string) (string, error) { return "", nil }

// Through the route: a runtime change to an image of the tenant's own is
// refused, and nothing is written.
func TestARuntimeChangeToAForeignImageIsRefused(t *testing.T) {
	fs := &runtimeFakeStore{srv: onlineServer()}
	rw := runtimeRequest(t, fs, `{"javaImage":"evil.example/setuid-root:1"}`)
	if rw.Code != 400 || fs.written {
		t.Fatalf("status %d, written %v; want 400 and nothing stored", rw.Code, fs.written)
	}
}

// Saving the placement page wrote back the 0 it showed, switching the node's
// process cap off without anyone choosing to.
func TestThePlacementPageDefaultsToTheNodesProcessCap(t *testing.T) {
	if defaultPlacementSettings.PidsLimit != 4096 {
		t.Fatalf("default pids limit = %d, want the node's 4096", defaultPlacementSettings.PidsLimit)
	}
}
