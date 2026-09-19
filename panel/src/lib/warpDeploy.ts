/**
 * Deploy snippets for an external (BYON) node.
 *
 * The panel used to show four ENV lines plus a bare `docker swarm join`, which
 * is not enough to bring a machine up: the operator still had to know that warp
 * must start before the node, that the Link is a service of its own, that
 * the node must NOT get a CLUSTER_SECRET, and which addresses are reachable only
 * over the overlay. All of that is encoded here instead.
 *
 * Values the operator must fill in are left as obvious <placeholders> rather
 * than guessed: an address that merely looks plausible is worse than a blank,
 * because it fails later and somewhere else. Everything the panel CAN answer
 * (the two keys, the node id, the overlay addresses) is filled in, so a
 * complete deploy is a copy-paste rather than a scavenger hunt.
 *
 * Lines whose value equals the image default are deliberately absent: LEADER is
 * false unless set. The node starts no Link (since 2026.09.12), so a file that
 * should run one says so itself. Every line left here is one the reader has to
 * be able to justify.
 */

export type WarpDeployInput = {
    /** Plaintext warp enrollment key. Shown once at mint time. */
    apiKey: string;
    /** Core's public base URL, no trailing /api. */
    enrollUrl: string;
    /** Overlay CIDR where Redis and core gRPC live, e.g. 10.20.0.0/16. */
    tunnelSubnets?: string;
    /** Single-use node enroll token, when the operator has one. */
    nodeEnrollToken?: string;
    /**
     * Core's gRPC certificate fingerprint, from the enroll-token answer or the
     * deploy config (see kitGrpcTlsFingerprint). Core sends a value ONLY when
     * GRPC_TLS_ENABLED is on, so a value turns TLS on for the node too and ""
     * says plaintext - the two must match or the node cannot reach Core at all.
     * undefined means not known yet, which is neither.
     */
    grpcTlsFingerprint?: string;
    /** Stable id for the machine. */
    nodeId?: string;
    /** Route-only: the local host(s) the link may dial, comma-separated. Hosts only, no port. */
    localTarget?: string;
    /**
     * Which machine the snippet is for. Only route-only differs, and only in
     * where the customer's own server is reachable from - see
     * defaultLocalTarget. Defaults to linux.
     */
    platform?: DeployPlatform;
    /**
     * Node kit only: run the Link as a service of this file. The node starts no
     * Link itself (since 2026.09.12). Only for a key Core can answer with a
     * node's Link - a node- key that is bound to its machine, or will be when the
     * machine enrols with the token minted beside it: a tenant's node key, or an
     * admin's External node key. Core refuses any other key, which is why this
     * is opt-in rather than the default.
     */
    linkBesideNode?: boolean;
    /**
     * Node kit without a Link only: the key is an admin key minted before
     * External node keys existed. It changes only what the warning tells the
     * reader to do, because such a key can never be bound to a machine - the
     * tenant's way out ("bind one under the machine") does not exist for it.
     */
    legacyAdminKey?: boolean;
    /**
     * Node kit only: the file is an admin's, for an External node - a machine
     * the PLATFORM runs outside the datacenter. Set only by the admin Warp
     * dialog, never derived from the other fields. It changes the wording that
     * speaks to the reader as the machine's owner, because here the reader is
     * an operator and the machine belongs to nobody. Unset, the file is
     * byte-for-byte the tenant's.
     */
    externalNode?: boolean;
};

export type DeployPlatform = 'linux' | 'windows';

/**
 * Where the link can reach the customer's own Minecraft server.
 *
 * On Linux both containers are host-networked, so the host's loopback IS the
 * customer's loopback and 127.0.0.1 is right.
 *
 * On Docker Desktop it is not. `network_mode: host` there joins the WSL2 VM's
 * network namespace, not Windows', so 127.0.0.1 inside the link is the VM - a
 * server running on Windows is simply not there. Docker Desktop publishes the
 * Windows host as `host.docker.internal`, which is what actually reaches it.
 * (Measured: a host-networked port is NOT reachable from Windows, while
 * host.docker.internal resolves and connects.)
 *
 * warp and the link still find each other over the VM's 127.0.0.1 on both
 * platforms, because they share that namespace - only the customer's own
 * server sits outside it.
 */
export function defaultLocalTarget(platform: DeployPlatform | undefined): string {
    return platform === 'windows' ? 'host.docker.internal' : '127.0.0.1';
}

const REG = 'ghcr.io/dylaris-dev';

/**
 * Port warp binds locally for its Redis proxy.
 *
 * The same number is compiled in two other places - gateway/warp/proxy.go and
 * platform/node/warp_proxy.go - because there is no channel between the three
 * to negotiate a pair. A machine with a collision sets REDIS_ADDR explicitly,
 * which bypasses the proxy entirely.
 */
const WARP_PROXY_REDIS_PORT = '25571';

function or(value: string | undefined, placeholder: string): string {
    const v = (value ?? '').trim();
    return v === '' ? placeholder : v;
}

/**
 * The node's half of the Core gRPC pin. ALWAYS emitted, in one of three shapes.
 *
 * Core returns a fingerprint exclusively while GRPC_TLS_ENABLED is on, so a
 * value here means the control channel IS TLS and the node must match it. It
 * does not verify the hostname - it compares this fingerprint - which is also
 * why warp's local proxy in front of it changes nothing.
 *
 * The plaintext case ("") must be written out rather than left off, because the
 * node now defaults GRPC_TLS_ENABLED to TRUE. Omitting the line used to mean
 * "plaintext, same as Core"; with the default flipped it means "TLS, and no pin
 * to verify it with", which is a boot-time fatal on a BYON machine that holds no
 * CLUSTER_SECRET. Saying false explicitly keeps the snippet a complete
 * description of what the node should do instead of one that leans on a default
 * that has since changed underneath it.
 *
 * Not known (undefined) is a placeholder, never false: false against a TLS Core
 * is a node that dials plaintext and never reaches it. The placeholder is not a
 * boolean, so a node started with it keeps its TLS default and stops at boot
 * saying it has no pin - loud, where false fails quietly.
 */
function grpcTlsLines(fingerprint: string | undefined): string {
    if (fingerprint === undefined) {
        return `      # EDIT - this page could not load whether our control channel runs TLS.
      # Reload it before you deploy; the file then fills this in.
      GRPC_TLS_ENABLED: "<reload this page>"

`;
    }
    const fp = fingerprint.trim();
    if (fp === '') {
        return `      # keep - this platform runs the control channel in plaintext, and the
      # node defaults to TLS, so the opt-out has to be explicit.
      GRPC_TLS_ENABLED: "false"

`;
    }
    return `      # keep - the control channel is TLS, and the node pins this fingerprint
      # instead of a hostname. It must match ours exactly.
      GRPC_TLS_ENABLED: "true"
      GRPC_TLS_FINGERPRINT: "${fp}"

`;
}

/**
 * The fingerprint a node kit is written with. One handed over with a fresh
 * enroll token wins; every other kit (Update this machine, Deploy file, a rolled
 * key, the operator's deploy dialog) reads Core's deploy config. Those kits used
 * to get nothing at all and wrote plaintext against a TLS Core. undefined while
 * the config has not loaded, which grpcTlsLines writes as a placeholder.
 */
export function kitGrpcTlsFingerprint(
    explicit: string | undefined,
    config: { grpcTlsFingerprint?: string } | null | undefined,
): string | undefined {
    return explicit ?? config?.grpcTlsFingerprint;
}

/**
 * Turns the location name a customer typed into a usable NODE_ID.
 *
 * NODE_ID ends up in Redis keys, the mesh identity and the environment of every
 * container the node starts, so "My Home PC" cannot be pre-filled verbatim.
 * Returns undefined when nothing usable is left, which leaves the snippet's
 * placeholder in place rather than a value that is silently wrong.
 */
export function nodeIdFromLabel(label: string | undefined): string | undefined {
    const slug = (label ?? '')
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, '-')
        .replace(/^-+|-+$/g, '')
        .slice(0, 40)
        .replace(/-+$/, '');
    return slug === '' ? undefined : slug;
}

/**
 * routeOnlyCompose is ONE container, the link: the customer runs the Minecraft
 * server themselves and gets a protected Dylaris address for it. No node, no
 * swarm join, no published ports, and no tunnel into their network - the link
 * talks to our API over HTTPS with the kit key and dials out to the edges.
 *
 * It is host-networked so a server on loopback or the LAN is reached exactly as
 * the customer enters the target, so anything it binds lands on their own
 * machine. Its management server is therefore pinned to loopback below: nothing
 * reads it, and its /health is unauthenticated.
 */
export function routeOnlyCompose(i: WarpDeployInput): string {
    // The two lines below are not the same knob, and writing one value into
    // both breaks Docker Desktop.
    //
    // LINK_ALLOWED_TARGETS is the allow list for a target that is NOT loopback:
    // the link always accepts 127.0.0.1 (gateway/link/session.go
    // isAllowedDialTarget), so this line only matters for a customer whose
    // server sits on another machine - which is why it takes the address their
    // routes point at.
    //
    // LOCAL_HOST is what 127.0.0.1 is REWRITTEN to before dialling, and nothing
    // else (session.go). On Docker Desktop that rewrite is the only reason a
    // server running on Windows is reachable at all, so it stays the platform's
    // own answer and must never follow a route target.
    const allowedTarget = or(i.localTarget, defaultLocalTarget(i.platform));
    const localHost = defaultLocalTarget(i.platform);
    // A route-only key is a link key, not a warp key; the placeholder says so.
    const key = i.apiKey === KEY_PLACEHOLDER ? '<your-link-key>' : i.apiKey;
    // Which machine the file is for, in the file: the wrong one fails with
    // nothing but a dial error in the link's log.
    const header = i.platform === 'windows'
        ? `# Made for Docker Desktop on Windows. On a Linux host, take the Linux file.
# On Docker Desktop, host networking is the WSL2 VM's rather than Windows',
# so your own server is reached at host.docker.internal.`
        : `# Made for Linux. On Docker Desktop (Windows), take the Windows file.
# No VPN and no extra privileges: every connection is outbound.`;
    return `# route-only.yml
#
# link hands your own server to the gateway. It talks to our API with the key
# below and opens outbound tunnels to our edges. Nothing is published and no
# port is opened.
${header}
#
# Lines marked "keep" are filled in for this link and must stay as they are.
# Only a line marked EDIT is yours to change.

services:
  link:
    image: ${REG}/gateway-link:latest
    restart: unless-stopped
    environment:
      # keep - this link's key. Shown once; we store only a hash of it. It is
      # the only credential on this machine, and revoking it in the panel is
      # the whole revocation.
      LINK_KEY: "${key}"

      # keep - our API. It lives at the panel's own address.
      CORE_URL: "${or(i.enrollUrl, '<core-url>')}"

      # EDIT if your server is not on this machine. Host only, NO port: it is
      # compared as an exact string, so a "host:25565" here never matches.
      # Several servers: separate them with commas.
      LINK_ALLOWED_TARGETS: "${allowedTarget}"

      # keep - where this link looks for a server you told us runs "here".
      LOCAL_HOST: "${localHost}"

      # keep - loopback, so this unauthenticated status port stays off your LAN.
      LINK_PORT: "127.0.0.1:25540"

      # EDIT if you want a shorter window. When you stop or update link, it keeps
      # the players already on your server until the last one leaves, and only
      # then shuts down - nobody is kicked. It takes no new players while it
      # waits. This is how long it waits before giving up on whoever is left.
      LINK_DRAIN_TIMEOUT: "6h"
    network_mode: host
    # keep - a little longer than LINK_DRAIN_TIMEOUT, so link finishes on its own
    # terms instead of being killed with players still on it.
    stop_grace_period: 6h10m
`;
}

/**
 * nodeCompose is the full managed-node stack: warp joins the overlay, then the
 * node agent runs MC containers on this machine.
 *
 * Two things here are load-bearing and easy to get wrong by hand: the node gets
 * NO CLUSTER_SECRET (it fetches a scoped Redis credential over gRPC after
 * enrolling, which is what keeps a customer machine from holding fleet
 * credentials), and the Link runs as a service of its own - the node starts none
 * any more. linkBesideNode says whether the key this file is written with can
 * actually BOOT one: Core answers link-boot only for a key bound to a machine,
 * so a file written with any other key gets a warning in place of the link
 * service rather than a container that cannot start. That is not an option to
 * choose; it is whether this file can be complete at all.
 *
 * The Link beside the node sits on a Docker network with the Minecraft servers
 * and reaches warp's proxy at the gateway of that network, which it works out
 * from its own routing table rather than from the host-gateway mapping below -
 * that names the DEFAULT bridge on Linux, and Windows itself on Docker Desktop,
 * and warp is listening on neither in the case that matters. The mapping stays
 * in the file because an older Link image still depends on it; a current one
 * ignores it.
 *
 * Docker Desktop is therefore no longer an exception: it gets the same file.
 */
export function nodeCompose(i: WarpDeployInput): string {
    const kitLink = i.linkBesideNode === true;
    // Docker Desktop's "host" is the WSL2 VM, not Windows. That is the same
    // adaptation route-only already makes, and it is the whole difference: the
    // node, its warp tunnel and the Minecraft containers all sit inside that VM
    // together, so they reach each other exactly as they would on Linux. What
    // changes is where the FILES land, which is why the note points at the bind
    // mount rather than at the networking.
    const header = i.platform === 'windows'
        ? `# On Docker Desktop, "host" networking is the WSL2 VM's rather than Windows'.
# The node, its tunnel and your servers all live in that VM, so they reach one
# another normally - but the server files land in the VM unless you bind a
# Windows path below.`
        : `# Kernel WireGuard needs host networking and NET_ADMIN.`;
    const intro = kitLink
        ? `# warp opens an outbound tunnel to us; the node runs your Minecraft servers on
# this machine, and link carries your players to them.`
        : `# warp opens an outbound tunnel to us; the node runs your Minecraft servers on
# this machine.
#
# WARNING - THIS FILE RUNS NO LINK, AND THE NODE NO LONGER STARTS ONE EITHER.
# Without a link, nobody can reach the servers on this machine: in gateway
# routing the link is the only way in and a server publishes no port of its own.
${i.legacyAdminKey
            ? `# A link needs an overlay key made for one machine, and this key cannot be one:
# it was minted before External node keys existed, and it can never boot a link.
# Mint a new External node key under Settings -> Warp -> External Nodes, then
# deploy the file offered there - it contains the link.`
            : `# A link needs an overlay key BOUND TO THIS MACHINE, which this key is not. Bind
# one under the machine in the panel, then take the file offered there - it
# contains the link.`}`;
    // No NODE_MANAGES_LINK: a current node reads it, ignores it and logs that it
    // does nothing, so a line marked "keep" beside that log line only makes the
    // reader doubt one of the two. The node has started no Link since 2026.09.12
    // and a fresh kit pulls a current image.
    const manageLink = '';
    const linkService = kitLink
        ? `
  link:
    image: ${REG}/gateway-link:latest
    restart: unless-stopped
    depends_on: [warp]
    environment:
      # keep - the same address as ENROLL_URL above.
      CORE_URL: "${or(i.enrollUrl, '<core-url>')}"

      # keep - the same key again. Once the node has enrolled, link trades it
      # for this node's own Link credentials, so no second secret travels with
      # this file. Until then it waits.
      LINK_BOOT_KEY: "${i.apiKey}"

      # keep - this machine is outside our network, so link reaches our edges
      # over the internet. Through the tunnel instead, your players would share
      # one connection with your own uploads.
      LINK_EXTERNAL: "true"

      # EDIT if you want a shorter window. When you stop or update link, it keeps
      # the players already on your servers until the last one leaves, and only
      # then shuts down - nobody is kicked. It takes no new players while it
      # waits. This is how long it waits before giving up on whoever is left.
      LINK_DRAIN_TIMEOUT: "6h"
    # keep - a little longer than LINK_DRAIN_TIMEOUT, so link finishes on its own
    # terms instead of being killed with players still on it.
    stop_grace_period: 6h10m
    # keep - only an older link image uses this. A current one works the address
    # out from the network it is on, which is the answer that is also right on
    # Docker Desktop.
    extra_hosts: ["host.docker.internal:host-gateway"]
    volumes:
      # keep - what link last got from us, so a restart comes up even while
      # our API cannot be reached.
      - link_data:/data
    # keep - the network the node starts your servers on; link reaches them there.
    networks: [dylaris_net]
`
        : '';
    const tail = kitLink
        ? `volumes:
  byon_data:
  link_data:

networks:
  # keep - named exactly, without this folder's name in front: the node starts
  # your servers on the network called dylaris_net, and on a machine that ran
  # an earlier version of this file it already exists with your servers on it.
  # Docker Compose then warns that it did not create that network and uses it
  # anyway, which is what should happen. No subnet is set; Docker's own ranges
  # stay clear of the tunnel's.
  dylaris_net:
    name: dylaris_net
`
        : `volumes:
  byon_data:
`;
    return `# byon-node.yml
#
${intro}
${header}
#
# Lines marked "keep" are filled in for this node and must stay as they are.
# Only a line marked EDIT is yours to change.

services:
  warp:
    image: ${REG}/gateway-warp:latest
    restart: unless-stopped
    environment:
      # keep - this node's key. Shown once; we store only a hash of it.
      API_KEY: "${i.apiKey}"

      # keep - our API. It lives at the panel's own address.
      ENROLL_URL: "${or(i.enrollUrl, '<core-url>')}"

      # keep - the network routed through the tunnel. NOT your home LAN.
      TUNNEL_SUBNETS: "${or(i.tunnelSubnets, '<overlay-cidr e.g. 10.20.0.0/16>')}"

      # keep - serve the proxy to containers too. The link sidecar and every
      # Minecraft server sit on a Docker bridge and cannot reach loopback here.
      PROXY_BIND_DOCKER_BRIDGES: "true"
    network_mode: host
    cap_add: [NET_ADMIN]

  node:
    image: ${REG}/platform-node:latest
    restart: unless-stopped
    depends_on: [warp]
    environment:
      # keep - ${i.externalNode
          ? 'this machine runs outside our datacenter, reached through warp.'
          : 'this machine is yours, not ours.'}
      NODE_EXTERNAL: "true"

${manageLink}      # EDIT only for a different name. It ends up in keys and in the
      # environment of every container, so letters, digits and dashes.
      NODE_ID: "${or(i.nodeId, '<stable-id-for-this-machine>')}"

      # keep - single-use, first boot only.
      NODE_ENROLL_TOKEN: "${or(i.nodeEnrollToken, '<enroll-token-from-panel>')}"

      # EDIT to "false" to drop the Beam LAN fast-path. It is a TLS listener on
      # :25523, and this container is host-networked, so it sits on your LAN.
      # Transfers then go through the relay instead.
      BEAM_LAN_FASTPATH: "true"

${grpcTlsLines(i.grpcTlsFingerprint)}      # No CORE_GRPC_ADDR, no REDIS_ADDR and no CLUSTER_SECRET, on purpose: the
      # node reaches us through warp's proxy, and it fetches a Redis credential
      # scoped to itself once it has enrolled. Nothing here changes if we move.
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /dev:/dev:ro

      # EDIT to keep the server files somewhere you can see:
      #   Linux           - /srv/dylaris:/app/dylaris_data
      #   Docker Desktop  - C:\\dylaris:/app/dylaris_data
      - byon_data:/app/dylaris_data
    network_mode: host
    cap_add: [SYS_ADMIN]
${linkService}
${tail}`;
}

/**
 * The input a deploy kit is rendered from, built in ONE place.
 *
 * It exists because the component used to assemble this object inline and
 * dropped `linkBesideNode` on the way: every customer file then came out
 * without the link service, which in gateway routing is the only way in, so no
 * player could reach any server on that machine. The compose functions were
 * tested directly and stayed green the whole time, because the defect was in
 * the hand-off, not in them. A pure function is the seam a test can hold.
 */
export type KitInputProps = {
    warpKey: string | null;
    enrollUrl: string;
    nodeEnrollToken?: string;
    grpcTlsFingerprint?: string;
    nodeId?: string;
    platform: DeployPlatform;
    config?: { tunnelSubnets?: string; grpcTlsFingerprint?: string } | null;
    /** An explicitly saved overlay CIDR, which wins over the detected one. */
    tunnelSubnets?: string;
    linkBesideNode?: boolean;
    localTarget?: string;
    externalNode?: boolean;
    legacyAdminKey?: boolean;
};

/**
 * What a caller passes to a kit component, which picks the platform itself.
 * Spread it rather than listing the fields again: a field listed by hand is a
 * field that can be forgotten, which is the defect this file exists to prevent.
 */
export type KitProps = Omit<KitInputProps, 'platform'>;

/** What kitInput puts in the file when the key is no longer known. */
export const KEY_PLACEHOLDER = '<your-warp-key>';

export function kitInput(p: KitInputProps): WarpDeployInput {
    return {
        apiKey: p.warpKey ?? KEY_PLACEHOLDER,
        enrollUrl: p.enrollUrl,
        nodeEnrollToken: p.nodeEnrollToken,
        grpcTlsFingerprint: kitGrpcTlsFingerprint(p.grpcTlsFingerprint, p.config),
        nodeId: p.nodeId,
        platform: p.platform,
        // Undetermined values stay undefined so the snippet keeps its
        // placeholder: a blank tells the reader something is missing, an empty
        // string looks like a setting that was deliberately cleared.
        tunnelSubnets: p.tunnelSubnets || p.config?.tunnelSubnets || undefined,
        linkBesideNode: p.linkBesideNode,
        localTarget: p.localTarget,
        externalNode: p.externalNode,
        legacyAdminKey: p.legacyAdminKey,
    };
}

/**
 * The LINK_ALLOWED_TARGETS value for a route-only file: every target the link's
 * routes point at, comma-separated, the form the link reads.
 *
 * All of them, not one. This used to fill in a target only when the routes
 * agreed on one and fall back to 127.0.0.1 otherwise, so a link with a route on
 * this machine and one on the LAN got a file that refused the LAN one, with
 * nothing shown anywhere. undefined when there are none, so the platform
 * default applies.
 */
export function allowedTargets(targets: string[]): string | undefined {
    const seen = [...new Set(targets.map(t => t.trim()).filter(t => t !== ''))];
    return seen.length > 0 ? seen.join(',') : undefined;
}

/**
 * Whether the generic node file - the one that runs NO Link - still says
 * anything true to this reader.
 *
 * That file warns, loudly, that nothing can reach the servers on the machine
 * without a Link, and tells the reader to bind an overlay key under the machine.
 * Correct for someone with no machine yet, or with one whose key is not bound.
 * Wrong, and alarming, for an owner whose machines are all set up: it sits under
 * a list of green machines that are serving players and tells them to do
 * something they have already done. Their file is the per-machine one, which
 * does contain the Link.
 *
 * `keysLoaded` is part of the answer rather than a caller's job: without it the
 * warning appears for one render while the keys are still arriving, which is the
 * moment it is least deserved and most visible.
 *
 * A function, and tested, because it is a VISIBILITY rule over three states and
 * the panel has no render-level tests to catch it being inverted.
 */
export function genericNodeKitApplies(
    keysLoaded: boolean,
    machines: { boundKey: boolean }[],
): boolean {
    if (!keysLoaded) return true;
    if (machines.length === 0) return true;
    return !machines.every(m => m.boundKey);
}

/** The compose file's name on disk, and the name every command refers to. */
export function composeFileName(kind: 'route-only' | 'node'): string {
    return kind === 'route-only' ? 'route-only.yml' : 'byon-node.yml';
}

/**
 * The sentence above the command block: how the file gets onto the machine.
 *
 * Prose rather than a numbered shell step, because creating a file is not a
 * command - and on Windows it does not even happen in a terminal. It was the
 * first line of the command block, where the one instruction nobody can copy
 * sat among four they can.
 *
 * Windows gets the extra half sentence because Notepad's default "Text
 * Documents" filter silently produces route-only.yml.txt, and Docker then
 * reports a missing file rather than a misnamed one.
 */
export function deployIntro(kind: 'route-only' | 'node', platform: DeployPlatform = 'linux'): string {
    const file = composeFileName(kind);
    return platform === 'windows'
        ? `Save the file above as ${file}. In the Save-as dialog set "Save as type" to All files, or Windows appends .txt to it. Open PowerShell in that folder, then run:`
        : `Save the file above as ${file}, open a terminal in that folder, then run:`;
}

/** For readers who do not want a terminal at all. */
export const DEPLOY_PORTAINER_NOTE =
    'Using Portainer instead? Stacks, Add stack, Web editor, paste the same file, Deploy. Nothing in it changes.';

/**
 * What to run once the file is on the machine, and what to check afterwards.
 * externalNode is WarpDeployInput.externalNode: the admin's External node kit,
 * which appears somewhere else in the panel than a tenant's machine.
 */
export function deployCli(kind: 'route-only' | 'node', externalNode = false): string {
    const file = composeFileName(kind);
    if (kind === 'route-only') {
        return `# 1. Start it. Pull first, so a cached image is not what runs.
docker compose -f ${file} pull
docker compose -f ${file} up -d

# 2. Watch the link come up: it gets its token from us, then opens a tunnel
#    to every edge ("Secure Tunnel established ... over the internet").
docker compose -f ${file} logs -f link

# 3. Create the route(s) in the panel.
#
# Updating later is the same pull + up. With players online the old link lets
# them finish and takes nobody new meanwhile, so "up" can take up to
# LINK_DRAIN_TIMEOUT. Update when the server is quiet.`;
    }
    return `# 1. Start it. Pull first: the tunnel agent is what supplies the internal
#    addresses, so a stale cached image would leave the rest of the stack
#    with nothing to talk to.
docker compose -f ${file} pull
docker compose -f ${file} up -d

# 2. Watch the tunnel come up (peer + handshake within ~15s):
docker compose -f ${file} logs -f warp

# 3. Verify the overlay actually carries traffic, not just that wg0 exists:
docker compose -f ${file} exec warp wg show

# 4. The node registers itself; it appears in the panel under ${externalNode
        ? 'My infrastructure -> External nodes'
        : 'Nodes'} within ~30s.
docker compose -f ${file} logs -f node`;
}

/**
 * A node running with host networking binds these on the customer's machine.
 * NODE_EXTERNAL is an advertised mode, not enforcement - the SFTP listener in
 * particular starts regardless - so the operator has to be told rather than
 * assume they are closed.
 */
export const EXTERNAL_NODE_PORTS = [
    { port: 25520, what: 'SFTP', note: 'starts even with NODE_EXTERNAL=true' },
    { port: 25521, what: 'Beam gRPC', note: 'every interface; needs a ticket Dylaris issues' },
    { port: 25522, what: 'Migration pull', note: 'auto-move transport' },
    { port: 25523, what: 'Beam LAN fast-path', note: 'set BEAM_LAN_FASTPATH=false to drop it' },
    { port: 25570, what: 'Overlay proxy (Core)', note: 'bound by warp on loopback + your Docker bridges, never the LAN' },
    { port: 25571, what: 'Overlay proxy (Redis)', note: 'same; this is how containers reach the overlay' },
];
