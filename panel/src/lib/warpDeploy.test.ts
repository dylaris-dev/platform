import { describe, it, expect } from 'vitest';
import {
    routeOnlyCompose, nodeCompose, deployCli, deployIntro, composeFileName,
    nodeIdFromLabel, defaultLocalTarget, EXTERNAL_NODE_PORTS, kitGrpcTlsFingerprint,
} from './warpDeploy';

const base = { apiKey: 'KEY123', enrollUrl: 'https://api.example.com' };

describe('routeOnlyCompose', () => {
    it('embeds the key and enroll url', () => {
        const out = routeOnlyCompose(base);
        expect(out).toContain('API_KEY: "KEY123"');
        expect(out).toContain('ENROLL_URL: "https://api.example.com"');
    });

    // The link is what makes this route-only rather than plain overlay access.
    it('includes the link container', () => {
        const out = routeOnlyCompose(base);
        expect(out).toContain('ghcr.io/dylaris-dev/gateway-link:latest');
        expect(out).toContain('depends_on: [warp]');
    });

    // One key does both jobs: the link exchanges it for its own derived token,
    // so no second secret is handed out.
    it('reuses the warp key as LINK_BOOT_KEY', () => {
        expect(routeOnlyCompose(base)).toContain('LINK_BOOT_KEY: "KEY123"');
    });

    // Against a plain-TCP Redis a TLS client fails the handshake and the link
    // never registers - the exact trap the repo example still carries. With the
    // local proxy it is doubly required: a TLS client would verify the
    // certificate against 127.0.0.1. The path is inside WireGuard either way.
    it('defaults REDIS_USE_TLS to false', () => {
        expect(routeOnlyCompose(base)).toContain('REDIS_USE_TLS: "false"');
    });

    // Host networking means anything the link binds lands on the customer's
    // machine, and /health has no auth. Nothing reads it in this mode.
    it('keeps the link management server off the LAN', () => {
        expect(routeOnlyCompose(base)).toContain('LINK_PORT: "127.0.0.1:25540"');
    });

    // Route-only runs the link with host networking, so warp's loopback
    // listener is already in its namespace and no bridge binding is needed.
    // The link defaults to that listener on its own, so the file no longer
    // names an address a reader could get wrong.
    it('leaves the proxy address to the link and binds no bridges', () => {
        const out = routeOnlyCompose(base);
        expect(out).not.toContain('REDIS_ADDR: "');
        expect(out).not.toContain('PROXY_BIND_DOCKER_BRIDGES');
    });

    // The link caches what it last got from Core under /data. Without a named
    // volume that cache dies with the container, and a recreate while Core is
    // unreachable does not come up.
    it('keeps the link cache across a recreate', () => {
        const out = routeOnlyCompose(base);
        expect(out).toContain('- link_data:/data');
        expect(out).toMatch(/\nvolumes:\n {2}link_data:\n/);
    });

    // LINK_ALLOWED_TARGETS is compared as an exact host string; a port never matches.
    it('uses a bare host for the local target', () => {
        const out = routeOnlyCompose({ ...base, localTarget: '192.168.1.50' });
        expect(out).toContain('LINK_ALLOWED_TARGETS: "192.168.1.50"');
        expect(out).not.toContain('192.168.1.50:');
        expect(routeOnlyCompose(base)).toContain('LINK_ALLOWED_TARGETS: "127.0.0.1"');
    });

    it('leaves an obvious placeholder when the overlay CIDR is unknown', () => {
        expect(routeOnlyCompose(base)).toContain('<overlay-cidr');
        expect(routeOnlyCompose({ ...base, tunnelSubnets: '10.20.0.0/16' }))
            .toContain('TUNNEL_SUBNETS: "10.20.0.0/16"');
    });

    it('treats a whitespace-only value as unset rather than emitting an empty string', () => {
        expect(routeOnlyCompose({ ...base, tunnelSubnets: '   ' })).toContain('<overlay-cidr');
    });
});

describe('nodeCompose', () => {
    it('never emits a CLUSTER_SECRET — a customer machine must not hold fleet credentials', () => {
        expect(nodeCompose(base)).not.toContain('CLUSTER_SECRET:');
    });

    it('starts warp before the node, since the node reaches Redis over the overlay', () => {
        const out = nodeCompose(base);
        expect(out.indexOf('warp:')).toBeLessThan(out.indexOf('node:'));
        expect(out).toContain('depends_on: [warp]');
    });

    // Both are image defaults now (an external node manages its own Link with
    // the built-in image), so carrying them here is noise the reader has to
    // evaluate. Keep them out - if they come back, the reason has to be new.
    it('omits settings that equal the image default', () => {
        const out = nodeCompose(base);
        expect(out).not.toContain('NODE_MANAGES_LINK');
        expect(out).not.toContain('LINK_IMAGE');
        expect(out).not.toContain('LEADER:');
    });

    // Named volumes live where Docker decides; someone running servers on their
    // own box wants to know where the files are.
    it('shows how to bind the data directory to a real path', () => {
        const out = nodeCompose(base);
        expect(out).toContain('/app/dylaris_data');
        expect(out).toContain('/srv/dylaris:/app/dylaris_data');
        expect(out).toContain('C:\\dylaris:/app/dylaris_data');
    });

    it('fills in every operator value when known', () => {
        const out = nodeCompose({
            ...base,
            tunnelSubnets: '10.20.0.0/16',
            nodeEnrollToken: 'TOK',
            nodeId: 'home-desktop',
            grpcTlsFingerprint: '',
        });
        expect(out).toContain('NODE_ENROLL_TOKEN: "TOK"');
        expect(out).toContain('NODE_ID: "home-desktop"');
        expect(out).not.toContain('<');
    });

    // The whole point of the local proxy: the file carries no overlay address,
    // so a platform that moves does not send every customer back to the panel.
    it('carries no overlay address at all', () => {
        const out = nodeCompose({ ...base, tunnelSubnets: '10.20.0.0/16', nodeEnrollToken: 'TOK', nodeId: 'n' });
        // The names still appear in the comment that explains their absence,
        // so assert on the assignment, which is what a reader would have to edit.
        expect(out).not.toContain('CORE_GRPC_ADDR: "');
        expect(out).not.toContain('REDIS_ADDR: "');
    });

    // The link sidecar and every MC server run on a Docker bridge, where
    // 127.0.0.1 is their own loopback and not the host warp listens on.
    it('lets warp serve the proxy to containers too', () => {
        expect(nodeCompose(base)).toContain('PROXY_BIND_DOCKER_BRIDGES: "true"');
    });

    // Core returns a fingerprint only while its gRPC channel is TLS, so its
    // presence is the signal. Emitting the pin against a plaintext Core would
    // make every BYON node fail its handshake instead of connecting.
    it('pins the Core gRPC certificate when there is a fingerprint', () => {
        const withPin = nodeCompose({ ...base, grpcTlsFingerprint: 'ab12cd34' });
        expect(withPin).toContain('GRPC_TLS_ENABLED: "true"');
        expect(withPin).toContain('GRPC_TLS_FINGERPRINT: "ab12cd34"');
    });

    // The node defaults GRPC_TLS_ENABLED to true, and a BYON machine holds no
    // CLUSTER_SECRET to derive a pin from - so a snippet that simply omits the
    // line hands the customer a container that exits at boot with "no
    // certificate pin available". Silence stopped being a safe way to say
    // "plaintext" the moment the default flipped; the opt-out has to be written.
    it('says plaintext out loud rather than leaning on the old default', () => {
        const without = nodeCompose({ ...base, grpcTlsFingerprint: '' });
        expect(without).toContain('GRPC_TLS_ENABLED: "false"');
        expect(without).not.toContain('GRPC_TLS_FINGERPRINT');
    });

    // A machine that runs our node also runs warp's proxy, and the operator is
    // told what binds rather than left to find two unexplained ports.
    it('lists the proxy ports among what the machine binds', () => {
        const ports = EXTERNAL_NODE_PORTS.map(p => p.port);
        expect(ports).toContain(25570);
        expect(ports).toContain(25571);
    });
});

describe('nodeIdFromLabel', () => {
    // NODE_ID lands in Redis keys, the mesh identity and every container's
    // environment, so the free-text location name cannot go in verbatim.
    it('reduces a typed location name to a safe id', () => {
        expect(nodeIdFromLabel('My Home PC')).toBe('my-home-pc');
        expect(nodeIdFromLabel('  Rack #3 / EU  ')).toBe('rack-3-eu');
        expect(nodeIdFromLabel('home-desktop')).toBe('home-desktop');
    });

    // undefined leaves the snippet's placeholder in place; an empty string
    // would render as NODE_ID: "" and read like a deliberate setting.
    it('returns undefined when nothing usable is left', () => {
        expect(nodeIdFromLabel('')).toBeUndefined();
        expect(nodeIdFromLabel('   ')).toBeUndefined();
        expect(nodeIdFromLabel('###')).toBeUndefined();
        expect(nodeIdFromLabel(undefined)).toBeUndefined();
    });

    // The slice that caps the length must not leave a trailing separator.
    it('never ends in a separator after truncation', () => {
        const out = nodeIdFromLabel('a'.repeat(39) + ' tail');
        expect(out).not.toMatch(/-$/);
    });
});

describe('deployCli', () => {
    it('names the matching compose file', () => {
        expect(deployCli('route-only')).toContain('route-only.yml');
        expect(deployCli('node')).toContain('byon-node.yml');
    });

    it('tails the container that matters for each variant', () => {
        expect(deployCli('node')).toContain('logs -f node');
        expect(deployCli('route-only')).not.toContain('logs -f node');
        expect(deployCli('route-only')).toContain('logs -f link');
    });

    // Creating the file is not a command, and on Windows not even a terminal
    // step. It used to be step 1 of the block, where the one line nobody can
    // copy sat among four they can.
    it('leaves creating the file out of the command block', () => {
        const cli = deployCli('route-only');
        expect(cli).not.toContain('nano');
        expect(cli).not.toContain('notepad');
        expect(cli).toMatch(/^# 1\. Start it/);
        expect(cli).toContain('docker compose -f route-only.yml pull');
        expect(cli).toContain('docker compose -f route-only.yml up -d');
    });
});

describe('deployIntro', () => {
    it('names the file the commands then refer to', () => {
        expect(deployIntro('route-only')).toContain('route-only.yml');
        expect(deployIntro('node')).toContain('byon-node.yml');
        expect(deployIntro('route-only')).toBe(deployIntro('route-only', 'linux'));
    });

    // Notepad's default filter produces route-only.yml.txt, and Docker then
    // reports a missing file rather than a misnamed one.
    it('warns Windows readers about the appended .txt', () => {
        expect(deployIntro('route-only', 'windows')).toContain('.txt');
        expect(deployIntro('route-only', 'linux')).not.toContain('.txt');
    });
});

// Every env line carries one of two markers, because the reader's real
// question about each of them is "may I touch this". A line with neither is a
// line they have to guess about.
// The edge's private address is reachable from a customer machine too, because
// warp routes the overlay - so the link's default preference succeeds and puts
// every player's bytes through the tunnel. Only the kit knows which side of the
// network this compose file lands on.
describe('route-only is marked as an outside machine', () => {
    it('sets LINK_EXTERNAL on the link', () => {
        expect(routeOnlyCompose(base)).toContain('LINK_EXTERNAL: "true"');
    });

    // A managed node sets it for its own sidecar from NODE_EXTERNAL, so the
    // node kit must NOT also carry it - two sources for one setting is how they
    // end up disagreeing.
    it('leaves it out of the node kit, which sets it itself', () => {
        expect(nodeCompose(base)).not.toContain('LINK_EXTERNAL');
    });
});

describe('compose annotations', () => {
    const envLine = /^ {6}[A-Z][A-Z0-9_]*:/;

    const annotated = (body: string) => {
        const lines = body.split('\n');
        return lines.every((line, idx) => {
            if (!envLine.test(line)) return true;
            // Walk up past sibling env lines (LOCAL_HOST under
            // LINK_ALLOWED_TARGETS shares one marker) and past the wrapped
            // tail of a comment, to the line the comment block STARTS on -
            // that is where the marker sits.
            let first = -1;
            for (let i = idx - 1; i >= 0; i--) {
                const prev = lines[i];
                if (envLine.test(prev)) continue;
                if (/^ *#/.test(prev)) { first = i; continue; }
                break;
            }
            return first >= 0 && /^ *# (keep|EDIT)/.test(lines[first]);
        });
    };

    it('marks every env as keep or EDIT', () => {
        expect(annotated(routeOnlyCompose(base))).toBe(true);
        expect(annotated(nodeCompose(base))).toBe(true);
        expect(annotated(nodeCompose({ ...base, linkBesideNode: true }))).toBe(true);
    });

    it('says up front what the two markers mean', () => {
        for (const body of [routeOnlyCompose(base), nodeCompose(base), nodeCompose({ ...base, linkBesideNode: true })]) {
            expect(body).toContain('Lines marked "keep"');
            expect(body).toContain('EDIT is yours to change');
        }
    });

    // The header names the file the reader must save it as, and the commands
    // then dial that exact name.
    it('agrees with the file name the commands use', () => {
        expect(routeOnlyCompose(base).startsWith('# route-only.yml')).toBe(true);
        expect(nodeCompose(base).startsWith('# byon-node.yml')).toBe(true);
        expect(composeFileName('route-only')).toBe('route-only.yml');
        expect(composeFileName('node')).toBe('byon-node.yml');
    });
});

describe('EXTERNAL_NODE_PORTS', () => {
    // NODE_EXTERNAL is an advertised mode, not enforcement: SFTP binds anyway.
    // Users need to be told, so this list must keep saying so.
    it('warns that SFTP binds despite NODE_EXTERNAL', () => {
        const sftp = EXTERNAL_NODE_PORTS.find(p => p.port === 25520);
        expect(sftp?.note).toContain('NODE_EXTERNAL');
    });

    // The note tells the operator to set an env var. A var that is absent from
    // the compose file's environment: block cannot be set from .env at all, so
    // the advice only works while the snippet actually writes the line out.
    it('only names an env var the generated compose forwards', () => {
        const fastpath = EXTERNAL_NODE_PORTS.find(p => p.port === 25523);
        expect(fastpath?.note).toContain('BEAM_LAN_FASTPATH=false');
        expect(nodeCompose(base)).toContain('BEAM_LAN_FASTPATH: "true"');
    });
});

// Docker Desktop's `network_mode: host` joins the WSL2 VM, not Windows. A
// snippet that kept 127.0.0.1 there would point the link at the VM's loopback
// and never reach the server the customer actually runs - measured: a
// host-networked port is not reachable from Windows, host.docker.internal is.
describe('routeOnlyCompose on Docker Desktop', () => {
    it('targets host.docker.internal instead of loopback', () => {
        const out = routeOnlyCompose({ ...base, platform: 'windows' });
        expect(out).toContain('LOCAL_HOST: "host.docker.internal"');
        expect(out).toContain('LINK_ALLOWED_TARGETS: "host.docker.internal"');
        expect(out).not.toContain('LOCAL_HOST: "127.0.0.1"');
    });

    it('says why, so the reader is not left guessing', () => {
        const out = routeOnlyCompose({ ...base, platform: 'windows' });
        expect(out).toContain('WSL2 VM');
        expect(out).not.toContain('Linux only');
    });

    // warp and the link share the VM's namespace, so they still find each other
    // on loopback - only the customer's own server sits outside it.
    it('keeps warp\'s local proxy on loopback', () => {
        const out = routeOnlyCompose({ ...base, platform: 'windows' });
        expect(out).not.toContain('REDIS_ADDR: "');
        expect(out).toContain('127.0.0.1:25571');
        expect(out).toContain('LINK_PORT: "127.0.0.1:25540"');
    });

    it('still lets an explicit target win', () => {
        const out = routeOnlyCompose({ ...base, platform: 'windows', localTarget: '192.168.1.50' });
        expect(out).toContain('LOCAL_HOST: "192.168.1.50"');
    });

    it('defaults to the linux target when no platform is given', () => {
        expect(defaultLocalTarget(undefined)).toBe('127.0.0.1');
        expect(defaultLocalTarget('linux')).toBe('127.0.0.1');
        expect(defaultLocalTarget('windows')).toBe('host.docker.internal');
        expect(routeOnlyCompose(base)).toContain('LOCAL_HOST: "127.0.0.1"');
    });
});

// The BYON kit runs the Link beside the node instead of the node starting one
// inside itself. Core answers the Link through the same warp key, so a customer
// machine still holds no CLUSTER_SECRET and no second secret.
describe('nodeCompose with the Link beside the node', () => {
    const kit = (over: Partial<Parameters<typeof nodeCompose>[0]> = {}) =>
        nodeCompose({ ...base, linkBesideNode: true, ...over });
    const service = (out: string, name: string) => {
        const start = out.indexOf(`\n  ${name}:\n`);
        expect(start).toBeGreaterThan(-1);
        const rest = out.slice(start + 1);
        const end = rest.search(/\n(?: {2}[a-z_]+:\n|[a-z]+:\n)/);
        return end === -1 ? rest : rest.slice(0, end + 1);
    };

    it('adds a link service that boots on the warp key', () => {
        const link = service(kit(), 'link');
        expect(link).toContain('image: ghcr.io/dylaris-dev/gateway-link:latest');
        expect(link).toContain('restart: unless-stopped');
        expect(link).toContain('CORE_URL: "https://api.example.com"');
        expect(link).toContain('LINK_BOOT_KEY: "KEY123"');
        expect(link).toContain('LINK_EXTERNAL: "true"');
    });

    // Exactly one Link per machine: two would fight over the same identity.
    it('tells the node not to start its own', () => {
        expect(service(kit(), 'node')).toContain('NODE_MANAGES_LINK: "false"');
        expect(kit()).not.toContain('do not run link yourself');
    });

    // Inside a Docker network 127.0.0.1 is the link's own container. It reaches
    // warp's proxy on the host through host-gateway, and Core tells it the
    // address - so a wrong loopback line is not there to get wrong.
    it('reaches the host through host-gateway, not loopback', () => {
        const link = service(kit(), 'link');
        expect(link).toContain('extra_hosts: ["host.docker.internal:host-gateway"]');
        expect(link).not.toContain('REDIS_ADDR');
        expect(link).not.toContain('network_mode: host');
    });

    // The node puts servers on the network named dylaris_net; a folder-prefixed
    // name would be a second network beside the one servers already run on.
    it('joins the servers\' network by its exact name, with no subnet pinned', () => {
        const out = kit();
        expect(service(out, 'link')).toContain('networks: [dylaris_net]');
        expect(out).toMatch(/\nnetworks:\n(?: {2}#.*\n)* {2}dylaris_net:\n {4}name: dylaris_net\n/);
        expect(out).not.toMatch(/^\s*(?:- )?subnet:/m);
        expect(out).not.toMatch(/^\s*ipam:/m);
    });

    it('keeps the link cache in a named volume', () => {
        const out = kit();
        expect(service(out, 'link')).toContain('- link_data:/data');
        expect(out).toMatch(/\nvolumes:\n {2}byon_data:\n {2}link_data:\n/);
    });

    // Everything the node kit promised before still holds with the link in it.
    it('keeps every node setting and adds no fleet secret or overlay address', () => {
        const out = kit({ tunnelSubnets: '10.20.0.0/16', nodeEnrollToken: 'TOK', nodeId: 'home-desktop', grpcTlsFingerprint: '' });
        for (const line of [
            'API_KEY: "KEY123"', 'PROXY_BIND_DOCKER_BRIDGES: "true"', 'NODE_EXTERNAL: "true"',
            'NODE_ID: "home-desktop"', 'NODE_ENROLL_TOKEN: "TOK"', 'BEAM_LAN_FASTPATH: "true"',
            'GRPC_TLS_ENABLED: "false"', '- byon_data:/app/dylaris_data',
        ]) {
            expect(out).toContain(line);
        }
        expect(out).not.toContain('CLUSTER_SECRET:');
        expect(out).not.toContain('CORE_GRPC_ADDR: "');
        expect(out).not.toContain('REDIS_ADDR: "');
        expect(out).not.toContain('<');
    });

    // host-gateway on Docker Desktop is Windows, not the VM warp listens in, so
    // that platform keeps the node-managed Link until the path is proven.
    it('leaves Docker Desktop on the node-managed Link', () => {
        const win = kit({ platform: 'windows' });
        expect(win).toBe(nodeCompose({ ...base, platform: 'windows' }));
        expect(win).not.toContain('NODE_MANAGES_LINK');
    });

    // Without the flag the file is exactly what it was: an operator key has no
    // node Core could answer it with, so its kit must keep the node's own Link.
    it('is opt-in', () => {
        expect(nodeCompose(base)).not.toContain('gateway-link');
        expect(nodeCompose(base)).not.toContain('NODE_MANAGES_LINK');
        expect(nodeCompose(base)).not.toContain('networks:');
    });
});

// "Update this machine", "Deploy file" and a rolled key show a kit without a
// fresh enroll token. The fingerprint rode only on the token, so those kits
// wrote GRPC_TLS_ENABLED "false" against a TLS Core, and a node redeployed from
// one dialled plaintext and lost its control channel. They read it from Core's
// deploy config now, through the same helper the operator's dialog uses.
describe('the TLS lines of a kit without a fresh enroll token', () => {
    type Config = { tunnelSubnets: string; grpcTlsFingerprint?: string } | null;
    const kitFor = (config: Config, explicit?: string) =>
        nodeCompose({ ...base, linkBesideNode: true, grpcTlsFingerprint: kitGrpcTlsFingerprint(explicit, config) });

    it('enables TLS and pins the fingerprint when Core runs TLS', () => {
        const out = kitFor({ tunnelSubnets: '', grpcTlsFingerprint: 'ab12cd34' });
        expect(out).toContain('GRPC_TLS_ENABLED: "true"');
        expect(out).toContain('GRPC_TLS_FINGERPRINT: "ab12cd34"');
        expect(out).not.toContain('GRPC_TLS_ENABLED: "false"');
    });

    it('says plaintext when Core says its channel is plaintext', () => {
        const out = kitFor({ tunnelSubnets: '', grpcTlsFingerprint: '' });
        expect(out).toContain('GRPC_TLS_ENABLED: "false"');
        expect(out).not.toContain('GRPC_TLS_FINGERPRINT');
    });

    // Not loaded yet, failed to load, or a Core older than the field: a "false"
    // here would be the same broken node against a TLS Core.
    it('never writes plaintext while it does not know', () => {
        for (const config of [null, { tunnelSubnets: '' }] as Config[]) {
            const out = kitFor(config);
            expect(out).not.toContain('GRPC_TLS_ENABLED: "false"');
            expect(out).not.toContain('GRPC_TLS_ENABLED: "true"');
            expect(out).toContain('GRPC_TLS_ENABLED: "<reload this page>"');
        }
    });

    it('lets the fingerprint handed over with an enroll token win', () => {
        const config = { tunnelSubnets: '', grpcTlsFingerprint: 'from-config' };
        expect(kitGrpcTlsFingerprint('from-mint', config)).toBe('from-mint');
        expect(kitGrpcTlsFingerprint('', config)).toBe('');
        expect(kitGrpcTlsFingerprint(undefined, config)).toBe('from-config');
    });
});

// The node kit was Linux-only on this screen for longer than it had to be. On
// Docker Desktop the socket, the tunnel and the Minecraft containers all sit in
// the same WSL2 VM, so they reach each other as they would on Linux - what
// genuinely differs is where the files land, and the snippet has to say so.
describe('nodeCompose on Windows', () => {

    it('names the WSL2 VM instead of claiming host networking is Windows', () => {
        const got = nodeCompose({ ...base, platform: 'windows' });
        expect(got).toContain('WSL2 VM');
        expect(got).not.toContain('Kernel WireGuard needs host networking');
    });

    it('leaves the Linux snippet as it was', () => {
        const got = nodeCompose({ ...base, platform: 'linux' });
        expect(got).toContain('Kernel WireGuard needs host networking');
        expect(got).not.toContain('WSL2 VM');
    });

    // The two platforms must not silently produce the same file: that is what a
    // toggle doing nothing looks like from the outside.
    it('actually differs between the two', () => {
        expect(nodeCompose({ ...base, platform: 'windows' }))
            .not.toBe(nodeCompose({ ...base, platform: 'linux' }));
    });
});

// Every image this file emits, spelled out. The organisation move renamed all
// of them (ghcr.io/bartis-dev/dylaris-<x> -> ghcr.io/dylaris-dev/<x>) and the
// templates kept the old middle segment, so the snippet handed to every BYON
// and route-only customer named three images that do not exist. Nothing failed:
// the one image assertion in this file was an incidental substring that matched
// the WRONG name, so it stayed green precisely because the bug was there.
describe('emitted image paths', () => {
    it('route-only names warp and link at the current registry path', () => {
        const out = routeOnlyCompose(base);
        expect(out).toContain('image: ghcr.io/dylaris-dev/gateway-warp:latest');
        expect(out).toContain('image: ghcr.io/dylaris-dev/gateway-link:latest');
    });

    it('node names warp and the node agent at the current registry path', () => {
        const out = nodeCompose(base);
        expect(out).toContain('image: ghcr.io/dylaris-dev/gateway-warp:latest');
        expect(out).toContain('image: ghcr.io/dylaris-dev/platform-node:latest');
        expect(nodeCompose({ ...base, linkBesideNode: true }))
            .toContain('image: ghcr.io/dylaris-dev/gateway-link:latest');
    });

    // The old owner must not survive anywhere in a file a customer runs, and
    // neither must the doubled segment the move left behind.
    it('emits no legacy registry path', () => {
        for (const out of [routeOnlyCompose(base), nodeCompose(base), nodeCompose({ ...base, linkBesideNode: true })]) {
            expect(out).not.toContain('bartis-dev');
            expect(out).not.toContain('dylaris-dev/dylaris-');
        }
    });
});

// LINK_DRAIN_TIMEOUT has no default and the link REFUSES TO START without it.
// A kit that omits it is a file a customer deploys and a link that never comes
// up, with every route pointing at it dead - and the kit is the only place most
// customers ever get that line from.
describe('every kit that ships a link sets its drain window', () => {
    const withLink: Array<[string, string]> = [
        ['route-only', routeOnlyCompose(base)],
        ['node with the link beside it', nodeCompose({ ...base, linkBesideNode: true })],
    ];

    it.each(withLink)('%s sets LINK_DRAIN_TIMEOUT', (_name, out) => {
        expect(out).toContain('LINK_DRAIN_TIMEOUT: "6h"');
    });

    // Without a stop timeout of its own the container is killed after ten
    // seconds, and the drain is decoration: the players it was keeping drop
    // exactly as they did before it existed.
    it.each(withLink)('%s gives the link longer to drain than the drain itself', (_name, out) => {
        expect(out).toContain('stop_grace_period: 6h10m');
    });

    // A node kit WITHOUT its own link runs no link at all, so the line would be
    // an instruction about a container that is not there.
    it('a node kit without a link says nothing about draining', () => {
        const out = nodeCompose(base);
        expect(out).not.toContain('LINK_DRAIN_TIMEOUT');
        expect(out).not.toContain('stop_grace_period');
    });
});
