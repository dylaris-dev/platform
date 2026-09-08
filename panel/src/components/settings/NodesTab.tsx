"use client";

import React, { useState, useEffect, useRef } from 'react';
import {
    getNodes, Node,
    getPlacementSettings, savePlacementSettings, PlacementSettings,
    setNodePlacement, configureNode, Region,
    getNodeCpu, getNodeStorage, getNodeDeployBundle, updateNodeCpuset, type NodeCpuTopology,
    getNodeAdmission, updateNodeAdmission, addAdmissionCIDR, deleteAdmissionCIDR, resetNodePairing,
    mintEnrollToken, listEnrollTokens, revokeEnrollToken, type AdmissionCIDR, type NodeEnrollToken,
    getFleetStoragePlacement, setFleetStoragePlacement, type StoragePlacement as StoragePlacementConfig,
} from '@/lib/api';
import DeleteNodeModal, { type DeletableNode } from '@/components/nodes/DeleteNodeModal';
import NodeJoinAttempts from '@/components/settings/NodeJoinAttempts';
import { parseCpuset, compactCpuset } from '@/lib/cpuset';
import { nodeConnectivity, dotFor, connLabel } from '@/lib/connectivity';
import { timeAgo } from '@/lib/time';
import { SkeletonHeader, SkeletonCard } from '@/components/Skeleton';
import Select from '@/components/ui/Select';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import StoragePlacement from '@/components/StoragePlacement';
import LinkUpdatesPanel from '@/components/settings/LinkUpdatesPanel';
import GuardedTabs from '@/components/settings/GuardedTabs';
import { useTabParam } from '@/lib/useTabParam';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard from '@/components/settings/SettingsCard';
import { toast } from '@/components/ui/Toast';
import { regionLabel, regionFlag } from '@/lib/regions';
import { useAppData } from '@/lib/AppDataContext';
import {
    Network, Server, Globe, Settings as SettingsIcon, Save,
    Pencil, X, AlertTriangle, SlidersHorizontal, Cpu, KeyRound, Copy,
    ShieldCheck, Plus, Trash2, Ticket, RotateCcw,
} from 'lucide-react';
import HelpTip from '@/components/ui/HelpTip';

// Shape of GET /nodes/{id}/deploy-bundle — the secret-free node + link deploy
// ENV for an already-enrolled node (see WarpTab's mint+reveal pattern).
interface DeployBundle {
    nodeId: string;
    grpcTlsFingerprint: string;
    linkSecret: string;
    linkDiscoveryProof: string;
}

type SubTab = 'nodes' | 'external' | 'placement' | 'enrollment';

export const NODE_TABS: readonly SubTab[] = ['nodes', 'external', 'placement', 'enrollment'];

const NAV_ITEMS: { id: SubTab; label: string; icon: React.ElementType }[] = [
    { id: 'nodes', label: 'Nodes', icon: Server },
    // Split from Nodes rather than mixed in: an external node is the operator's
    // own hardware outside the swarm and only routes through gateway+beam, so
    // "why can I not place a server there" has a different answer for it.
    // Customers' machines appear on NEITHER - see the 'fleet' scope below.
    { id: 'external', label: 'External nodes', icon: Globe },
    { id: 'placement', label: 'Placement', icon: SettingsIcon },
    // How a machine BECOMES a node (admission gate + enroll tokens) is a
    // different job from looking after the ones that already are, and mixing
    // the two put a rarely-touched security gate underneath a list that polls
    // every 5 seconds.
    { id: 'enrollment', label: 'Node settings', icon: ShieldCheck },
];

// The sub-navigation used to be a SECOND left sidebar, sitting beside the
// settings sidebar. Two vertical navigations next to each other read as one
// confused one, and it put this page's depth at three levels. A tab bar is the
// only sub-navigation in the panel now, and it is the last level there is.
export default function NodesTab() {
    const [subTab, setSubTab] = useTabParam<SubTab>(NODE_TABS, 'nodes');

    return (
        <div className="flex flex-col h-full min-h-0">
            <GuardedTabs items={NAV_ITEMS} active={subTab} onChange={setSubTab} ariaLabel="Node settings" />

            <div className="flex-1 overflow-y-auto pt-5">
                {subTab === 'nodes' && <NodesPanel showToast={toast} kind="platform" />}
                {subTab === 'external' && <NodesPanel showToast={toast} kind="external" />}
                {subTab === 'placement' && <PlacementPanel showToast={toast} />}
                {subTab === 'enrollment' && (
                    <div className="space-y-6">
                        <NodeEnrollmentPanel showToast={toast} />
                        <LinkUpdatesPanel showToast={toast} />
                    </div>
                )}
            </div>
        </div>
    );
}

// ─────────────────────────────────────────────
// Nodes list (existing UX + per-node placement edit)
// ─────────────────────────────────────────────

// A node is "external" (home/Warp node) when its tags carry the `external`
// marker — same client-side derivation the badge rendering already uses.
// External nodes can only carry traffic in gateway routing mode, so when
// Game Traffic is on IP:Port they're shown as unusable.
function isExternalNode(node: Node): boolean {
    return !!node.tags && node.tags.split(',').map(t => t.trim()).includes('external');
}

function NodesPanel({ showToast, kind }: { showToast: (msg: string, ok?: boolean) => void; kind: 'platform' | 'external' }) {
    // Applied routing mode from the shared app context (same source WarpTab
    // gates on) — no extra fetch, no new store. regions feed the config picker.
    const { routingMode, regions } = useAppData();
    const gatewayOff = routingMode === 'ip_port';

    const [nodes, setNodes] = useState<Node[]>([]);
    const [editingPlacement, setEditingPlacement] = useState<number | null>(null);
    const [editingConfig, setEditingConfig] = useState<number | null>(null);
    const [revealed, setRevealed] = useState<DeployBundle | null>(null);
    const [revealingId, setRevealingId] = useState<number | null>(null);
    const [resettingId, setResettingId] = useState<number | null>(null);
    // Sticky rather than a toast: loadNodes runs on a 5s interval, so a toast
    // per failed poll would be a stream of them.
    const [loadError, setLoadError] = useState<string | null>(null);
    const [deleteTarget, setDeleteTarget] = useState<DeletableNode | null>(null);

    useEffect(() => {
        loadNodes();
        const interval = setInterval(loadNodes, 5000);
        return () => clearInterval(interval);
    }, []);

    const loadNodes = async () => {
        // 'fleet' is the operator's own machines, platform and external. This
        // screen used to ask for the unscoped list, which is every machine the
        // caller may see - so a customer's node was listed here with Configure
        // and Reset pairing live beside it. Enforced in Core, not here: a
        // filter in the panel would still have sent the rows to the browser.
        const res = await getNodes('fleet');
        // A dropped failure left the list at its last value - empty on the first
        // poll - and the panel then stated "No nodes connected". For an operator
        // checking whether the fleet is up, that is the worst possible thing to
        // get wrong: it reads as an answer about the nodes when it is an answer
        // about the request.
        if (res.success) { setNodes(res.nodes); setLoadError(null); }
        else setLoadError(res.message || 'Could not load the node list.');
    };

    const revealDeployBundle = async (nodeId: number) => {
        setRevealingId(nodeId);
        const res = await getNodeDeployBundle(nodeId);
        setRevealingId(null);
        if (res.success) {
            setRevealed({
                nodeId: res.nodeId,
                grpcTlsFingerprint: res.grpcTlsFingerprint,
                linkSecret: res.linkSecret,
                linkDiscoveryProof: res.linkDiscoveryProof,
            });
        } else {
            showToast(res.message || res.error || 'Failed to load deploy bundle', false);
        }
    };

    const resetPairing = async (node: Node) => {
        if (!await confirmDialog({
            title: `Reset pairing for "${node.name}"?`,
            message: 'Its current secret is invalidated immediately. A node holding the cluster secret re-pairs itself within seconds; any other node appears under Connection attempts, where you admit it. Nothing needs changing on the machine, and server data is preserved.',
            confirmLabel: 'Reset pairing',
        })) return;
        setResettingId(node.id);
        const res = await resetNodePairing(node.id);
        setResettingId(null);
        if (res.success) showToast(res.note || 'Pairing reset.');
        else showToast(res.message || 'Reset failed.', false);
    };

    // Extracted so the copy buttons below can send the exact same string that
    // is rendered in the <pre> blocks, with no duplication.
    // Core returns a fingerprint only while its own GRPC_TLS_ENABLED is on, so
    // an empty one means this platform runs the control channel in plaintext.
    // Both branches state the flag: the node defaults it to true, so printing
    // GRPC_TLS_FINGERPRINT= with nothing after it would hand an operator a file
    // that turns TLS on and gives it nothing to verify against.
    const grpcTlsEnv = revealed?.grpcTlsFingerprint
        ? `GRPC_TLS_ENABLED=true\nGRPC_TLS_FINGERPRINT=${revealed.grpcTlsFingerprint}`
        : 'GRPC_TLS_ENABLED=false';
    const nodeEnv = revealed ? `${grpcTlsEnv}
NODE_ENROLL_TOKEN=<your enroll token>
CORE_GRPC_ADDR=<core-host:25501>
NODE_MANAGES_LINK=true
LINK_IMAGE=<public link image, e.g. ghcr.io/dylaris-dev/gateway-link:latest>` : '';
    const linkEnv = revealed ? `NODE_ID=${revealed.nodeId}
LINK_SECRET=${revealed.linkSecret}
LINK_DISCOVERY_PROOF=${revealed.linkDiscoveryProof}` : '';

    // Both tabs poll the same 'fleet' request and split it here. One poll, and
    // the two tabs can never disagree about which machines exist. Customers'
    // machines are absent already - Core does not send them to this scope.
    const shownNodes = nodes.filter(n => isExternalNode(n) === (kind === 'external'));

    return (
        <div className="space-y-6">
            {/* Above the list, and only on the Nodes tab so it appears once: a
                machine Core is refusing is the most urgent thing on this screen
                and used to be invisible here entirely. It renders nothing when
                nothing is being refused. */}
            {kind === 'platform' && <NodeJoinAttempts onAdmitted={loadNodes} />}

            <div>
                <h3 className="text-base font-display font-bold text-(--base-09) mb-1">
                    {kind === 'external' ? 'External nodes' : 'Cluster nodes'}
                </h3>
                <p className="text-xs text-(--base-06) mb-4">
                    {kind === 'external'
                        ? 'Your own machines outside the swarm. They reach players through the gateway and beam only.'
                        : 'The machines running inside your swarm.'}
                </p>
                <div className="space-y-3">
                    {loadError && (
                        <div className="p-3 border border-(--error) rounded-md text-(--error) text-sm">
                            {loadError} The list below may be out of date.
                        </div>
                    )}
                    {shownNodes.length === 0 && !loadError ? (
                        <div className="text-center p-8 border border-dashed border-(--base-04) rounded-lg text-(--base-06) text-sm">
                            {kind === 'external'
                                ? 'No external nodes. A node joins this tab by starting with NODE_EXTERNAL=true.'
                                : 'No nodes connected. Start a node!'}
                        </div>
                    ) : (
                        shownNodes.map(node => (
                            <NodeCard
                                key={node.id}
                                node={node}
                                regions={regions}
                                gatewayRequired={isExternalNode(node) && gatewayOff}
                                isEditing={editingPlacement === node.id}
                                isConfiguring={editingConfig === node.id}
                                onEdit={() => { setEditingConfig(null); setEditingPlacement(node.id); }}
                                onCancel={() => setEditingPlacement(null)}
                                onSaved={() => { setEditingPlacement(null); loadNodes(); showToast('Placement updated'); }}
                                onConfigure={() => { setEditingPlacement(null); setEditingConfig(node.id); }}
                                onConfigCancel={() => setEditingConfig(null)}
                                onConfigSaved={() => { setEditingConfig(null); loadNodes(); showToast('Node configured'); }}
                                onCpuPoolSaved={() => { loadNodes(); showToast('Container CPU pool updated'); }}
                                onError={msg => showToast(msg, false)}
                                onRevealDeployBundle={() => revealDeployBundle(node.id)}
                                revealingDeployBundle={revealingId === node.id}
                                onResetPairing={() => resetPairing(node)}
                                resettingPairing={resettingId === node.id}
                                onOpenDeleteDialog={node.status === 'online' ? null : () => setDeleteTarget({ id: node.id, name: node.name })}
                            />
                        ))
                    )}
                </div>
            </div>

            {deleteTarget && (
                <DeleteNodeModal
                    node={deleteTarget}
                    onClose={() => setDeleteTarget(null)}
                    onDeleted={name => { setDeleteTarget(null); loadNodes(); showToast(`Node "${name}" deleted`); }}
                    onError={msg => showToast(msg, false)}
                />
            )}

            {revealed && (
                <div className="modal-overlay animate-fade-in" onClick={() => setRevealed(null)}>
                    <div className="modal-panel max-w-xl" onClick={e => e.stopPropagation()}>
                        <div className="modal-header"><h3 className="modal-title text-(--accent-light)">Setup values — {revealed.nodeId.slice(0, 8)}</h3></div>
                        <div className="modal-body space-y-3">
                            <div className="space-y-1">
                                <label className="mono-label">Node deploy ENV (secret-free)</label>
                                <pre className="p-3 rounded-md bg-(--base-02) border border-(--base-04) font-mono text-xs whitespace-pre-wrap break-all">{nodeEnv}</pre>
                                <button onClick={() => { navigator.clipboard.writeText(nodeEnv); showToast('Node deploy ENV copied.', true); }} className="btn btn-secondary btn-sm">
                                    <Copy size={12} /> Copy node ENV
                                </button>
                            </div>
                            <div className="space-y-1">
                                <label className="mono-label">Link deploy ENV (manual / DC only)</label>
                                <p className="text-xs text-(--base-06)">A secret-free node auto-provisions its own Link sidecar (set LINK_IMAGE + NODE_MANAGES_LINK). Use this only for a manually deployed / DC Link.</p>
                                <pre className="p-3 rounded-md bg-(--base-02) border border-(--base-04) font-mono text-xs whitespace-pre-wrap break-all">{linkEnv}</pre>
                                <button onClick={() => { navigator.clipboard.writeText(linkEnv); showToast('Link deploy ENV copied.', true); }} className="btn btn-secondary btn-sm">
                                    <Copy size={12} /> Copy link ENV
                                </button>
                            </div>
                        </div>
                        <div className="modal-footer"><button onClick={() => setRevealed(null)} className="btn btn-primary">Done</button></div>
                    </div>
                </div>
            )}
        </div>
    );
}

interface NodeCardProps {
    node: Node;
    regions: Region[];
    // External node + gateway not active — its servers can't receive player
    // traffic or file access until routing mode is switched to Gateway/Both.
    gatewayRequired: boolean;
    isEditing: boolean;
    isConfiguring: boolean;
    onEdit: () => void;
    onCancel: () => void;
    onSaved: () => void;
    onConfigure: () => void;
    onConfigCancel: () => void;
    onConfigSaved: () => void;
    onCpuPoolSaved: () => void;
    onError: (msg: string) => void;
    onRevealDeployBundle: () => void;
    revealingDeployBundle: boolean;
    onResetPairing: () => void;
    resettingPairing: boolean;
    // Only offered while the node is offline: deleting a machine that is still
    // running would leave its containers alive with nothing tracking them.
    onOpenDeleteDialog: (() => void) | null;
}

// nodeActionClass styles a node action that OPENS an editor below the row.
//
// The active one used to be removed from the row entirely: clicking "Configure"
// made the Configure button vanish, which reads as the click having broken
// something rather than having opened the form under it. It stays, lit, and
// clicking it again closes what it opened - so the control that opened a panel
// is also the one that closes it.
//
// needsAttention is the separate, pre-existing highlight for a node that has
// never been configured. Being open outranks it: the same button cannot be
// telling you to look here and showing you are already here.
export function nodeActionClass(active: boolean, needsAttention: boolean): string {
    const base = 'text-xs inline-flex items-center gap-1 rounded px-1.5 py-0.5 -mx-1.5 transition-colors';
    if (active) {
        return `${base} bg-(--accent)/15 text-(--accent-light) hover:bg-(--accent)/25`;
    }
    if (needsAttention) {
        return `${base} text-(--accent-light) hover:text-(--accent)`;
    }
    return `${base} text-(--base-06) hover:text-(--accent-light)`;
}

function NodeCard({ node, regions, gatewayRequired, isEditing, isConfiguring, onEdit, onCancel, onSaved, onConfigure, onConfigCancel, onConfigSaved, onCpuPoolSaved, onError, onRevealDeployBundle, revealingDeployBundle, onResetPairing, resettingPairing, onOpenDeleteDialog }: NodeCardProps) {
    const [cpuRatio, setCpuRatio] = useState(node.cpuOvercommitRatio ?? 1.0);
    const [ramRatio, setRamRatio] = useState(node.ramOvercommitRatio ?? 1.0);
    const [saving, setSaving] = useState(false);
    // Best-effort CPU topology for the P/E breakdown chip. Non-fatal: the
    // cores stat falls back to node.totalCpu when the node hasn't reported.
    const [cpuTopology, setCpuTopology] = useState<NodeCpuTopology | null>(null);
    // The node's allowed container core pool ("" = all cores). Drives the pool
    // editor below; refetched whenever the node prop changes (the tab polls).
    const [nodeCpuset, setNodeCpuset] = useState('');
    // Best-effort per-path storage summary for the Storage stat. Non-fatal:
    // falls back to '—' when the node hasn't reported (offline / no heartbeat).
    const [nodeStorage, setNodeStorage] = useState<{ total_bytes: number; free_bytes: number }[] | null>(null);

    useEffect(() => {
        setCpuRatio(node.cpuOvercommitRatio ?? 1.0);
        setRamRatio(node.ramOvercommitRatio ?? 1.0);
    }, [node.cpuOvercommitRatio, node.ramOvercommitRatio, isEditing]);

    useEffect(() => {
        let cancelled = false;
        getNodeCpu(node.id).then(res => {
            if (!cancelled && res?.success) {
                setCpuTopology(res.topology ?? null);
                setNodeCpuset(res.nodeCpuset ?? '');
            }
        }).catch(() => { /* non-fatal */ });
        return () => { cancelled = true; };
    }, [node.id]);

    useEffect(() => {
        let cancelled = false;
        getNodeStorage(node.id).then(res => {
            if (!cancelled && res?.success) setNodeStorage(res.storage ?? []);
        }).catch(() => { /* non-fatal */ });
        return () => { cancelled = true; };
    }, [node.id]);

    // "8P + 8E" when the node reports a hybrid layout, else null.
    const peBreakdown = cpuTopology?.hybrid
        ? (() => {
            const p = cpuTopology.cores.filter(c => c.type === 'P').length;
            const e = cpuTopology.cores.filter(c => c.type === 'E').length;
            return `${p}P + ${e}E`;
        })()
        : null;

    const handleSave = async () => {
        if (cpuRatio <= 0 || ramRatio <= 0) {
            onError('Overcommit ratios must be > 0');
            return;
        }
        setSaving(true);
        const res = await setNodePlacement(node.id, { cpuOvercommitRatio: cpuRatio, ramOvercommitRatio: ramRatio });
        setSaving(false);
        if (res.success) onSaved();
        else onError(res.message || 'Save failed');
    };

    return (
        <div className="card p-2.5 transition-colors hover:border-(--base-05)">
            <div className="flex flex-col md:flex-row md:items-center justify-between gap-2.5">
                <div className="flex items-center gap-3 min-w-0">
                    {(() => {
                      const now = Date.now();
                      const { tier } = nodeConnectivity(node.status, node.lastSeenAt, now);
                      const dot = dotFor(tier, 'bg-(--success-light) shadow-[0_0_8px_var(--success-light)]');
                      const title = tier === 'ok' ? node.status : connLabel(tier, node.lastSeenAt);
                      return <div className={`status-dot shrink-0 ${dot}`} title={title}></div>;
                    })()}
                    <div className="w-8 h-8 bg-(--accent-ghost) text-(--accent-light) rounded-md flex items-center justify-center border border-(--accent-border) shrink-0">
                        <Server size={18} />
                    </div>
                    <div className="flex flex-col min-w-0">
                        <div className="flex flex-col md:flex-row md:items-center gap-2 md:gap-3 min-w-0">
                            <div className="font-medium text-sm text-(--base-09) whitespace-nowrap truncate">
                                {node.displayName || node.name || (node.token ? node.token.slice(0, 8) : '')}
                            </div>
                            <div className="h-4 w-px bg-(--base-04) hidden md:block"></div>
                            <div className="text-xs font-mono text-(--base-06) flex items-center bg-(--base-01) px-2 py-1 rounded-sm border border-(--base-04) w-fit whitespace-nowrap">
                                <Globe size={14} className="mr-1.5 opacity-70" />
                                {node.address}
                            </div>
                        </div>
                        {node.status !== 'online' && node.lastSeenAt && (
                            <p className="text-[10px] text-(--base-05) font-mono mt-1">Last seen {timeAgo(node.lastSeenAt)}</p>
                        )}
                    </div>
                </div>

                <div className="flex items-center flex-wrap gap-3 gap-y-2 shrink-0">
                    {node.region && (
                        <span className="inline-flex items-center gap-1.5 px-2 py-0.5 rounded-md bg-(--accent-ghost) border border-(--accent-border) text-(--accent-light) text-xs font-medium" title="Region (DYLARIS_REGION env)">
                            <span>{regionFlag(node.region)}</span>
                            <span>{regionLabel(node.region)}</span>
                        </span>
                    )}
                    {node.tags && node.tags !== 'auto-discovered' && (
                        <div className="flex flex-wrap gap-1.5">
                            {node.tags.split(',').filter(tag => tag.trim() !== 'external').map(tag => {
                                const t = tag.trim();
                                if (!t) return null;
                                return <span key={t} className="badge badge-accent">{t}</span>;
                            })}
                        </div>
                    )}
                    {node.tags && node.tags.split(',').map(t => t.trim()).includes('external') && (
                        <span className="badge badge-accent" title="External / home node — forces gateway+beam">external</span>
                    )}
                    {node.needsConfiguration && (
                        <span
                            className="badge badge-warning inline-flex items-center gap-1"
                            title="This node booted with only a cluster secret and has no region. Configure its name, region and tags so it can be used for placement."
                        >
                            <AlertTriangle size={11} />
                            Needs configuration
                        </span>
                    )}
                    {gatewayRequired && (
                        <span
                            className="badge badge-warning inline-flex items-center gap-1"
                            title="This external node needs Gateway routing. Switch Game Traffic to Gateway or Both in the Gateway tab — otherwise its servers can't receive player traffic or file access."
                        >
                            <AlertTriangle size={11} />
                            Requires gateway
                        </span>
                    )}
                    <button
                        onClick={isConfiguring ? onConfigCancel : onConfigure}
                        aria-expanded={isConfiguring}
                        className={nodeActionClass(isConfiguring, !!node.needsConfiguration)}
                        title="Configure name, region and tags"
                    >
                        <SlidersHorizontal size={11} />
                        Configure
                    </button>
                    <button
                        onClick={isEditing ? onCancel : onEdit}
                        aria-expanded={isEditing}
                        className={nodeActionClass(isEditing, false)}
                        title="Edit placement"
                    >
                        <Pencil size={11} />
                        Placement
                    </button>
                    <span className="inline-flex items-center gap-1">
                        <button
                            onClick={onRevealDeployBundle}
                            disabled={revealingDeployBundle}
                            className="text-xs text-(--base-06) hover:text-(--accent-light) inline-flex items-center gap-1 transition-colors disabled:opacity-40"
                        >
                            <KeyRound size={11} />
                            {revealingDeployBundle ? 'Loading…' : 'Show setup values'}
                        </button>
                        <HelpTip label="About the setup values">
                            <p className="mb-2">
                                Shows this node&apos;s connection values again: its identity, the TLS
                                fingerprint to pin, and its link token. They are the same ones the
                                enrolment gave you once, and this is the only way back to them.
                            </p>
                            <p className="mb-2">
                                You need it when you rebuild the host, move the node to another
                                machine, or lose the compose file - anything that means writing the
                                node&apos;s environment out again. In day-to-day operation you never
                                open it.
                            </p>
                            <p>
                                Treat what it shows as a credential: the link token in it
                                authenticates that node&apos;s tunnel. It does not contain the cluster
                                secret. If the node&apos;s own secret is gone rather than mislaid,
                                re-pairing is the way back, not this.
                            </p>
                        </HelpTip>
                    </span>
                    <button
                        onClick={onResetPairing}
                        disabled={resettingPairing}
                        className="text-xs text-(--base-06) hover:text-(--error-light) inline-flex items-center gap-1 transition-colors disabled:opacity-40"
                        title="Invalidate this node's secret; the node re-pairs itself or waits to be admitted"
                    >
                        <RotateCcw size={11} />
                        {resettingPairing ? 'Resetting…' : 'Reset pairing'}
                    </button>
                    {onOpenDeleteDialog && (
                        <button
                            onClick={onOpenDeleteDialog}
                            className="text-xs text-(--base-06) hover:text-(--error-light) inline-flex items-center gap-1 transition-colors"
                            title="Delete this node and everything on it"
                        >
                            <Trash2 size={11} />
                            Delete
                        </button>
                    )}
                </div>
            </div>

            {/* Configuration editor (name / region / tags) */}
            {isConfiguring && (
                <NodeConfigForm
                    node={node}
                    regions={regions}
                    onSaved={onConfigSaved}
                    onCancel={onConfigCancel}
                    onError={onError}
                />
            )}

            {/* Placement summary / editor */}
            <div className="mt-2.5 pt-2.5 border-t border-(--base-03) grid grid-cols-2 md:grid-cols-6 gap-x-3 gap-y-2 text-xs">
                <Stat label="Total CPU" value={node.totalCpu ? `${node.totalCpu.toFixed(1)} cores` : '—'} />
                <Stat
                    label="CPU cores"
                    value={node.totalCpu ? (
                        <span className="inline-flex items-center gap-1.5">
                            {Math.round(node.totalCpu)}
                            {peBreakdown && (
                                <span className="badge badge-accent text-[9px]" title="Performance + Efficiency cores">{peBreakdown}</span>
                            )}
                        </span>
                    ) : '—'}
                />
                <Stat label="Total RAM" value={node.totalRamMb ? `${(node.totalRamMb / 1024).toFixed(1)} GB` : '—'} />
                <Stat
                    label="Storage"
                    value={nodeStorage && nodeStorage.length > 0
                        ? `${(nodeStorage.reduce((a, s) => a + s.free_bytes, 0) / 1e9).toFixed(0)} / ${(nodeStorage.reduce((a, s) => a + s.total_bytes, 0) / 1e9).toFixed(0)} GB free`
                        : '—'}
                />
                <Stat
                    label="CPU Overcommit"
                    value={isEditing ? (
                        <div className="relative inline-block w-20">
                            <input
                                type="number"
                                step={10}
                                min={10}
                                value={Math.round(cpuRatio * 100)}
                                onChange={e => setCpuRatio(Math.max(0.1, Number(e.target.value) / 100))}
                                className="input-mono w-full pr-5 text-center text-xs [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                            />
                            <span className="absolute right-1.5 top-1/2 -translate-y-1/2 text-[10px] text-(--base-06) font-mono pointer-events-none">%</span>
                        </div>
                    ) : `${Math.round((node.cpuOvercommitRatio ?? 1.0) * 100)}%`}
                />
                <Stat
                    label="RAM Overcommit"
                    value={isEditing ? (
                        <div className="relative inline-block w-20">
                            <input
                                type="number"
                                step={10}
                                min={10}
                                value={Math.round(ramRatio * 100)}
                                onChange={e => setRamRatio(Math.max(0.1, Number(e.target.value) / 100))}
                                className="input-mono w-full pr-5 text-center text-xs [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                            />
                            <span className="absolute right-1.5 top-1/2 -translate-y-1/2 text-[10px] text-(--base-06) font-mono pointer-events-none">%</span>
                        </div>
                    ) : `${Math.round((node.ramOvercommitRatio ?? 1.0) * 100)}%`}
                />
            </div>

            {isEditing && (
                <div className="mt-3 flex items-center gap-2 justify-end">
                    <button onClick={onCancel} className="btn btn-secondary btn-sm">
                        <X size={12} /> Cancel
                    </button>
                    <button onClick={handleSave} disabled={saving} className="btn btn-primary btn-sm disabled:opacity-40">
                        <Save size={12} /> {saving ? 'Saving…' : 'Save'}
                    </button>
                </div>
            )}

            <NodeCpuPoolEditor
                node={node}
                topology={cpuTopology}
                nodeCpuset={nodeCpuset}
                onSaved={onCpuPoolSaved}
                onError={onError}
            />
        </div>
    );
}

// NodeCpuPoolEditor lets an admin restrict which host cores this node may place
// containers on (the node's container CPU pool). Empty pool = all cores allowed.
// The restricted pool then narrows per-server CPU pinning (see CpuPinningControl).
function NodeCpuPoolEditor({
    node, topology, nodeCpuset, onSaved, onError,
}: {
    node: Node;
    topology: NodeCpuTopology | null;
    nodeCpuset: string;
    onSaved: () => void;
    onError: (msg: string) => void;
}) {
    // 'all' = no restriction; 'restrict' = pin the node to a subset of cores.
    const [restrict, setRestrict] = useState(nodeCpuset.trim() !== '');
    const [selected, setSelected] = useState<Set<number>>(() => new Set(parseCpuset(nodeCpuset)));
    const [saving, setSaving] = useState(false);

    // Re-sync from the latest server value whenever it changes (the tab polls
    // every 5s) so a fresh save elsewhere doesn't get clobbered by stale state.
    useEffect(() => {
        setRestrict(nodeCpuset.trim() !== '');
        setSelected(new Set(parseCpuset(nodeCpuset)));
    }, [nodeCpuset]);

    const toggleCore = (id: number) => {
        setSelected(prev => {
            const next = new Set(prev);
            if (next.has(id)) next.delete(id);
            else next.add(id);
            return next;
        });
    };

    const pendingCpuset = restrict ? compactCpuset([...selected]) : '';
    const dirty = pendingCpuset !== nodeCpuset.trim();

    const handleSave = async () => {
        if (restrict && selected.size === 0) {
            onError('Select at least one core, or choose "All cores".');
            return;
        }
        setSaving(true);
        const res = await updateNodeCpuset(node.id, pendingCpuset);
        setSaving(false);
        if (res?.success !== false && !res?.error) onSaved();
        else onError(res.message || res.error || 'Save failed');
    };

    return (
        <div className="mt-3 pt-3 border-t border-(--base-03) space-y-3">
            <div className="flex items-center gap-2">
                <Cpu size={13} className="text-(--base-06)" />
                <span className="mono-label">Container CPU pool</span>
            </div>

            <div className="flex bg-(--base-03) p-0.5 rounded-md w-fit">
                {([
                    { id: 'all', label: 'All cores' },
                    { id: 'restrict', label: 'Restrict to cores' },
                ] as const).map(({ id, label }) => {
                    const active = (id === 'restrict') === restrict;
                    return (
                        <button
                            key={id}
                            type="button"
                            onClick={() => setRestrict(id === 'restrict')}
                            className={`px-2.5 py-1 text-[11px] rounded-sm transition-colors ${
                                active ? 'bg-(--accent) text-white' : 'text-(--base-07) hover:text-(--base-09)'
                            }`}
                        >
                            {label}
                        </button>
                    );
                })}
            </div>

            {!restrict ? (
                <p className="text-[11px] text-(--base-06)">Containers may use any host core.</p>
            ) : topology ? (
                <div className="flex flex-col gap-2">
                    <div className="flex flex-wrap gap-1.5">
                        {topology.cores.map(core => {
                            const isSel = selected.has(core.id);
                            return (
                                <button
                                    key={core.id}
                                    type="button"
                                    onClick={() => toggleCore(core.id)}
                                    title={`Core ${core.id}${topology.hybrid ? ` (${core.type === 'P' ? 'Performance' : core.type === 'E' ? 'Efficiency' : 'standard'})` : ''}`}
                                    className={`w-10 h-10 rounded-md border flex flex-col items-center justify-center transition-colors ${
                                        isSel
                                            ? 'border-(--accent) bg-(--accent-ghost) text-(--accent-light)'
                                            : 'border-(--base-04) text-(--base-07) hover:border-(--base-05) hover:text-(--base-09)'
                                    }`}
                                >
                                    <span className="font-mono text-xs leading-none">{core.id}</span>
                                    {topology.hybrid && core.type !== 'standard' && (
                                        <span className={`font-mono text-[8px] leading-none mt-0.5 ${core.type === 'P' ? 'text-(--accent-light)' : 'text-(--base-06)'}`}>
                                            {core.type}
                                        </span>
                                    )}
                                </button>
                            );
                        })}
                    </div>
                    <p className="text-[11px] text-(--base-06)">
                        {selected.size === 0
                            ? 'No cores selected. Pick at least one.'
                            : `Pool: ${compactCpuset([...selected])}`}
                    </p>
                </div>
            ) : (
                <p className="text-[11px] text-(--base-06)">This node has not reported its CPU layout yet.</p>
            )}

            <div className="flex items-center gap-2 justify-end">
                <button
                    onClick={handleSave}
                    disabled={saving || !dirty}
                    className="btn btn-secondary btn-sm disabled:opacity-40"
                >
                    <Save size={12} /> {saving ? 'Saving…' : 'Save pool'}
                </button>
            </div>
        </div>
    );
}

// NodeConfigForm lets an admin adopt an auto-discovered node by setting its
// name, region and tags, plus an optional human display name. Saving persists
// to the DB (PATCH /nodes/{id}/config); from then on the node's heartbeat env
// no longer overwrites name/region/tags.
function NodeConfigForm({
    node, regions, onSaved, onCancel, onError,
}: {
    node: Node;
    regions: Region[];
    onSaved: () => void;
    onCancel: () => void;
    onError: (msg: string) => void;
}) {
    const [name, setName] = useState(node.name || node.token || '');
    const [displayName, setDisplayName] = useState(node.displayName || '');
    const [region, setRegion] = useState(node.region || '');
    const [tags, setTags] = useState(node.tags && node.tags !== 'auto-discovered' ? node.tags : '');
    const [saving, setSaving] = useState(false);

    const handleSave = async () => {
        if (!region) { onError('Please select a region'); return; }
        setSaving(true);
        const res = await configureNode(node.id, { name: name.trim(), region, tags: tags.trim(), displayName: displayName.trim() });
        setSaving(false);
        if (res.success) onSaved();
        else onError(res.message || res.error || 'Save failed');
    };

    return (
        <div className="mt-3 pt-3 border-t border-(--base-03) space-y-3">
            <div className="grid grid-cols-1 md:grid-cols-4 gap-3">
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Node Name</label>
                    <input
                        value={name}
                        onChange={e => setName(e.target.value)}
                        className="input-field text-sm"
                        placeholder={node.token}
                    />
                </div>
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Display Name</label>
                    <input
                        value={displayName}
                        onChange={e => setDisplayName(e.target.value)}
                        className="input-field text-sm"
                        placeholder={node.name || node.token}
                    />
                </div>
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Region</label>
                    <select
                        value={region}
                        onChange={e => setRegion(e.target.value)}
                        className="input-field text-sm"
                    >
                        <option value="">Select region…</option>
                        {regions.map(rg => (
                            <option key={rg.id} value={rg.id}>{rg.displayName}</option>
                        ))}
                    </select>
                </div>
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Tags</label>
                    <input
                        value={tags}
                        onChange={e => setTags(e.target.value)}
                        className="input-field text-sm"
                        placeholder="e.g. premium, ssd"
                    />
                </div>
            </div>
            <p className="text-xs text-(--base-06)">
                Saving adopts this node: its name, region and tags are managed here from now on and the node&apos;s env values no longer overwrite them. Display name is a purely cosmetic label shown on the card. Keep the <code className="font-mono bg-(--base-03) px-1 py-0.5 rounded text-(--base-08)">external</code> tag if this is a home/Warp node.
            </p>
            <div className="flex items-center gap-2 justify-end">
                <button onClick={onCancel} className="btn btn-secondary btn-sm">
                    <X size={12} /> Cancel
                </button>
                <button onClick={handleSave} disabled={saving} className="btn btn-primary btn-sm disabled:opacity-40">
                    <Save size={12} /> {saving ? 'Saving…' : 'Save'}
                </button>
            </div>
        </div>
    );
}

function Stat({ label, value }: { label: string; value: React.ReactNode }) {
    return (
        <div>
            <div className="mono-label">{label}</div>
            <div className="text-sm text-(--base-09) leading-tight">{value}</div>
        </div>
    );
}

// ─────────────────────────────────────────────
// Fleet-wide default storage order
// ─────────────────────────────────────────────

/**
 * FleetStorageOrder edits the policy a node uses when it has none of its own.
 *
 * A fleet-wide ORDER beats a single "default path": paths a node does not have
 * are skipped and paths the list does not mention go last, so one entry of
 * /storage means "/storage first everywhere" across a mixed fleet without
 * naming a single node.
 */
function FleetStorageOrder() {
    const [paths, setPaths] = useState<string[]>([]);
    const [placement, setPlacement] = useState<StoragePlacementConfig | null>(null);

    useEffect(() => {
        getFleetStoragePlacement().then(res => {
            if (!res?.success) return;
            setPaths(res.paths ?? []);
            setPlacement(res.placement ?? { mode: 'auto', order: [] });
        });
    }, []);

    if (!placement) return <SkeletonCard height="h-24" />;
    if (paths.length === 0) {
        return (
            <p className="text-xs text-(--base-06)">
                No node is currently reporting a storage path, so there is nothing to order yet.
            </p>
        );
    }

    return (
        <div className="max-w-md">
            <StoragePlacement
                paths={paths}
                placement={placement}
                save={setFleetStoragePlacement}
                onSaved={setPlacement}
                autoLabel="Most free"
                hint="Applies to every node that has no order of its own. A node skips paths it does not have."
            />
        </div>
    );
}

// ─────────────────────────────────────────────
// Placement (global defaults + rebalance toggle)
// ─────────────────────────────────────────────

function PlacementPanel({ showToast }: { showToast: (msg: string, ok?: boolean) => void }) {
    const [settings, setSettings] = useState<PlacementSettings>({
        cpuOvercommitDefault: 2.0,
        ramOvercommitDefault: 1.0,
        diskBufferGb: 50,
        rebalanceEnabled: false,
        rebalanceThreshold: 90,
        portMode: 'sequential',
        containerPort: 25565,
        pidsLimit: 0,
        ioWeight: 0,
        diskEnforcement: 'soft',
        diskWarnPercent: 80,
        diskCriticalPercent: 95,
    });
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);
    // Fifteen fields with no dirty tracking: clicking another tab dropped the
    // lot without a word. The per-node editors above keep their Cancel/Save,
    // because entering edit mode on a row and committing or cancelling it is the
    // same shape as a dialog. This is not that - it is a page of fields.
    const snapshotRef = useRef<PlacementSettings | null>(null);

    useEffect(() => {
        getPlacementSettings().then(res => {
            if (res.success && res.settings) {
                setSettings(res.settings);
                snapshotRef.current = res.settings;
            }
            setLoading(false);
        });
    }, []);

    const handleSave = async (): Promise<boolean> => {
        setSaving(true);
        try {
            const res = await savePlacementSettings(settings);
            if (res.success) snapshotRef.current = settings;
            showToast(res.success ? 'Placement settings saved.' : (res.message || 'Save failed.'), res.success);
            return !!res.success;
        } finally {
            setSaving(false);
        }
    };

    const dirty =
        snapshotRef.current !== null &&
        JSON.stringify(settings) !== JSON.stringify(snapshotRef.current);

    useUnsavedChanges({
        dirty,
        saving,
        save: handleSave,
        discard: () => { if (snapshotRef.current) setSettings(snapshotRef.current); },
    });

    if (loading) return (
        <div className="space-y-6">
            <SkeletonHeader />
            <SkeletonCard height="h-96" />
        </div>
    );

    return (
        <SettingsPage
            title="Placement"
            description="Global defaults for how new servers are placed on nodes. Per-node overrides live on the Nodes tab."
        >
            <SettingsCard
                title="Placement defaults"
                form={{ dirty, saving, save: handleSave, discard: () => { if (snapshotRef.current) setSettings(snapshotRef.current); } }}
            >
                <div>
                    <h3 className="mono-label mb-3">Overcommit Defaults</h3>
                    <p className="text-xs text-(--base-06) mb-4">
                        How much allocation is allowed relative to physical capacity.
                        <span className="font-mono"> 100%</span> = no overcommit, <span className="font-mono">200%</span> = double-book.
                        Applied to new nodes — existing nodes keep their per-node value.
                    </p>

                    <div className="grid grid-cols-2 gap-4">
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label flex items-center gap-1.5">
                                CPU Overcommit
                                <HelpTip label="About CPU overcommit">
                                    <p className="mb-2">
                                        How much CPU may be promised beyond what the host physically
                                        has. 100% promises exactly the cores present, 200% double-books
                                        them.
                                    </p>
                                    <p>
                                        Overcommitting CPU is comparatively safe: when everyone wants
                                        it at once they each get less and everything slows down. That
                                        is why the usual answer here is higher than for RAM, where the
                                        same bet ends in a kill rather than a slowdown.
                                    </p>
                                </HelpTip>
                            </label>
                            <div className="relative w-32">
                                <input
                                    type="number"
                                    step={10}
                                    min={10}
                                    value={Math.round(settings.cpuOvercommitDefault * 100)}
                                    onChange={e => setSettings(s => ({ ...s, cpuOvercommitDefault: Math.max(0.1, Number(e.target.value) / 100) }))}
                                    className="input-field input-mono w-full pr-7 text-center [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                                />
                                <span className="absolute right-3 top-[9px] text-(--base-06) font-mono text-sm pointer-events-none">%</span>
                            </div>
                            <p className="text-xs text-(--base-06)">CPU is time-shared — 200% is conservative.</p>
                        </div>
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label flex items-center gap-1.5">
                                RAM Overcommit
                                <HelpTip label="About RAM overcommit">
                                    <p className="mb-2">
                                        Above 100% you are promising more memory than the host has, on
                                        the bet that servers do not all use their allocation at once.
                                    </p>
                                    <p className="mb-2">
                                        When that bet loses, the kernel picks a container and kills
                                        it. The player sees a crash, not a warning, and nothing in the
                                        panel caused it - so this is the setting whose failure is
                                        hardest to trace back to the change that made it.
                                    </p>
                                    <p>
                                        Unlike CPU, memory cannot be shared out slowly under pressure.
                                        Leave it at 100% unless you have measured that your servers
                                        idle well below their allocation.
                                    </p>
                                </HelpTip>
                            </label>
                            <div className="relative w-32">
                                <input
                                    type="number"
                                    step={10}
                                    min={10}
                                    value={Math.round(settings.ramOvercommitDefault * 100)}
                                    onChange={e => setSettings(s => ({ ...s, ramOvercommitDefault: Math.max(0.1, Number(e.target.value) / 100) }))}
                                    className="input-field input-mono w-full pr-7 text-center [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                                />
                                <span className="absolute right-3 top-[9px] text-(--base-06) font-mono text-sm pointer-events-none">%</span>
                            </div>
                            <p className="text-xs text-(--base-06)">RAM can&apos;t be reclaimed — 100% is safest, increase only for idle-heavy workloads.</p>
                        </div>
                    </div>
                </div>

                <div className="border-t border-(--base-03) pt-5">
                    <h3 className="mono-label mb-3">Disk Buffer</h3>
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">Disk Buffer</label>
                        <div className="relative w-32">
                            {/* Clamped in the handler, not just via min: min only
                                bounds the spinner, a typed value still reaches
                                state. Below the floor Core does not clamp to 10
                                either - it discards the value and falls back to
                                the 50 GB default, so a typed 5 would silently
                                become 50. Same shape as the overcommit fields. */}
                            <input
                                type="number"
                                min={10}
                                value={settings.diskBufferGb}
                                onChange={e => setSettings(s => ({ ...s, diskBufferGb: Math.max(10, Number(e.target.value)) }))}
                                className="input-field input-mono w-full pr-9 text-center [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                            />
                            <span className="absolute right-3 top-[9px] text-(--base-06) font-mono text-sm pointer-events-none">GB</span>
                        </div>
                        <p className="text-xs text-(--base-06)">
                            Free space every storage path must retain. Counted against what is already <em>promised</em> to
                            the servers on that path, not just what they have written &mdash; twenty servers with a 50 GB
                            limit have claimed a terabyte the day they are created. Minimum 10 GB; anything lower is
                            discarded and the 50 GB default applies instead.
                        </p>
                    </div>

                    <div className="mt-5 flex flex-col gap-[5px]">
                        <label className="input-label">When a placement would eat into the buffer</label>
                        <div className="w-64">
                            <Select
                                value={settings.diskEnforcement}
                                onChange={v => setSettings(s => ({ ...s, diskEnforcement: v as 'soft' | 'hard' }))}
                                options={[
                                    { value: 'soft', label: 'Soft — place it anyway, flag it' },
                                    { value: 'hard', label: 'Hard — refuse the placement' },
                                ]}
                                ariaLabel="Disk buffer enforcement"
                            />
                        </div>
                        <p className="text-xs text-(--base-06)">
                            Applies to placing a server and raising a limit. Neither mode stops a server that is already
                            running from writing &mdash; that is what the per-server disk limit does, and only on xfs/ext4.
                        </p>
                    </div>

                    <div className="mt-5 flex gap-4">
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label">Warn at</label>
                            <div className="relative w-28">
                                <input
                                    type="number" min={1} max={100}
                                    value={settings.diskWarnPercent}
                                    onChange={e => setSettings(s => ({ ...s, diskWarnPercent: Number(e.target.value) }))}
                                    className="input-field input-mono w-full pr-8 text-center [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                                />
                                <span className="absolute right-3 top-[9px] text-(--base-06) font-mono text-sm pointer-events-none">%</span>
                            </div>
                        </div>
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label">Critical at</label>
                            <div className="relative w-28">
                                <input
                                    type="number" min={1} max={100}
                                    value={settings.diskCriticalPercent}
                                    onChange={e => setSettings(s => ({ ...s, diskCriticalPercent: Number(e.target.value) }))}
                                    className="input-field input-mono w-full pr-8 text-center [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                                />
                                <span className="absolute right-3 top-[9px] text-(--base-06) font-mono text-sm pointer-events-none">%</span>
                            </div>
                        </div>
                    </div>
                    <p className="mt-2 text-xs text-(--base-06)">
                        Measured on the <em>projected</em> fill: written plus still promised, over total.
                    </p>
                </div>

                <div className="border-t border-(--base-03) pt-5">
                    <h3 className="mono-label mb-3">Default storage order</h3>
                    <FleetStorageOrder />
                </div>

                <div className="border-t border-(--base-03) pt-5">
                    <h3 className="mono-label mb-3">Process Limit (anti fork-bomb)</h3>
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">Max processes / threads per server</label>
                        <div className="relative w-32">
                            <input
                                type="number"
                                min={0}
                                value={settings.pidsLimit}
                                onChange={e => setSettings(s => ({ ...s, pidsLimit: Math.max(0, Number(e.target.value)) }))}
                                className="input-field input-mono w-full text-center [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                            />
                        </div>
                        <p className="text-xs text-(--base-06)">
                            Caps the cgroup PID count per server container (applies on next (re)start). <span className="font-mono">0</span> = unlimited. This counts <strong>threads too</strong>, so set it generously (e.g. <span className="font-mono">4096</span>) — a value too low will throttle or crash heavy modded servers.
                        </p>
                    </div>
                </div>

                <div className="border-t border-(--base-03) pt-5">
                    <h3 className="mono-label mb-3">Disk I/O Fair-Share</h3>
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">blkio weight for all MC containers (10-1000, 0 = off)</label>
                        <div className="relative w-32">
                            <input
                                type="number"
                                min={0}
                                max={1000}
                                value={settings.ioWeight}
                                onChange={e => setSettings(s => ({ ...s, ioWeight: Math.max(0, Math.min(1000, Number(e.target.value))) }))}
                                className="input-field input-mono w-full text-center [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                            />
                        </div>
                        <p className="text-xs text-(--base-06)">
                            One global relative disk-I/O share applied to <strong>every</strong> MC container on this node (not a per-server value; applies on next (re)start), so one noisy server can&apos;t starve the others. It only has any effect <strong>under disk contention</strong>, and only with a weight-aware I/O scheduler (<span className="font-mono">BFQ</span>, or legacy <span className="font-mono">CFQ</span>). On typical NVMe/SSD nodes (<span className="font-mono">none</span>/<span className="font-mono">mq-deadline</span>) it is a harmless no-op. <span className="font-mono">0</span> = off; valid range <span className="font-mono">10-1000</span>. This is a <strong>relative weight, not a hard cap</strong>.
                        </p>
                    </div>
                </div>

                <div className="border-t border-(--base-03) pt-5">
                    <h3 className="mono-label mb-3">Host-Port Allocation</h3>
                    <p className="text-xs text-(--base-06) mb-4">
                        How nodes pick a host port from their range when a server is created. Applies cluster-wide;
                        per-node port range stays in the node&apos;s <code className="font-mono bg-(--base-03) px-1.5 py-0.5 rounded text-(--base-08)">PORT_RANGE</code> env.
                    </p>
                    <div className="grid grid-cols-2 gap-4">
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label">Port Mode</label>
                            <select
                                value={settings.portMode}
                                onChange={e => setSettings(s => ({ ...s, portMode: e.target.value as 'sequential' | 'random' }))}
                                className="input-field w-44"
                            >
                                <option value="sequential">Sequential</option>
                                <option value="random">Random</option>
                            </select>
                            <p className="text-xs text-(--base-06)">
                                Sequential = first-free port. Random = uniform over the range — useful when you want to obscure server adjacency.
                            </p>
                        </div>
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label">Default Container Port</label>
                            <input
                                type="number"
                                min={1}
                                max={65535}
                                value={settings.containerPort}
                                onChange={e => setSettings(s => ({ ...s, containerPort: Number(e.target.value) }))}
                                className="input-field input-mono w-28 text-center [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                            />
                            <p className="text-xs text-(--base-06)">MC default is 25565. Change only when your server image binds elsewhere.</p>
                        </div>
                    </div>
                </div>

                <div className="border-t border-(--base-03) pt-5">
                    <h3 className="mono-label mb-3">Auto Rebalance (Phase B)</h3>
                    <div className="flex items-center justify-between">
                        <div>
                            <p className="text-sm text-(--base-09)">Migrate idle auto-move servers off overloaded nodes</p>
                            <p className="text-xs text-(--base-06)">Background worker checks node load every 60s. Migration logic ships in Phase B.</p>
                        </div>
                        <button
                            type="button"
                            role="switch"
                            aria-checked={settings.rebalanceEnabled}
                            onClick={() => setSettings(s => ({ ...s, rebalanceEnabled: !s.rebalanceEnabled }))}
                            className={`toggle-track ${settings.rebalanceEnabled ? 'toggle-track-on' : 'toggle-track-off'}`}
                        >
                            <span className={`toggle-knob ${settings.rebalanceEnabled ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                        </button>
                    </div>
                    {settings.rebalanceEnabled && (
                        <div className="flex flex-col gap-[5px] mt-3">
                            <label className="input-label">Overload Threshold</label>
                            <div className="flex items-center gap-3">
                                <input
                                    type="range"
                                    min={50}
                                    max={100}
                                    value={settings.rebalanceThreshold}
                                    onChange={e => setSettings(s => ({ ...s, rebalanceThreshold: Number(e.target.value) }))}
                                    className="flex-1"
                                />
                                <span className="font-mono text-sm text-(--base-09) w-12 text-right">{settings.rebalanceThreshold}%</span>
                            </div>
                            <p className="text-xs text-(--base-06)">A node is &quot;overloaded&quot; when its allocation exceeds this percentage of its overcommit-adjusted capacity.</p>
                        </div>
                    )}
                </div>
            </SettingsCard>
        </SettingsPage>
    );
}

// ─────────────────────────────────────────────
// Node settings: how a machine becomes a node, and who is allowed to try.
// ─────────────────────────────────────────────

function NodeEnrollmentPanel({ showToast }: { showToast: (msg: string, ok?: boolean) => void }) {
    // The admission gate decides whether a minted token can ever be redeemed, so
    // the two cards have to agree on it. One owner, passed down - not two fetches
    // that drift the moment the gate is changed.
    const [joinMode, setJoinMode] = useState('open');
    return (
        <div className="space-y-6">
            <HowNodesJoinCard />
            <AdmissionCard showToast={showToast} onJoinMode={setJoinMode} />
            <EnrollTokensSection showToast={showToast} joinMode={joinMode} />
        </div>
    );
}

// There are exactly TWO ways a machine becomes a node, and the old card here
// described only the first while implying it was the only one ("No manual setup
// needed"). Since the pairing hardening landed, an unknown node is created ONLY
// on the gRPC enroll path (grpc/server.go): either it proves possession of
// CLUSTER_SECRET, or it presents a single-use enroll token. A heartbeat from an
// id Core never assigned is dropped outright (services/discovery.go). Stating
// both paths is the difference between an operator understanding why their node
// never showed up and an operator re-reading their Swarm labels for an hour.
function HowNodesJoinCard() {
    return (
        <div className="card border-(--accent-border) p-6 space-y-4">
            <h3 className="text-base font-display font-bold text-(--accent-light) flex items-center gap-2">
                <Network size={18} /> How a machine becomes a node
            </h3>

            <div className="space-y-1.5">
                <div className="text-sm font-medium text-(--base-09)">1. Your own hosts — automatic</div>
                <p className="text-sm text-(--base-07)">
                    A host that runs the platform stack <em>and</em> holds the cluster secret proves it on
                    connect and enrolls itself. Nothing to do here.
                </p>
                <p className="text-xs text-(--base-06)">
                    Example for a Docker Swarm deployment — adapt it to however your own stack is
                    scheduled (labels, constraints and stack name are yours, not ours):
                </p>
                <code className="block bg-(--base-02) border border-(--base-04) px-2.5 py-1.5 rounded-md text-(--base-08) font-mono text-xs break-all">
                    docker node update --label-add role=node &lt;hostname&gt;
                </code>
            </div>

            <div className="space-y-1.5 pt-1 border-t border-(--base-03)">
                <div className="text-sm font-medium text-(--base-09)">2. Someone else&apos;s machine — enroll token</div>
                <p className="text-sm text-(--base-07)">
                    A bring-your-own-node machine never receives the cluster secret. It pairs with a
                    single-use enroll token instead, minted below or by the tenant on their own
                    &ldquo;My nodes&rdquo; page.
                </p>
            </div>

            <p className="text-xs text-(--base-06) pt-1 border-t border-(--base-03)">
                A machine with neither is rejected: a heartbeat alone can never create a node.
            </p>
        </div>
    );
}

// ─────────────────────────────────────────────
// Node Admission (P0b-5): join-mode + IP allowlist gate on a node's FIRST
// registration. Always available regardless of BYON — this guards the
// existing auto-discovery flow too.
// ─────────────────────────────────────────────

function AdmissionCard({ showToast, onJoinMode }: { showToast: (msg: string, ok?: boolean) => void; onJoinMode?: (mode: string) => void }) {
    const [joinMode, setJoinMode] = useState<string>('open');
    const [ipMode, setIpMode] = useState<string>('allow');
    const [cidrs, setCidrs] = useState<AdmissionCIDR[]>([]);
    const [loading, setLoading] = useState(true);
    const [saving, setSaving] = useState(false);
    const [newCidr, setNewCidr] = useState('');
    const [newLabel, setNewLabel] = useState('');
    const [cidrBusy, setCidrBusy] = useState(false);

    const load = async () => {
        const res = await getNodeAdmission();
        if (res.success) {
            setJoinMode(res.joinMode || 'open');
            setIpMode(res.ipMode || 'allow');
            setCidrs(res.cidrs || []);
            onJoinMode?.(res.joinMode || 'open');
        }
        setLoading(false);
    };
    useEffect(() => { load(); }, []);

    const save = async (nextJoin: string, nextIp: string) => {
        setSaving(true);
        const res = await updateNodeAdmission({ joinMode: nextJoin, ipMode: nextIp });
        setSaving(false);
        if (res.success) {
            setJoinMode(nextJoin);
            setIpMode(nextIp);
            onJoinMode?.(nextJoin);
            showToast('Admission updated.');
        } else {
            showToast(res.message || 'Save failed.', false);
        }
    };

    const addCidr = async () => {
        const cidr = newCidr.trim();
        if (!cidr) return;
        setCidrBusy(true);
        const res = await addAdmissionCIDR({ cidr, label: newLabel.trim() });
        setCidrBusy(false);
        if (res.success) {
            setNewCidr(''); setNewLabel('');
            showToast('CIDR added.');
            load();
        } else {
            showToast(res.message || 'Invalid CIDR.', false);
        }
    };

    const removeCidr = async (id: string) => {
        setCidrBusy(true);
        const res = await deleteAdmissionCIDR(id);
        setCidrBusy(false);
        if (res.success) { showToast('CIDR removed.'); load(); }
        else showToast(res.message || 'Remove failed.', false);
    };

    if (loading) return <SkeletonCard />;

    return (
        <div className="card p-6 space-y-5">
            <div className="flex items-center gap-2">
                <ShieldCheck size={18} className="text-(--accent-light)" />
                <h3 className="text-base font-display font-bold text-(--base-09)">Node Admission</h3>
            </div>
            <p className="text-sm text-(--base-06)">
                Gates a node&apos;s FIRST registration (network + join), before any auth. Known nodes always reconnect. Inert defaults: join <code className="font-mono text-xs">open</code>, IP <code className="font-mono text-xs">allow</code>.
            </p>

            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                <div className="space-y-1">
                    <label className="input-label">Join mode</label>
                    <select value={joinMode} disabled={saving} onChange={e => save(e.target.value, ipMode)} className="input-field w-full text-sm">
                        <option value="open">open — any new node may join</option>
                        <option value="one-shot">one-shot — admit exactly one, then disable</option>
                        <option value="disabled">disabled — reject all new nodes</option>
                    </select>
                </div>
                <div className="space-y-1">
                    <label className="input-label">IP mode</label>
                    <select value={ipMode} disabled={saving} onChange={e => save(joinMode, e.target.value)} className="input-field w-full text-sm">
                        <option value="allow">allow — CIDR list advisory (any IP)</option>
                        <option value="deny">deny — only IPs inside a listed CIDR</option>
                    </select>
                </div>
            </div>

            <div className="space-y-2">
                <label className="mono-label">Allowed CIDRs {ipMode === 'allow' && <span className="text-(--base-06) normal-case">(advisory while IP mode is allow)</span>}</label>
                {cidrs.length === 0 ? (
                    <div className="text-sm text-(--base-06)">No CIDRs configured.</div>
                ) : (
                    <div className="space-y-2">
                        {cidrs.map(c => (
                            <div key={c.id} className="flex items-center justify-between bg-(--base-02) border border-(--base-04) rounded-md px-3 py-2">
                                <div className="flex items-center gap-3">
                                    <code className="font-mono text-xs text-(--base-09)">{c.cidr}</code>
                                    {c.label && <span className="text-xs text-(--base-06)">{c.label}</span>}
                                </div>
                                <button onClick={() => removeCidr(c.id)} disabled={cidrBusy} className="text-(--base-06) hover:text-(--error-light) transition-colors disabled:opacity-40" title="Remove CIDR">
                                    <Trash2 size={14} />
                                </button>
                            </div>
                        ))}
                    </div>
                )}
                <div className="flex items-center gap-2 pt-1">
                    <input value={newCidr} onChange={e => setNewCidr(e.target.value)} placeholder="10.0.0.0/24" disabled={cidrBusy} className="input-field flex-1 font-mono text-sm disabled:opacity-40" />
                    <input value={newLabel} onChange={e => setNewLabel(e.target.value)} placeholder="label (optional)" disabled={cidrBusy} className="input-field flex-1 text-sm disabled:opacity-40" />
                    <button onClick={addCidr} disabled={cidrBusy} className="btn btn-secondary btn-sm shrink-0 disabled:opacity-40">
                        <Plus size={14} /> Add
                    </button>
                </div>
            </div>
        </div>
    );
}

// ─────────────────────────────────────────────
// Enroll tokens (P0b-5): single-use tokens authorizing a NEW node's first
// pairing. BYON-gated on the backend (byonActive) — this section renders
// visibly disabled with a "Requires BYON" note when the byon flag is off.
// ─────────────────────────────────────────────

function EnrollTokensSection({ showToast, joinMode }: { showToast: (msg: string, ok?: boolean) => void; joinMode: string }) {
    // The enroll-token backend is BYON-gated (byonActive). Read the already-loaded,
    // SSE-reactive flag from the app context (same idiom as GatewayTab/UserManagementTab)
    // instead of a one-shot fetch, so this section reacts live and never flashes
    // "Requires BYON" on initial load.
    const { featureFlags } = useAppData();
    const byonEnabled = featureFlags.byon;
    // "disabled" rejects every new registration, so a token minted now can never
    // be redeemed. Listing and revoking existing tokens stays available - those
    // are cleanup actions and blocking them would strand real tokens.
    const joiningBlocked = joinMode === 'disabled';

    const [tokens, setTokens] = useState<NodeEnrollToken[]>([]);
    const [label, setLabel] = useState('');
    const [expiresDays, setExpiresDays] = useState(7);
    const [minting, setMinting] = useState(false);
    const [revealed, setRevealed] = useState<{ token: string } | null>(null);

    const load = async () => {
        const res = await listEnrollTokens();
        if (res.success) setTokens(res.tokens || []);
    };
    useEffect(() => {
        if (byonEnabled) load();
    }, [byonEnabled]);

    const mint = async () => {
        setMinting(true);
        const res = await mintEnrollToken({ label: label.trim(), expiresDays });
        setMinting(false);
        if (res.success && res.token) {
            setRevealed({ token: res.token });
            setLabel('');
            load();
        } else {
            showToast(res.message || 'Mint failed.', false);
        }
    };

    const revoke = async (id: string) => {
        const res = await revokeEnrollToken(id);
        if (res.success) { showToast('Token revoked.'); load(); }
        else showToast(res.message || 'Revoke failed.', false);
    };

    const fmt = (s?: string) => (s ? new Date(s).toLocaleString() : '—');

    return (
        <div className="card p-6 space-y-5">
            <div className="flex items-center gap-2">
                <Ticket size={18} className="text-(--accent-light)" />
                <h3 className="text-base font-display font-bold text-(--base-09)">Enroll tokens</h3>
            </div>
            <p className="text-sm text-(--base-06)">
                Single-use tokens that authorize a NEW node&apos;s first pairing. Shown once at mint time; start the node with <code className="font-mono text-xs">NODE_ENROLL_TOKEN</code> set.
            </p>

            {!byonEnabled ? (
                <p className="text-sm text-(--base-06)">Requires BYON — enable it in Settings → Features to mint node enroll tokens.</p>
            ) : (
              <>
            {/* Minting is disabled while admission rejects every new node: the
                token would be issued, handed over, and then fail at the gate for
                a reason that is nowhere near the token. Note the gate is NOT
                "open" - a token is required in EVERY join mode, since it is the
                only way a machine without the cluster secret can pair at all.
                Only "disabled" makes a token unusable. */}
            {joiningBlocked && (
                <div className="alert alert-warning text-xs">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <span>
                        Node admission is set to <code className="font-mono">disabled</code>, so no new node
                        can pair right now and a token minted here could not be redeemed. Set a join mode
                        above first.
                    </span>
                </div>
            )}
            <fieldset disabled={joiningBlocked} className={joiningBlocked ? 'opacity-50' : undefined}>
            <div className="flex items-end gap-2">
                <div className="flex-1 space-y-1">
                    <label className="input-label">Label</label>
                    <input value={label} onChange={e => setLabel(e.target.value)} placeholder="e.g. home-pc" className="input-field w-full text-sm" />
                </div>
                <div className="w-28 space-y-1">
                    <label className="input-label">Expires (days)</label>
                    <input type="number" min={1} value={expiresDays} onChange={e => setExpiresDays(parseInt(e.target.value) || 7)} className="input-field w-full text-sm" />
                </div>
                <button onClick={mint} disabled={minting} className="btn btn-primary btn-sm shrink-0 disabled:opacity-40">
                    <Plus size={14} /> {minting ? 'Minting…' : 'Mint token'}
                </button>
            </div>
            </fieldset>

            {tokens.length === 0 ? (
                <div className="text-sm text-(--base-06)">No enroll tokens.</div>
            ) : (
                <div className="space-y-2">
                    {tokens.map(t => (
                        <div key={t.id} className="flex items-center justify-between bg-(--base-02) border border-(--base-04) rounded-md px-3 py-2">
                            <div className="flex flex-col">
                                <span className="text-sm text-(--base-09)">{t.label || <span className="text-(--base-06)">(no label)</span>}</span>
                                <span className="text-xs text-(--base-06)">
                                    created {fmt(t.createdAt)} · expires {fmt(t.expiresAt)} · {t.consumedAt ? `consumed ${fmt(t.consumedAt)}` : 'unused'}
                                </span>
                            </div>
                            <button onClick={() => revoke(t.id)} className="text-(--base-06) hover:text-(--error-light) transition-colors" title="Revoke token">
                                <Trash2 size={14} />
                            </button>
                        </div>
                    ))}
                </div>
            )}
              </>
            )}

            {revealed && (
                <div className="modal-overlay animate-fade-in" onClick={() => setRevealed(null)}>
                    <div className="modal-panel max-w-lg" onClick={e => e.stopPropagation()}>
                        <div className="modal-header"><h3 className="modal-title text-(--accent-light)">Enroll token</h3></div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-07)">Shown once. Copy it now — it cannot be retrieved later.</p>
                            <div className="space-y-1">
                                <label className="mono-label">NODE_ENROLL_TOKEN</label>
                                <pre className="p-3 rounded-md bg-(--base-02) border border-(--base-04) font-mono text-xs whitespace-pre-wrap break-all">{revealed.token}</pre>
                                <button onClick={() => { navigator.clipboard.writeText(revealed.token); showToast('Enroll token copied.', true); }} className="btn btn-secondary btn-sm">
                                    <Copy size={12} /> Copy token
                                </button>
                            </div>
                        </div>
                        <div className="modal-footer"><button onClick={() => setRevealed(null)} className="btn btn-primary">Done</button></div>
                    </div>
                </div>
            )}
        </div>
    );
}
