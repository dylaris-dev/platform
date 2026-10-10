package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	// The node image is alpine without tzdata, so without this TZ would fall
	// back to UTC and LINK_UPDATE_TIME would mean UTC on every customer host.
	_ "time/tzdata"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Start-first update of the Link that runs beside a BYON/External node.
//
// Once a day the node pulls the Link's image. When it changed, a clone
// ("<name>-next") starts on the new image; once that one has a tunnel up, the
// old one is renamed "<name>-draining", loses its restart policy and gets
// SIGTERM. It then refuses new players, keeps its current ones for up to
// LINK_DRAIN_TIMEOUT and exits by itself; after that the clone takes the old
// name. Nobody is disconnected.
//
// Every step is read back from Docker on each tick, not remembered, so a node
// restart anywhere in the middle continues where it stopped. "-draining" (and
// "-abandoned" for a clone given up on) marks a Link that is to be signalled
// exactly once: a second signal ends the drain at once and drops every player
// still on it. Whether it already got one is read from its own log, never
// assumed from the name, so a node stopped between the rename and the signal
// (watchtower updates the node) cannot leave it running forever.
const (
	linkComponentLabel  = "dylaris.component=link"
	linkNextSuffix      = "-next"
	linkDrainingSuffix  = "-draining"
	linkAbandonedSuffix = "-abandoned"
	// Logged by gateway/link: tunnel.go when a tunnel to an edge comes up and
	// when it drops, cmd/standalone/main.go on every signal (since the first
	// release, so also by a Link too old to drain).
	linkTunnelUpPrefix   = "Secure Tunnel established to "
	linkTunnelLostPrefix = "Connection to "
	linkSignalLine       = "Received signal"
	linkServeTimeout     = 3 * time.Minute
	linkUpdateTick       = time.Minute
	linkLogReadMax       = 16 << 20
	linkOldLogTail       = "20000"
	linkStepTimeout      = 30 * time.Second
)

type linkCtr struct {
	ID, Name, ImageID string
	Running           bool
}

type linkStep int

const (
	linkIdle          linkStep = iota // one Link, nothing in flight: update it when due
	linkSkip                          // nothing safe to act on; reason says why
	linkWait                          // the old Link is draining
	linkCheckClone                    // clone up, old not drained yet
	linkFinish                        // old one gone or exited: promote the clone
	linkRemoveClone                   // clone not running while the old one still serves
	linkRemoveDrained                 // a drained Link left behind with no clone
)

type linkPlan struct {
	step   linkStep
	reason string
	target *linkCtr // linkIdle
	clone  *linkCtr // linkCheckClone, linkFinish, linkRemoveClone
	old    *linkCtr // linkCheckClone, linkWait, linkRemoveDrained; may be nil on linkFinish
	base   string   // linkFinish: the name the clone takes
}

func skipLink(reason string) linkPlan { return linkPlan{step: linkSkip, reason: reason} }

// planLinkUpdate decides what the Link containers on this host need next. It
// acts only on a state it fully recognises; anything else is a skip, because
// the cost of a wrong move here is every player on the host.
func planLinkUpdate(cs []linkCtr) linkPlan {
	var plain, next, draining []*linkCtr
	abandoned := 0
	for i := range cs {
		c := &cs[i]
		switch {
		case strings.HasSuffix(c.Name, linkAbandonedSuffix):
			abandoned++ // handled by the tick on its own; it only blocks a new update
		case strings.HasSuffix(c.Name, linkNextSuffix):
			next = append(next, c)
		case strings.HasSuffix(c.Name, linkDrainingSuffix):
			draining = append(draining, c)
		default:
			plain = append(plain, c)
		}
	}
	if len(next) > 1 || len(draining) > 1 {
		return skipLink("more than one Link update in flight")
	}
	if len(next) == 0 {
		if len(draining) == 1 {
			if draining[0].Running {
				return linkPlan{step: linkWait, old: draining[0]}
			}
			return linkPlan{step: linkRemoveDrained, old: draining[0]}
		}
		var running []*linkCtr
		for _, p := range plain {
			if p.Running {
				running = append(running, p)
			}
		}
		switch len(running) {
		case 0:
			return skipLink("no running Link")
		case 1:
			if abandoned > 0 {
				return skipLink("an abandoned clone is still draining")
			}
			return linkPlan{step: linkIdle, target: running[0]}
		default:
			return skipLink(fmt.Sprintf("%d running Links, expected one", len(running)))
		}
	}

	clone := next[0]
	base := strings.TrimSuffix(clone.Name, linkNextSuffix)
	var drained, old *linkCtr
	if len(draining) == 1 {
		if draining[0].Name != base+linkDrainingSuffix {
			return skipLink(fmt.Sprintf("%s and %s do not belong together", clone.Name, draining[0].Name))
		}
		drained = draining[0]
	}
	for _, p := range plain {
		if p.Name == base {
			old = p
			continue
		}
		if p.Running {
			return skipLink(fmt.Sprintf("%s runs beside the update of %s", p.Name, base))
		}
	}
	if old != nil && drained != nil {
		return skipLink(fmt.Sprintf("both %s and %s exist", old.Name, drained.Name))
	}

	if !clone.Running {
		if old != nil && old.Running {
			return linkPlan{step: linkRemoveClone, clone: clone, old: old}
		}
		return skipLink(fmt.Sprintf("%s is not running and no Link before it is either", clone.Name))
	}
	if drained != nil {
		if drained.Running {
			return linkPlan{step: linkWait, old: drained}
		}
		return linkPlan{step: linkFinish, clone: clone, old: drained, base: base}
	}
	if old != nil && old.Running {
		return linkPlan{step: linkCheckClone, clone: clone, old: old}
	}
	return linkPlan{step: linkFinish, clone: clone, old: old, base: base}
}

type cloneVerdict int

const (
	cloneWait cloneVerdict = iota
	cloneDrainOld
	cloneAbort
)

// judgeClone counts from the clone's CREATION: StartedAt resets on every
// restart, and a clone in a restart loop would never time out.
func judgeClone(serving bool, created, now time.Time) cloneVerdict {
	if serving {
		return cloneDrainOld
	}
	if now.Sub(created) >= linkServeTimeout {
		return cloneAbort
	}
	return cloneWait
}

// tunnelsUp replays a Link's log and counts the edges it holds a tunnel to at
// the end of it.
func tunnelsUp(logText string) int {
	up := map[string]bool{}
	for _, line := range strings.Split(logText, "\n") {
		if _, rest, ok := strings.Cut(line, linkTunnelUpPrefix); ok {
			addr, _, _ := strings.Cut(rest, " ")
			up[addr] = true
		} else if _, rest, ok := strings.Cut(line, linkTunnelLostPrefix); ok {
			addr, _, _ := strings.Cut(rest, " ")
			delete(up, addr)
		}
	}
	return len(up)
}

// cloneReady: the clone serves only once it holds a tunnel to at least as many
// edges as the old Link. Draining on the first tunnel would leave every edge
// the clone has not reached yet routing new players to a Link that refuses
// them.
func cloneReady(cloneUp, oldUp int) bool { return cloneUp >= 1 && cloneUp >= oldUp }

func parseLinkUpdateTime(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, fmt.Errorf("LINK_UPDATE_TIME %q is not HH:MM: %w", s, err)
	}
	return t.Hour(), t.Minute(), nil
}

// linkUpdateDue: once per local day, at or after hour:minute. A node that
// starts after that time the same day runs it once at start.
func linkUpdateDue(now time.Time, hour, minute int, lastRun string) bool {
	if now.Format(time.DateOnly) == lastRun {
		return false
	}
	return now.Hour()*60+now.Minute() >= hour*60+minute
}

// imageDefaults is what a container inherits from its image. Inspect reports
// those merged into the container's own config; copying them onto the clone
// would pin the OLD image's values over the new image's.
type imageDefaults struct {
	Env, Cmd, Entrypoint, Healthcheck []string
	Labels                            map[string]string
}

func defaultsOf(img dockerimage.InspectResponse) imageDefaults {
	if img.Config == nil {
		return imageDefaults{}
	}
	d := imageDefaults{Env: img.Config.Env, Cmd: img.Config.Cmd, Entrypoint: img.Config.Entrypoint, Labels: img.Config.Labels}
	if img.Config.Healthcheck != nil {
		d.Healthcheck = img.Config.Healthcheck.Test
	}
	return d
}

// buildLinkClone copies the old Link's container for re-creation. Config.Image
// stays the REFERENCE (just pulled, so it resolves to the new image), not the
// image id: the clone becomes the Link, and the next update pulls that string.
func buildLinkClone(info container.InspectResponse, img imageDefaults) (*container.Config, *container.HostConfig, *network.NetworkingConfig, string) {
	cfg := *info.Config
	cfg.Env = nil
	for _, e := range info.Config.Env {
		if !slices.Contains(img.Env, e) {
			cfg.Env = append(cfg.Env, e)
		}
	}
	if slices.Equal(info.Config.Cmd, img.Cmd) {
		cfg.Cmd = nil
	}
	if slices.Equal(info.Config.Entrypoint, img.Entrypoint) {
		cfg.Entrypoint = nil
	}
	if info.Config.Healthcheck != nil && img.Healthcheck != nil && slices.Equal(info.Config.Healthcheck.Test, img.Healthcheck) {
		cfg.Healthcheck = nil
	}
	cfg.Labels = map[string]string{}
	for k, v := range info.Config.Labels {
		if iv, ok := img.Labels[k]; !ok || iv != v {
			cfg.Labels[k] = v
		}
	}
	short := shortContainerID(info.ID)
	// Docker's default hostname is the container's own short id; copied, the
	// clone would carry the old one's.
	if cfg.Hostname == short {
		cfg.Hostname = ""
	}

	var hc container.HostConfig
	if info.HostConfig != nil {
		hc = *info.HostConfig
	}

	// Only what the user configured travels: no static address or MAC, which
	// the still-running old Link holds, and not the old short id alias.
	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	if info.NetworkSettings != nil {
		for name, ep := range info.NetworkSettings.Networks {
			if ep == nil {
				continue
			}
			var aliases []string
			for _, a := range ep.Aliases {
				if a != short {
					aliases = append(aliases, a)
				}
			}
			nc.EndpointsConfig[name] = &network.EndpointSettings{Aliases: aliases, Links: ep.Links, DriverOpts: ep.DriverOpts, GwPriority: ep.GwPriority}
		}
	}
	return &cfg, &hc, nc, strings.TrimPrefix(info.Name, "/") + linkNextSuffix
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

type linkUpdater struct {
	cli          *client.Client
	project      string // compose project of the node's own container; "" = label alone
	hour, minute int
	lastRun      string
}

// StartLinkUpdater runs only where LINK_AUTO_UPDATE=true: the BYON/External
// kits set it, nothing else does.
func StartLinkUpdater(ctx context.Context, dm *DockerManager) {
	if !parseBoolEnvDefault("LINK_AUTO_UPDATE", false) {
		return
	}
	at := os.Getenv("LINK_UPDATE_TIME")
	if strings.TrimSpace(at) == "" {
		at = "04:00"
	}
	h, m, err := parseLinkUpdateTime(at)
	if err != nil {
		log.Printf("link update: DISABLED: %v", err)
		return
	}
	u := &linkUpdater{cli: dm.cli, hour: h, minute: m}
	if ref := selfContainerRef(); ref != "" {
		if self, err := dm.cli.ContainerInspect(ctx, ref); err == nil && self.Config != nil {
			u.project = self.Config.Labels["com.docker.compose.project"]
		}
	}
	log.Printf("link update: enabled, daily at %02d:%02d %s, compose project %q", h, m, time.Local, u.project)
	t := time.NewTicker(linkUpdateTick)
	defer t.Stop()
	for {
		u.tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (u *linkUpdater) list(ctx context.Context) ([]linkCtr, error) {
	f := filters.NewArgs(filters.Arg("label", linkComponentLabel))
	if u.project != "" {
		f.Add("label", "com.docker.compose.project="+u.project)
	}
	cs, err := u.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, fmt.Errorf("list link containers: %w", err)
	}
	var out []linkCtr
	for _, c := range cs {
		// A Swarm task belongs to its orchestrator, which updates it itself.
		if c.Labels["com.docker.swarm.service.id"] != "" || len(c.Names) == 0 {
			continue
		}
		out = append(out, linkCtr{ID: c.ID, Name: strings.TrimPrefix(c.Names[0], "/"), ImageID: c.ImageID, Running: c.State == container.StateRunning})
	}
	return out, nil
}

func (u *linkUpdater) tick(ctx context.Context, now time.Time) {
	cs, err := u.list(ctx)
	if err != nil {
		log.Printf("link update: %v", err)
		return
	}
	due := linkUpdateDue(now, u.hour, u.minute, u.lastRun)
	if due {
		u.lastRun = now.Format(time.DateOnly)
	}
	for i := range cs {
		if c := &cs[i]; strings.HasSuffix(c.Name, linkAbandonedSuffix) {
			if c.Running {
				u.ensureSignalled(ctx, c)
			} else {
				log.Printf("link update: removing %s, which has finished draining", c.Name)
				u.remove(ctx, c, false)
			}
		}
	}
	p := planLinkUpdate(cs)
	switch p.step {
	case linkSkip:
		if due {
			log.Printf("link update: skipped: %s", p.reason)
		}
	case linkIdle:
		if due {
			u.update(ctx, p.target)
		}
	case linkWait:
		u.ensureSignalled(ctx, p.old)
		if due {
			log.Printf("link update: skipped, %s is still draining", p.old.Name)
		}
	case linkCheckClone:
		u.checkClone(ctx, p.clone, p.old, now)
	case linkFinish:
		u.finish(ctx, p.clone, p.old, p.base)
	case linkRemoveClone:
		log.Printf("link update: ABORTED, %s is not running; removing it, %s keeps serving", p.clone.Name, p.old.Name)
		u.remove(ctx, p.clone, true)
	case linkRemoveDrained:
		log.Printf("link update: removing %s, which has finished draining", p.old.Name)
		u.remove(ctx, p.old, false)
	}
}

func (u *linkUpdater) update(ctx context.Context, target *linkCtr) {
	info, err := u.cli.ContainerInspect(ctx, target.ID)
	if err != nil || info.Config == nil {
		log.Printf("link update: cannot inspect %s: %v", target.Name, err)
		return
	}
	ref := info.Config.Image
	rc, err := u.cli.ImagePull(ctx, ref, dockerimage.PullOptions{})
	if err != nil {
		log.Printf("link update: pull %s for %s: %v", ref, target.Name, err)
		return
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()
	// The pull stream carries its own errors; what the tag resolves to now is
	// the answer either way.
	newImg, err := u.cli.ImageInspect(ctx, ref)
	if err != nil {
		log.Printf("link update: inspect pulled %s: %v", ref, err)
		return
	}
	if newImg.ID == info.Image {
		log.Printf("link update: %s is up to date (%s)", target.Name, ref)
		return
	}
	oldImg, err := u.cli.ImageInspect(ctx, info.Image)
	if err != nil {
		log.Printf("link update: ABORTED, cannot inspect the image %s runs on: %v", target.Name, err)
		return
	}
	cfg, hc, nc, name := buildLinkClone(info, defaultsOf(oldImg))
	created, err := u.cli.ContainerCreate(ctx, cfg, hc, nc, nil, name)
	if err != nil {
		// Nothing to clean up: a failed create leaves no container, and a name
		// conflict means one we did not make and must not remove.
		log.Printf("link update: ABORTED, create %s: %v; %s keeps serving", name, err, target.Name)
		return
	}
	if err := u.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		log.Printf("link update: ABORTED, start %s: %v; %s keeps serving", name, err, target.Name)
		u.remove(ctx, &linkCtr{ID: created.ID, Name: name}, true)
		return
	}
	log.Printf("link update: STARTED, %s runs %s (%s); %s is drained once its tunnel is up", name, ref, newImg.ID, target.Name)
}

func (u *linkUpdater) checkClone(ctx context.Context, clone, old *linkCtr, now time.Time) {
	info, err := u.cli.ContainerInspect(ctx, clone.ID)
	if err != nil || info.ContainerJSONBase == nil {
		log.Printf("link update: cannot inspect %s: %v", clone.Name, err)
		return
	}
	created, _ := time.Parse(time.RFC3339Nano, info.Created)
	cloneLog, err := u.logs(ctx, clone.ID, container.LogsOptions{})
	if err != nil {
		log.Printf("link update: read logs of %s: %v", clone.Name, err)
		return
	}
	// The old Link may have run for weeks: its recent lines are enough to know
	// which tunnels it holds now.
	oldLog, err := u.logs(ctx, old.ID, container.LogsOptions{Tail: linkOldLogTail})
	if err != nil {
		log.Printf("link update: read logs of %s: %v", old.Name, err)
		return
	}
	cloneUp, oldUp := tunnelsUp(cloneLog), tunnelsUp(oldLog)
	switch judgeClone(cloneReady(cloneUp, oldUp), created, now) {
	case cloneDrainOld:
		log.Printf("link update: %s holds %d tunnels (%s holds %d); new players go to it", clone.Name, cloneUp, old.Name, oldUp)
		u.retire(ctx, old, old.Name+linkDrainingSuffix)
	case cloneAbort:
		// Not removed: it may already carry players. It drains like an old one.
		log.Printf("link update: ABORTED, %s holds %d of %d tunnels after %s; it drains, %s keeps serving", clone.Name, cloneUp, oldUp, linkServeTimeout, old.Name)
		u.retire(ctx, clone, strings.TrimSuffix(clone.Name, linkNextSuffix)+linkAbandonedSuffix)
	}
}

// logs returns a container's log as text. Compose runs the Link without a TTY,
// so the stream is multiplexed; a TTY container's raw stream is used as is.
func (u *linkUpdater) logs(ctx context.Context, id string, opts container.LogsOptions) (string, error) {
	opts.ShowStdout, opts.ShowStderr = true, true
	rc, err := u.cli.ContainerLogs(ctx, id, opts)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	raw, err := io.ReadAll(io.LimitReader(rc, linkLogReadMax))
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, bytes.NewReader(raw)); err != nil {
		return string(raw), nil
	}
	return out.String(), nil
}

// retire renames a running Link to its drain name, then signals it once. It
// runs past a cancelled ctx: stopping half-way is what ensureSignalled exists
// to repair, not something to cause.
func (u *linkUpdater) retire(ctx context.Context, c *linkCtr, drainName string) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), linkStepTimeout)
	defer cancel()
	// Renamed FIRST: from then on every tick treats it as draining, never as a
	// Link to update or to compare the clone against.
	if err := u.cli.ContainerRename(sctx, c.ID, drainName); err != nil {
		log.Printf("link update: rename %s to %s: %v", c.Name, drainName, err)
		return
	}
	was := c.Name
	c.Name = drainName
	if u.ensureSignalled(sctx, c) {
		log.Printf("link update: %s (was %s) is draining: it keeps its players and takes no new ones", drainName, was)
	}
}

// ensureSignalled sends SIGTERM to a draining Link that has had no signal since
// it last started, and never to one that has. Any error leaves it as it is and
// the next tick asks again; there is no rename back. Reports whether the Link
// is draining now.
func (u *linkUpdater) ensureSignalled(ctx context.Context, c *linkCtr) bool {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), linkStepTimeout)
	defer cancel()
	info, err := u.cli.ContainerInspect(sctx, c.ID)
	if err != nil || info.State == nil || !info.State.Running {
		return false
	}
	text, err := u.logs(sctx, c.ID, container.LogsOptions{Since: info.State.StartedAt})
	if err != nil {
		log.Printf("link update: read logs of %s: %v", c.Name, err)
		return false
	}
	if strings.Contains(text, linkSignalLine) {
		return true
	}
	// Otherwise Docker brings it back once the drain ends.
	if _, err := u.cli.ContainerUpdate(sctx, c.ID, container.UpdateConfig{RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled}}); err != nil {
		log.Printf("link update: clear restart policy of %s: %v", c.Name, err)
		return false
	}
	if err := u.cli.ContainerKill(sctx, c.ID, "SIGTERM"); err != nil {
		log.Printf("link update: signal %s: %v", c.Name, err)
		return false
	}
	log.Printf("link update: sent SIGTERM to %s", c.Name)
	return true
}

func (u *linkUpdater) finish(ctx context.Context, clone, old *linkCtr, base string) {
	if old != nil {
		// No force: the plan saw it exited, and a Link that came back since is
		// not ours to kill.
		if err := u.cli.ContainerRemove(ctx, old.ID, container.RemoveOptions{}); err != nil {
			log.Printf("link update: remove %s: %v", old.Name, err)
			return
		}
	}
	if err := u.cli.ContainerRename(ctx, clone.ID, base); err != nil {
		log.Printf("link update: rename %s to %s: %v", clone.Name, base, err)
		return
	}
	log.Printf("link update: DONE, %s is now %s", clone.Name, base)
	if old != nil && old.ImageID != "" && old.ImageID != clone.ImageID {
		// No force: an image another container still uses stays.
		if _, err := u.cli.ImageRemove(ctx, old.ImageID, dockerimage.RemoveOptions{PruneChildren: true}); err != nil {
			log.Printf("link update: old image %s kept: %v", old.ImageID, err)
		}
	}
}

func (u *linkUpdater) remove(ctx context.Context, c *linkCtr, force bool) {
	if err := u.cli.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: force}); err != nil && !client.IsErrNotFound(err) {
		log.Printf("link update: remove %s: %v", c.Name, err)
	}
}
