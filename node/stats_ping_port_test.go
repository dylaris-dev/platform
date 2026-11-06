package main

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
)

// J48: the status ping dialled the node-wide container port, so a server built
// on its own container port never turned ready and reported no players.
func TestPingPortIsTheOneTheContainerWasBuiltOn(t *testing.T) {
	def := getContainerPort()
	bound := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{
			PortBindings: nat.PortMap{"25570/tcp": {{HostIP: "0.0.0.0", HostPort: "25601"}}},
		}},
	}
	withExtra := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{
			PortBindings: nat.PortMap{
				"25566/tcp": {{HostIP: "0.0.0.0", HostPort: "25602"}},
				"8100/tcp":  {{HostIP: "0.0.0.0", HostPort: "26002"}},
			},
		}},
		Config: &container.Config{Labels: map[string]string{extraPortsLabel: "tcp:26002"}},
	}
	unbound := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{HostConfig: &container.HostConfig{}},
	}
	for _, c := range []struct {
		name  string
		info  container.InspectResponse
		saved int
		want  int
	}{
		{"its own binding beats the saved port", bound, 25580, 25570},
		{"an extra port is not the game port", withExtra, 0, 25566},
		{"no binding (gateway): the saved port", unbound, 25580, 25580},
		{"nothing set: the node default", unbound, 0, def},
		{"no host config: the saved port", container.InspectResponse{}, 25580, 25580},
	} {
		if got := pingPort(c.info, func() int { return c.saved }); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}
