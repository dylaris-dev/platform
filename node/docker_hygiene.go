package main

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerimage "github.com/docker/docker/api/types/image"
)

// roleInstaller marks a one-shot installer container. They are removed by the
// install that ran them; one whose node was restarted meanwhile ran to its end
// and stayed behind as Exited, and nothing else knew it was ours.
const (
	roleInstaller    = "installer"
	staleImagePeriod = 24 * time.Hour
)

// daemonLogDriver is the Docker daemon's default log driver, read once at
// start. Empty until known.
var daemonLogDriver atomic.Value

// boundedLogConfig caps a container's own log where the driver takes a size.
// Docker's default json-file grows without limit; any other driver may refuse
// these options and the container would not be created, so it is left alone.
func boundedLogConfig(driver string) container.LogConfig {
	switch driver {
	case "json-file", "local":
		return container.LogConfig{Type: driver, Config: map[string]string{"max-size": "10m", "max-file": "3"}}
	}
	return container.LogConfig{}
}

func currentLogConfig() container.LogConfig {
	d, _ := daemonLogDriver.Load().(string)
	return boundedLogConfig(d)
}

// isStaleRuntimeImage reports a Minecraft runtime image of ours that no tag
// points at any more - the previous build after a rebuild - or one under the
// retired repository name. Removal is never forced, so one a container still
// uses stays.
func isStaleRuntimeImage(img dockerimage.Summary) bool {
	ours := func(ref string) bool {
		return strings.HasPrefix(ref, currentImagePrefix+"mc-java") || strings.HasPrefix(ref, legacyImagePrefix+"mc-java")
	}
	if len(img.RepoTags) == 0 {
		for _, d := range img.RepoDigests {
			if ours(d) {
				return true
			}
		}
		return false
	}
	for _, t := range img.RepoTags {
		if !strings.HasPrefix(t, legacyImagePrefix+"mc-java") {
			return false
		}
	}
	return true
}

// startDockerHygiene reads the log driver, removes installer containers a
// restart left, and keeps removing stale runtime images: each rebuild left
// about 300 MB per Java version on every node, for good.
func (dm *DockerManager) startDockerHygiene(ctx context.Context) {
	infoCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if info, err := dm.cli.Info(infoCtx); err == nil {
		daemonLogDriver.Store(info.LoggingDriver)
		// Docker then creates every server with no memory limit and says so
		// only in its own daemon log: the booked RAM is not enforced.
		if !info.MemoryLimit {
			log.Printf("WARNING: this host has no memory cgroup, so server RAM limits are NOT enforced. " +
				"On Raspberry Pi OS add 'cgroup_enable=memory' to /boot/firmware/cmdline.txt and reboot.")
		}
	}
	cancel()

	go func() {
		dm.removeOrphanInstallers(ctx)
		t := time.NewTicker(staleImagePeriod)
		defer t.Stop()
		for {
			dm.removeStaleRuntimeImages(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

func (dm *DockerManager) removeOrphanInstallers(ctx context.Context) {
	list, err := dm.cli.ContainerList(ctx, container.ListOptions{
		All: true,
		// This node's only: another node on the same host may be reading
		// the log of an installer that just exited.
		Filters: filters.NewArgs(
			filters.Arg("label", roleLabel+"="+roleInstaller),
			filters.Arg("label", ownerLabel+"="+nodeIdentity(nodeSecretDir)),
		),
	})
	if err != nil {
		return
	}
	for _, c := range list {
		// Exited only. One still running, or created and about to start,
		// belongs to an install in progress; it is removed at a later start.
		if c.State == "exited" || c.State == "dead" {
			if dm.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{}) == nil {
				log.Printf("cleanup: removed installer container %s left by a restart", shortID(c.ID))
			}
		}
	}
}

func (dm *DockerManager) removeStaleRuntimeImages(ctx context.Context) {
	images, err := dm.cli.ImageList(ctx, dockerimage.ListOptions{})
	if err != nil {
		return
	}
	for _, img := range images {
		if !isStaleRuntimeImage(img) {
			continue
		}
		if _, err := dm.cli.ImageRemove(ctx, img.ID, dockerimage.RemoveOptions{}); err == nil {
			log.Printf("cleanup: removed stale runtime image %s (%d MB)", shortID(img.ID), img.Size>>20)
		}
	}
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
