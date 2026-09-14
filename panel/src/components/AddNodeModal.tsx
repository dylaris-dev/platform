"use client";

import { useState } from 'react';
import { X, Server, Network, Copy, Check, ExternalLink, Info } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { coreOrigin } from '@/lib/api/core';
import { isWails } from '@/lib/adapters';

// ---------------------------------------------------------------------------
// "How do I add a node?" — the admin answer.
//
// The sidebar's "+ Create -> Add a node" used to navigate an admin to a
// settings tab and leave them there. Adding a node is a HOST-side operation:
// nothing in the panel performs it, the panel only shows the result. So the
// honest response to the button is the instructions, not a screen.
//
// Two shapes, and which ones exist depends on the install:
//   - Fleet node: a machine the operator owns, joined to the Swarm/compose
//     stack. Always available.
//   - External node: a machine the platform runs outside the datacenter,
//     reached over the warp overlay. Added under Settings -> Warp, which mints
//     its key and enroll token together; the node belongs to nobody. Only
//     meaningful once the gateway subsystem is routing, so that tab appears only
//     then. A customer's machine is not added here at all: the customer adds it
//     under My infrastructure, and an admin token would make it the admin's.
// ---------------------------------------------------------------------------

const enrollUrl = coreOrigin();

const FLEET_COMPOSE = `# Add this service to the stack you deploy on the new host.
# It joins the SAME overlay network as core and redis, so nothing is published.
services:
  node:
    image: ghcr.io/dylaris-dev/platform-node:latest
    restart: unless-stopped
    environment:
      NODE_ID: "<stable-id-for-this-machine>"
      # The same value Core runs with. A fleet host is trusted infrastructure:
      # it pairs by proving this secret, so no enroll token is involved.
      CLUSTER_SECRET: "\${CLUSTER_SECRET}"
      CORE_GRPC_ADDR: "core:25501"
      # No REDIS_ADDR: Core tells the node its own address on every login, and
      # the node fetches a Redis credential scoped to itself.
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - /dev:/dev:ro
      - node_data:/app/dylaris_data
    networks: [dylaris_net]
    cap_add: [SYS_ADMIN]

volumes:
  node_data:
networks:
  dylaris_net:
    external: true`;

const FLEET_STEPS = `# 1. Deploy the stack on the new host (Swarm):
docker stack deploy -c docker-stack.yml dylaris

#    ...or with compose on a single host:
docker compose up -d node

# 2. Watch it pair. It proves CLUSTER_SECRET and Core gives it its identity.
docker compose logs -f node

# 3. It appears under Settings -> Nodes within ~30s.`;

function CodeBlock({ code, label }: { code: string; label: string }) {
    const [copied, setCopied] = useState(false);
    return (
        <div>
            <div className="flex items-center justify-between mb-1.5">
                <span className="mono-label">{label}</span>
                <button
                    type="button"
                    onClick={async () => {
                        try {
                            await navigator.clipboard.writeText(code);
                            setCopied(true);
                            setTimeout(() => setCopied(false), 1800);
                        } catch { /* clipboard blocked; the text is selectable */ }
                    }}
                    className="btn btn-secondary btn-sm"
                >
                    {copied ? <><Check size={12} /> Copied</> : <><Copy size={12} /> Copy</>}
                </button>
            </div>
            <pre className="input-mono text-[11px] leading-relaxed bg-(--base-02) border border-(--base-03) rounded-md p-3 overflow-x-auto text-(--base-08) whitespace-pre">
                {code}
            </pre>
        </div>
    );
}

type NodeKind = 'fleet' | 'external';

export default function AddNodeModal({ onClose }: { onClose: () => void }) {
    const { gatewayEnabled } = useAppData();
    const [tab, setTab] = useState<NodeKind>('fleet');

    // An external node reaches Core over the warp overlay, which is part of the
    // gateway subsystem. With routing on ip_port there is no overlay deployed,
    // so offering the tab would document a path that does not exist here.
    const showExternal = gatewayEnabled;
    const active: NodeKind = showExternal ? tab : 'fleet';

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <div className="modal-panel w-full max-w-3xl flex flex-col max-h-[88vh]" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-start justify-between gap-4">
                    <div>
                        <h3 className="modal-title flex items-center gap-2">
                            <Server size={16} className="text-(--accent-light)" />
                            Add a node
                        </h3>
                        <p className="text-xs text-(--base-07) mt-1">
                            A node is added on the machine itself; the panel shows the result.
                        </p>
                    </div>
                    <button onClick={onClose} className="p-1 text-(--base-06) hover:text-(--base-09) transition-colors">
                        <X size={18} />
                    </button>
                </div>

                {isWails() && (
                    <div className="px-6 pt-4 shrink-0">
                        {/* Only inside Beam. A browser cannot know what is
                            installed on the machine reading this page, so
                            everywhere else the honest answer is the instructions
                            below - which is what this modal already is. */}
                        <button
                            type="button"
                            onClick={() => { window.location.href = '/__beam/#deploy'; }}
                            className="btn btn-primary btn-sm w-full justify-center"
                        >
                            <Server size={13} /> Run a node on THIS machine
                        </button>
                        <p className="text-xs text-(--base-06) mt-1.5">
                            Beam checks Docker here and writes the compose file for you. The steps
                            below stay available for any other machine.
                        </p>
                    </div>
                )}

                {showExternal && (
                    <div className="px-6 pt-4 shrink-0">
                        <div className="flex gap-1 border-b border-(--base-03)">
                            {([
                                { id: 'fleet' as const, label: 'Fleet node', Icon: Server },
                                { id: 'external' as const, label: 'External node', Icon: Network },
                            ]).map(({ id, label, Icon }) => (
                                <button
                                    key={id}
                                    type="button"
                                    onClick={() => setTab(id)}
                                    className={`flex items-center gap-1.5 px-3 py-2 text-xs font-medium border-b-2 transition-colors ${
                                        active === id
                                            ? 'border-(--accent) text-(--accent-light)'
                                            : 'border-transparent text-(--base-07) hover:text-(--base-09) hover:border-(--base-04)'
                                    }`}
                                >
                                    <Icon size={12} />
                                    {label}
                                </button>
                            ))}
                        </div>
                    </div>
                )}

                <div className="modal-body flex-1 overflow-y-auto space-y-4">
                    {active === 'fleet' ? (
                        <>
                            <p className="text-sm text-(--base-07)">
                                A machine you own, on the same Docker network as Core and Redis. It needs no public
                                port and no overlay: it dials Core over the internal network.
                            </p>
                            <div className="alert alert-info text-xs">
                                <Info size={13} className="shrink-0 mt-0.5" />
                                <span>
                                    A fleet host joins with <code>CLUSTER_SECRET</code>, the same value Core runs
                                    with: it proves the secret (a cluster proof) and Core pairs it, with no enroll
                                    token. Do not mint one for it - an enroll token makes the machine the minting
                                    account&apos;s own node. A host that cannot pair appears under{' '}
                                    <a href="/settings/nodes" className="text-(--accent-light) hover:underline">
                                        Settings &rarr; Nodes
                                    </a>
                                    {' '}as a connection attempt to admit.
                                </span>
                            </div>
                            <CodeBlock label="Stack service" code={FLEET_COMPOSE} />
                            <CodeBlock label="Deploy" code={FLEET_STEPS} />
                            <p className="text-xs text-(--base-06)">
                                Core is reachable at <code className="text-(--base-08)">{enrollUrl}</code>. If the new
                                host is not on the same overlay network, it is an External node - see the other tab.
                            </p>
                        </>
                    ) : (
                        <>
                            <p className="text-sm text-(--base-07)">
                                A machine the platform runs outside the datacenter. It joins the warp overlay and dials
                                Core through the tunnel, so it needs no public IP and no port forwarding, and it never
                                holds <code>CLUSTER_SECRET</code>. The node belongs to nobody, not to the admin who adds it.
                            </p>
                            <div className="alert alert-info text-xs">
                                <Info size={13} className="shrink-0 mt-0.5" />
                                <span>
                                    The compose file - warp, the node and its Link, with the key and the enroll token
                                    filled in - is generated under{' '}
                                    <a href="/settings/warp" className="text-(--accent-light) hover:underline inline-flex items-center gap-1">
                                        Settings &rarr; Warp &rarr; External Nodes <ExternalLink size={10} />
                                    </a>
                                    . It cannot be shown here because both are revealed exactly once, when they are minted.
                                    A customer&apos;s machine is not added here: the customer adds it under My infrastructure.
                                </span>
                            </div>
                            <ol className="text-sm text-(--base-07) space-y-2 list-decimal pl-5">
                                <li>
                                    In <span className="text-(--base-09)">Settings &rarr; Warp &rarr; External Nodes</span>,
                                    name the location and add the External node.
                                </li>
                                <li>Copy the compose file shown there onto the machine and start it.</li>
                                <li>
                                    The node enrols itself and appears under{' '}
                                    <a href="/nodes?tab=external" className="text-(--accent-light) hover:underline">
                                        My infrastructure &rarr; External nodes
                                    </a>
                                    {' '}within ~30s. Its Link starts once the node has enrolled.
                                </li>
                            </ol>
                            <a href="/settings/warp" className="btn btn-primary btn-sm inline-flex w-fit">
                                Open Warp settings <ExternalLink size={12} />
                            </a>
                        </>
                    )}
                </div>

                <div className="modal-footer">
                    <button onClick={onClose} className="btn btn-secondary">Close</button>
                </div>
            </div>
        </div>
    );
}
