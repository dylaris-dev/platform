"use client";

import React, { useState, useEffect, useMemo } from 'react';
import Link from 'next/link';
import {
    getUsers, User, createServer, getNodes, Node,
    getAvailableTags, getAvailableRegions, pickNode, NodeCandidate,
    updateServerResources, API_URL,
} from '../lib/api';
import { regionFlag, regionLabelFrom } from '../lib/regions';
import { X, Server, CircleCheck, Info, ArrowRight, Rocket, Network, HardDrive, Tag as TagIcon, Move, MapPin, Cpu } from 'lucide-react';
import CpuPinningControl from './CpuPinningControl';
import { useAppData } from '@/lib/AppDataContext';
import { sortUsersForPicker } from '@/lib/userOrder';
import { nodeLabel } from '@/lib/nodeLabel';

interface StoragePathInfo {
    path: string;
    total_bytes: number;
    free_bytes: number;
    used_bytes: number;
    server_count: number;
}

async function fetchNodeStorage(nodeId: number): Promise<StoragePathInfo[]> {
    try {
        const res = await fetch(`${API_URL}/nodes/${nodeId}/storage`);
        const data = await res.json();
        return data.success ? (data.storage || []) : [];
    } catch {
        return [];
    }
}

function fmtGB(bytes: number): string {
    return (bytes / (1024 * 1024 * 1024)).toFixed(1);
}

interface CreateServerWizardProps {
    isOpen: boolean;
    onClose: () => void;
    proxiesEnabled?: boolean;
}

export default function CreateServerWizard({ isOpen, onClose, proxiesEnabled = true }: CreateServerWizardProps) {
    // Admins get the full scheduling wizard (assign owner, any node, tags,
    // regions). A BYON tenant gets a slimmed flow: owner is themselves, and only
    // their own nodes are selectable. The 'placement' scope is what decides
    // that, for both: it offers unowned machines to an operator and an owned one
    // only to its owner, so the picker cannot suggest a target the create call
    // would refuse.
    // regions carries the operator's own names for them; regionLabelFrom
    // prefers those over the built-in map.
    const { user, regions } = useAppData();
    const isAdmin = !!user?.isAdmin;

    const [step, setStep] = useState(1);
    const [loading, setLoading] = useState(false);
    const [users, setUsers] = useState<User[]>([]);
    const [nodes, setNodes] = useState<Node[]>([]);
    const [nodesLoaded, setNodesLoaded] = useState(false);
    const [searchTerm, setSearchTerm] = useState("");

    const [nodeId, setNodeId] = useState("");
    const [nodeSearch, setNodeSearch] = useState("");

    // Target mode: pick a specific node OR pick tag(s) and let the scheduler
    // choose the best node from the intersection (AND-semantics).
    const [targetMode, setTargetMode] = useState<'node' | 'tag'>('node');
    const [selectedTags, setSelectedTags] = useState<string[]>([]);
    const [availableTags, setAvailableTags] = useState<string[]>([]);
    const [tagPreview, setTagPreview] = useState<NodeCandidate | null>(null);
    const [tagPreviewReason, setTagPreviewReason] = useState<string>('');

    // Region — first-class, env-driven on the node side. Empty string means
    // "any region" (the scheduler ignores the region filter).
    const [selectedRegion, setSelectedRegion] = useState<string>('');
    const [availableRegions, setAvailableRegions] = useState<string[]>([]);

    // Validation and create failures used to go to window.alert. A native alert
    // is not part of the design system, blocks the thread, and - the reason it
    // is a correctness problem and not only a cosmetic one - a browser that has
    // suppressed further dialogs swallows it silently, so the wizard just
    // stopped doing anything with no visible cause.
    const [formError, setFormError] = useState<string | null>(null);

    const [ownerId, setOwnerId] = useState<string | null>(null);
    const [serverType, setServerType] = useState<'game' | 'proxy'>('game');
    const [ram, setRam] = useState(2048);
    const [cpuLimit, setCpuLimit] = useState(0);
    const [diskLimit, setDiskLimit] = useState(20);
    const [storagePath, setStoragePath] = useState('auto');
    const [storagePaths, setStoragePaths] = useState<StoragePathInfo[]>([]);
    const [autoMove, setAutoMove] = useState(false);
    const [cpuMode, setCpuMode] = useState<'shared' | 'auto' | 'manual'>('shared');
    const [cpuset, setCpuset] = useState('');

    useEffect(() => {
        if (!isOpen) return;
        setStep(1);
        setNodeId(""); setOwnerId(null); setServerType('game'); setRam(2048); setCpuLimit(0); setDiskLimit(20);
        setSearchTerm(""); setNodeSearch("");
        setTargetMode('node'); setSelectedTags([]); setTagPreview(null); setTagPreviewReason('');
        setSelectedRegion(''); setAvailableRegions([]);
        setAutoMove(false);
        setCpuMode('shared'); setCpuset('');
        setNodesLoaded(false);

        // Owner assignment + tag/region scheduling are admin-only. A tenant owns
        // what they create, so skip the (admin-only) user list and pin the owner
        // to themselves.
        if (isAdmin) {
            getUsers().then(res => {
                if (res.success && res.users) {
                    // Yourself first, then the role holders, then everyone else.
                    const ordered = sortUsersForPicker(res.users, user?.id);
                    setUsers(ordered);
                    if (ordered.length > 0) setOwnerId(ordered[0].id);
                }
            });
            getAvailableRegions().then(res => {
                if (res.success) setAvailableRegions(res.regions || []);
            });
        } else if (user?.id) {
            setOwnerId(user.id);
        }

        getNodes('placement').then(res => {
            if (res.success && res.nodes) {
                setNodes(res.nodes);
                const online = res.nodes.filter((n: Node) => n.status === 'online');
                if (online.length > 0) setNodeId(String(online[0].id));
            }
        }).finally(() => setNodesLoaded(true));
    }, [isOpen, isAdmin, user?.id]);

    // Region changed → reload the tag list scoped to that region so admins
    // can't pick a tag that doesn't exist for the chosen region. Admin-only:
    // tenants never see the tag/region scheduler.
    useEffect(() => {
        if (!isOpen || !isAdmin) return;
        getAvailableTags(selectedRegion || undefined).then(res => {
            if (res.success) {
                const tags = res.tags || [];
                setAvailableTags(tags);
                // Drop any selected tags that are no longer in the pool.
                setSelectedTags(prev => prev.filter(t => tags.includes(t)));
            }
        });
    }, [isOpen, isAdmin, selectedRegion]);

    // Scheduler preview: ask which node would be picked whenever region,
    // tags, or resource shape changes. Surfaces the choice + reason in the
    // wizard so admins aren't deploying blind.
    useEffect(() => {
        if (targetMode !== 'tag') {
            setTagPreview(null);
            setTagPreviewReason('');
            return;
        }
        let cancelled = false;
        pickNode({
            region: selectedRegion || undefined,
            tags: selectedTags.length > 0 ? selectedTags : undefined,
            ramMb: ram,
            cpuCores: cpuLimit,
            diskGb: diskLimit,
        }).then(res => {
            if (cancelled) return;
            if (res.success && res.picked) {
                setTagPreview(res.picked);
                setTagPreviewReason(res.reason || '');
            } else {
                setTagPreview(null);
                setTagPreviewReason(res.reason || 'No eligible node');
            }
        }).catch(() => {
            if (!cancelled) { setTagPreview(null); setTagPreviewReason('Preview failed'); }
        });
        return () => { cancelled = true; };
    }, [targetMode, selectedRegion, selectedTags, ram, cpuLimit, diskLimit]);

    // Load storage paths when node changes
    useEffect(() => {
        if (!nodeId) { setStoragePaths([]); return; }
        fetchNodeStorage(Number(nodeId)).then(setStoragePaths);
        setStoragePath('auto');
    }, [nodeId]);

    const onlineNodes = useMemo(() => nodes.filter(n => n.status === 'online'), [nodes]);

    // A tenant with no online node of their own can't self-create a server: show
    // the "add a node" upsell instead of an empty picker. Admins never hit this.
    const showNoNodeMsg = !isAdmin && nodesLoaded && onlineNodes.length === 0;

    const filteredNodes = useMemo(() =>
        onlineNodes.filter(n =>
            !nodeSearch ||
            n.name.toLowerCase().includes(nodeSearch.toLowerCase()) ||
            n.address.includes(nodeSearch)
        ),
        [onlineNodes, nodeSearch]
    );

    const filteredUsers = useMemo(() =>
        users.filter(u => u.username.toLowerCase().includes(searchTerm.toLowerCase())),
        [users, searchTerm]
    );

    const handleCreate = async () => {
        setLoading(true);
        setFormError(null);
        const fail = (msg: string) => { setFormError(msg); setLoading(false); };
        if (!ownerId) { fail("Please select an owner."); return; }
        if (targetMode === 'node' && !nodeId) { fail("Please select a Node."); return; }
        if (targetMode === 'tag' && selectedTags.length === 0 && !selectedRegion) {
            fail("Please select a region or at least one tag.");
            return;
        }

        const randomPart = Math.random().toString(36).substring(2, 14);
        const serverUuid = `${ownerId}_${randomPart}`;

        const payload: Record<string, unknown> = {
            uuid: serverUuid,
            name: "",
            ownerId,
            serverType,
            autoMove,
            docker: { ram, cpuLimit, diskLimit: diskLimit > 0 ? diskLimit * 1024 : 0 },
        };
        if (targetMode === 'node') {
            payload.nodeId = nodeId;
        } else {
            if (selectedRegion) payload.region = selectedRegion;
            if (selectedTags.length > 0) payload.tags = selectedTags;
        }
        if (storagePath !== 'auto') {
            payload.storagePath = storagePath;
        }

        const result = await createServer(payload);
        if (result.success) {
            // The create payload never carries pinning. When the user chose a
            // non-default mode, apply it with a follow-up PATCH using the new
            // server id from the create response (server_id).
            if (cpuMode !== 'shared' && result.server_id) {
                const pinRes = await updateServerResources(
                    Number(result.server_id), ram, cpuLimit, diskLimit > 0 ? diskLimit * 1024 : 0,
                    undefined, undefined, { mode: cpuMode, cpuset },
                );
                // The server exists either way, so this is not a create failure -
                // but silently dropping it left the user with a server pinned
                // "shared" after they had explicitly chosen otherwise.
                if (pinRes?.success === false) {
                    setFormError(
                        (pinRes.message || pinRes.error || 'CPU pinning could not be applied.') +
                        ' The server was created; set the pinning from its Resources dialog.',
                    );
                    setLoading(false);
                    return;
                }
            }
            onClose();
        } else {
            setFormError(result.message || "Could not create the container.");
        }
        setLoading(false);
    };

    if (!isOpen) return null;

    return (
        <div className="modal-overlay animate-fade-in">
            <div className="modal-panel w-full max-w-4xl flex flex-col max-h-[90vh]">

                {/* Header */}
                <div className="modal-header flex justify-between items-center">
                    <div>
                        <h2 className="modal-title">Create Container</h2>
                        <p className="input-label mt-1">Step {step} of 2 — Software setup happens after deployment</p>
                    </div>
                    <button onClick={onClose} className="text-(--base-06) hover:text-(--error-light) transition-colors">
                        <X size={24} />
                    </button>
                </div>

                {/* Progress Bar */}
                <div className="flex w-full h-0.5 bg-(--base-03)">
                    <div className="h-full bg-(--accent) transition-all duration-300" style={{ width: `${step * 50}%` }} />
                </div>

                {/* Body */}
                <div className="modal-body overflow-y-auto flex-1 p-8">
                    {showNoNodeMsg ? (
                        <div className="flex flex-col items-center justify-center text-center gap-3 py-12 px-6">
                            <div className="w-14 h-14 rounded-xl bg-(--base-03) flex items-center justify-center">
                                <Server size={26} className="text-(--accent-light)" />
                            </div>
                            <h3 className="text-lg font-display font-bold text-(--base-09)">No nodes yet</h3>
                            <p className="text-sm text-(--base-06) max-w-sm">
                                To host a server you need your own node. Adding a node requires a subscription.
                            </p>
                            <p className="text-xs text-(--base-05) italic">Renting a managed server is coming soon.</p>
                        </div>
                    ) : (
                    <form onSubmit={(e) => e.preventDefault()}>

                        {step === 1 && (
                            <div className="space-y-5 animate-fade-in">
                                {/* Region pills (admin scheduling only) */}
                                {isAdmin && (
                                <section className="space-y-2">
                                    <div className="flex items-center gap-2">
                                        <MapPin size={14} className="text-(--accent-light)" />
                                        <h3 className="text-base font-display font-bold text-(--base-09)">Region</h3>
                                        <span className="font-mono text-[10px] text-(--base-06) ml-auto">
                                            {availableRegions.length === 0
                                                ? 'no regions configured'
                                                : selectedRegion ? regionLabelFrom(selectedRegion, regions) : 'any'}
                                        </span>
                                    </div>
                                    {availableRegions.length === 0 ? (
                                        <div className="rounded-md border border-dashed border-(--base-04) bg-(--base-02) px-3 py-2.5 text-xs text-(--base-06)">
                                            No node currently reports a region. Set{' '}
                                            <code className="font-mono bg-(--base-03) px-1.5 py-0.5 rounded text-(--base-08)">NODE_REGION=eu-central</code>{' '}
                                            on a node and redeploy it — the pill bar will appear once a heartbeat lands.
                                        </div>
                                    ) : (
                                        <div className="flex flex-wrap gap-2">
                                            <button
                                                type="button"
                                                onClick={() => setSelectedRegion('')}
                                                className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors border ${
                                                    !selectedRegion
                                                        ? 'bg-(--accent-ghost) border-(--accent-border) text-(--accent-light)'
                                                        : 'bg-(--base-02) border-(--base-03) text-(--base-07) hover:text-(--base-09) hover:border-(--base-05)'
                                                }`}
                                            >
                                                🌐 Any
                                            </button>
                                            {availableRegions.map(r => (
                                                <button
                                                    key={r}
                                                    type="button"
                                                    onClick={() => setSelectedRegion(r)}
                                                    className={`px-3 py-1.5 rounded-md text-xs font-medium transition-colors inline-flex items-center gap-1.5 border ${
                                                        selectedRegion === r
                                                            ? 'bg-(--accent-ghost) border-(--accent-border) text-(--accent-light)'
                                                            : 'bg-(--base-02) border-(--base-03) text-(--base-07) hover:text-(--base-09) hover:border-(--base-05)'
                                                    }`}
                                                >
                                                    <span>{regionFlag(r)}</span>
                                                    <span>{regionLabelFrom(r, regions)}</span>
                                                </button>
                                            ))}
                                        </div>
                                    )}
                                </section>
                                )}

                                <div className={`grid grid-cols-1 gap-6 ${isAdmin ? 'md:grid-cols-2' : ''}`}>
                                {/* Left: Owner (admin assigns; a tenant owns what they create) */}
                                {isAdmin && (
                                <section className="space-y-2 min-h-80 flex flex-col">
                                    <div className="flex items-center justify-between border-b border-(--base-03) pb-2 min-h-9">
                                        <h3 className="text-base font-display font-bold text-(--base-09)">Assign Owner</h3>
                                        <span className="font-mono text-[10px] text-(--base-06)">
                                            {filteredUsers.length} / {users.length}
                                        </span>
                                    </div>
                                    <input
                                        autoFocus
                                        type="text"
                                        placeholder="Search user..."
                                        value={searchTerm}
                                        onChange={e => setSearchTerm(e.target.value)}
                                        className="input-field w-full"
                                    />
                                    <div className="flex-1 flex flex-col gap-1 overflow-y-auto rounded-md border border-(--base-03) bg-(--base-02) p-1.5">
                                        {filteredUsers.length === 0 ? (
                                            <p className="text-xs text-(--base-06) text-center py-6 italic">No matching users.</p>
                                        ) : (
                                            filteredUsers.map(user => {
                                                const selected = ownerId === user.id;
                                                return (
                                                    <button
                                                        key={user.id}
                                                        type="button"
                                                        onClick={() => setOwnerId(user.id)}
                                                        className={`flex items-center gap-2 px-2 py-1.5 rounded-md text-left transition-colors ${
                                                            selected
                                                                ? 'bg-(--accent-ghost) border border-(--accent-border)'
                                                                : 'border border-transparent hover:bg-(--base-03)'
                                                        }`}
                                                    >
                                                        <div className="w-7 h-7 rounded-md bg-(--base-03) flex items-center justify-center font-medium text-xs text-(--base-08) shrink-0">
                                                            {user.username.charAt(0).toUpperCase()}
                                                        </div>
                                                        <span className="flex-1 truncate text-sm text-(--base-09)">{user.username}</span>
                                                        {user.isAdmin && (
                                                            <span className="font-mono text-[9px] uppercase text-(--accent-light)">admin</span>
                                                        )}
                                                        {selected && <CircleCheck size={14} className="text-(--accent-light) shrink-0" />}
                                                    </button>
                                                );
                                            })
                                        )}
                                    </div>
                                </section>
                                )}

                                {/* Right: Target. Tenants only ever pick a node;
                                    the Node|Tag scheduler toggle is admin-only. */}
                                <section className="space-y-2 min-h-80 flex flex-col">
                                    <div className="flex items-center justify-between border-b border-(--base-03) pb-2 min-h-9">
                                        <h3 className="text-base font-display font-bold text-(--base-09)">Target</h3>
                                        {isAdmin && (
                                        <div className="flex bg-(--base-03) p-0.5 rounded-md">
                                            <button
                                                type="button"
                                                onClick={() => setTargetMode('node')}
                                                className={`px-2.5 py-1 text-[11px] rounded-sm transition-colors inline-flex items-center gap-1 ${targetMode === 'node' ? 'bg-(--accent) text-white' : 'text-(--base-07)'}`}
                                            >
                                                <Server size={11} /> Node
                                            </button>
                                            <button
                                                type="button"
                                                onClick={() => setTargetMode('tag')}
                                                className={`px-2.5 py-1 text-[11px] rounded-sm transition-colors inline-flex items-center gap-1 ${targetMode === 'tag' ? 'bg-(--accent) text-white' : 'text-(--base-07)'}`}
                                            >
                                                <TagIcon size={11} /> Tag
                                            </button>
                                        </div>
                                        )}
                                    </div>

                                    {targetMode === 'node' && (
                                        <>
                                            <input
                                                type="text"
                                                placeholder="Search nodes..."
                                                value={nodeSearch}
                                                onChange={e => setNodeSearch(e.target.value)}
                                                className="input-field w-full"
                                            />
                                            <div className="flex-1 flex flex-col gap-1 overflow-y-auto rounded-md border border-(--base-03) bg-(--base-02) p-1.5">
                                                {filteredNodes.length === 0 ? (
                                                    <p className="text-xs text-(--base-06) text-center py-6 italic">
                                                        No online nodes{nodeSearch ? ' matching search' : ''}.
                                                    </p>
                                                ) : (
                                                    filteredNodes.map(node => {
                                                        const isSelected = nodeId === String(node.id);
                                                        const tags = (node.tags || '').split(',').map(t => t.trim()).filter(Boolean);
                                                        return (
                                                            <button
                                                                key={node.id}
                                                                type="button"
                                                                onClick={() => setNodeId(String(node.id))}
                                                                className={`flex items-center gap-2 px-2 py-1.5 rounded-md text-left transition-colors ${
                                                                    isSelected
                                                                        ? 'bg-(--accent-ghost) border border-(--accent-border)'
                                                                        : 'border border-transparent hover:bg-(--base-03)'
                                                                }`}
                                                            >
                                                                <div className="w-7 h-7 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
                                                                    <Server size={14} className="text-(--accent-light)" />
                                                                </div>
                                                                <div className="flex-1 min-w-0">
                                                                    <div className="text-sm text-(--base-09) truncate">{nodeLabel(node)}</div>
                                                                    <div className="text-[10px] font-mono text-(--base-06) truncate">
                                                                        {node.address}
                                                                        {node.serverCount !== undefined && ` · ${node.serverCount} servers`}
                                                                    </div>
                                                                </div>
                                                                {tags.length > 0 && (
                                                                    <div className="hidden md:flex flex-wrap gap-1 max-w-[40%] justify-end">
                                                                        {tags.slice(0, 2).map(t => (
                                                                            <span key={t} className="badge badge-accent text-[9px]">{t}</span>
                                                                        ))}
                                                                    </div>
                                                                )}
                                                                {isSelected && <CircleCheck size={14} className="text-(--accent-light) shrink-0" />}
                                                            </button>
                                                        );
                                                    })
                                                )}
                                            </div>
                                        </>
                                    )}

                                    {targetMode === 'tag' && (
                                        <>
                                            {availableTags.length === 0 ? (
                                                <div className="flex-1 flex items-center justify-center rounded-md border border-dashed border-(--base-04) bg-(--base-02) p-6 text-center">
                                                    <p className="text-xs text-(--base-06)">
                                                        {selectedRegion
                                                            ? <>No tags in region <span className="font-mono">{regionLabelFrom(selectedRegion, regions)}</span>.</>
                                                            : <>No tags advertised by any online node.<br/>Tag nodes in <span className="font-mono">Settings → Nodes</span> first.</>}
                                                    </p>
                                                </div>
                                            ) : (
                                                <>
                                                    <div className="flex flex-wrap gap-1.5">
                                                        {availableTags.map(t => {
                                                            const active = selectedTags.includes(t);
                                                            return (
                                                                <button
                                                                    key={t}
                                                                    type="button"
                                                                    onClick={() => setSelectedTags(prev =>
                                                                        prev.includes(t) ? prev.filter(x => x !== t) : [...prev, t]
                                                                    )}
                                                                    className={`px-2.5 py-1 rounded-md text-xs font-medium transition-colors border ${
                                                                        active
                                                                            ? 'bg-(--accent-ghost) border-(--accent-border) text-(--accent-light)'
                                                                            : 'bg-(--base-02) border-(--base-03) text-(--base-07) hover:text-(--base-09)'
                                                                    }`}
                                                                >
                                                                    {active && <CircleCheck size={11} className="inline mr-1 -mt-0.5" />}
                                                                    {t}
                                                                </button>
                                                            );
                                                        })}
                                                    </div>
                                                    <p className="mono-label">
                                                        {selectedTags.length === 0
                                                            ? 'any tag — all nodes in the region are candidates'
                                                            : `${selectedTags.length} selected — node must have all of them (AND)`}
                                                    </p>
                                                    <div className="flex-1 rounded-md border border-(--base-03) bg-(--base-02) p-3 text-sm">
                                                        {tagPreview ? (
                                                            <div className="space-y-2">
                                                                <div className="flex items-center gap-2 text-(--base-09)">
                                                                    <CircleCheck size={14} className="text-(--success-light)" />
                                                                    <span className="font-medium">Will be placed on:</span>
                                                                </div>
                                                                <div className="font-mono text-base text-(--accent-light)">{tagPreview.nodeName}</div>
                                                                <div className="text-xs text-(--base-06)">{tagPreviewReason}</div>
                                                                <div className="grid grid-cols-2 gap-2 pt-2 border-t border-(--base-03) text-xs">
                                                                    <div>
                                                                        <span className="text-(--base-06)">RAM </span>
                                                                        <span className="font-mono text-(--base-08)">{tagPreview.allocRamMb} / {(tagPreview.totalRamMb * tagPreview.overcommitRam).toFixed(0)} MB</span>
                                                                    </div>
                                                                    <div>
                                                                        <span className="text-(--base-06)">CPU </span>
                                                                        <span className="font-mono text-(--base-08)">{tagPreview.allocCpu.toFixed(1)} / {(tagPreview.totalCpu * tagPreview.overcommitCpu).toFixed(1)} cores</span>
                                                                    </div>
                                                                </div>
                                                            </div>
                                                        ) : (
                                                            <div className="flex items-center gap-2 text-xs text-(--warning-light)">
                                                                <Info size={12} />
                                                                {tagPreviewReason || 'Computing placement…'}
                                                            </div>
                                                        )}
                                                    </div>
                                                </>
                                            )}
                                        </>
                                    )}
                                </section>
                                </div>
                            </div>
                        )}

                        {step === 2 && (
                            <div className="space-y-6 animate-fade-in">
                                {/* Server Type */}
                                <section className="space-y-3">
                                    <h3 className="text-base font-display font-bold text-(--base-09) border-b border-(--base-03) pb-2">Server Type</h3>
                                    <div className={`grid gap-3 ${proxiesEnabled ? 'grid-cols-2' : 'grid-cols-1'}`}>
                                        <button
                                            type="button"
                                            onClick={() => setServerType('game')}
                                            className={`card text-left p-4 cursor-pointer transition-all flex items-center gap-3 ${
                                                serverType === 'game'
                                                    ? 'border-(--accent-border) bg-(--accent-ghost)'
                                                    : 'hover:border-(--base-05)'
                                            }`}
                                        >
                                            <div className="w-10 h-10 rounded-md bg-(--base-03) flex items-center justify-center">
                                                <Server size={20} className="text-(--primary-light)" />
                                            </div>
                                            <div>
                                                <div className="font-medium text-sm text-(--base-09)">Game Server</div>
                                                <div className="text-xs text-(--base-06)">Minecraft, Paper, Forge, etc.</div>
                                            </div>
                                            {serverType === 'game' && <CircleCheck size={16} className="text-(--accent-light) ml-auto" />}
                                        </button>
                                        {proxiesEnabled && (
                                            <button
                                                type="button"
                                                onClick={() => setServerType('proxy')}
                                                className={`card text-left p-4 cursor-pointer transition-all flex items-center gap-3 ${
                                                    serverType === 'proxy'
                                                        ? 'border-(--accent-border) bg-(--accent-ghost)'
                                                        : 'hover:border-(--base-05)'
                                                }`}
                                            >
                                                <div className="w-10 h-10 rounded-md bg-(--base-03) flex items-center justify-center">
                                                    <Network size={20} className="text-(--accent-light)" />
                                                </div>
                                                <div>
                                                    <div className="font-medium text-sm text-(--base-09)">Proxy Server</div>
                                                    <div className="text-xs text-(--base-06)">BungeeCord, Waterfall, Velocity</div>
                                                </div>
                                                {serverType === 'proxy' && <CircleCheck size={16} className="text-(--accent-light) ml-auto" />}
                                            </button>
                                        )}
                                    </div>
                                </section>

                                {/* Resources */}
                                <section className="space-y-4">
                                    <h3 className="text-base font-display font-bold text-(--base-09) border-b border-(--base-03) pb-2">Resources</h3>

                                    <div className="grid grid-cols-3 gap-4">
                                        <div className="flex flex-col gap-[5px]">
                                            <label className="input-label">RAM Limit (MB)</label>
                                            <div className="relative">
                                                <input
                                                    type="number"
                                                    value={ram}
                                                    onChange={e => setRam(Number(e.target.value))}
                                                    className="input-field w-full [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                                                />
                                                <span className="absolute right-3 top-[9px] text-(--base-06) font-mono text-sm pointer-events-none">MB</span>
                                            </div>
                                            <p className="text-xs text-(--base-06)">+512 MB OOM buffer added automatically</p>
                                        </div>
                                        <div className="flex flex-col gap-[5px]">
                                            <label className="input-label">CPU Limit (Cores)</label>
                                            <div className="relative">
                                                <input
                                                    type="number"
                                                    step="0.5"
                                                    value={cpuLimit}
                                                    onChange={e => setCpuLimit(Number(e.target.value))}
                                                    className="input-field w-full [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                                                />
                                                <span className="absolute right-3 top-[9px] text-(--base-06) font-mono text-sm pointer-events-none">Cores</span>
                                            </div>
                                            <p className="text-xs text-(--base-06)">0 = no limit</p>
                                        </div>
                                        <div className="flex flex-col gap-[5px]">
                                            <label className="input-label">Storage Limit (GB)</label>
                                            <div className="relative">
                                                <input
                                                    type="number"
                                                    min={0}
                                                    step={1}
                                                    value={diskLimit}
                                                    onChange={e => setDiskLimit(Number(e.target.value))}
                                                    className="input-field w-full [appearance:textfield] [&::-webkit-outer-spin-button]:appearance-none [&::-webkit-inner-spin-button]:appearance-none"
                                                />
                                                <span className="absolute right-3 top-[9px] text-(--base-06) font-mono text-sm pointer-events-none">GB</span>
                                            </div>
                                            <p className="text-xs text-(--base-06)">0 = unlimited</p>
                                        </div>
                                    </div>

                                    {/* Storage Path Selection */}
                                    {storagePaths.length > 1 && (
                                        <div className="flex flex-col gap-[5px]">
                                            <label className="input-label flex items-center gap-1.5">
                                                <HardDrive size={13} /> Storage Path
                                            </label>
                                            <select
                                                value={storagePath}
                                                onChange={e => setStoragePath(e.target.value)}
                                                className="input-field"
                                            >
                                                <option value="auto">Auto (most free space)</option>
                                                {storagePaths.map(s => (
                                                    <option key={s.path} value={s.path}>
                                                        {s.path} ({fmtGB(s.free_bytes)} GB free, {s.server_count} servers)
                                                    </option>
                                                ))}
                                            </select>
                                            <p className="text-xs text-(--base-06)">Auto distributes servers evenly across storage paths.</p>
                                        </div>
                                    )}
                                </section>

                                {/* CPU pinning. The node is only known in node-target
                                    mode; in tag mode the grid is hidden and Shared/Auto
                                    still work (Manual falls back to a raw cpuset input). */}
                                <section className="space-y-3">
                                    <h3 className="text-base font-display font-bold text-(--base-09) border-b border-(--base-03) pb-2 flex items-center gap-2">
                                        <Cpu size={16} className="text-(--accent-light)" /> CPU Pinning
                                    </h3>
                                    <CpuPinningControl
                                        nodeId={targetMode === 'node' ? Number(nodeId) || undefined : undefined}
                                        mode={cpuMode}
                                        cpuset={cpuset}
                                        cpuLimit={cpuLimit}
                                        onChange={({ mode, cpuset: cs }) => { setCpuMode(mode); setCpuset(cs); }}
                                    />
                                </section>

                                <section className="card p-3 flex items-center justify-between gap-4">
                                    <div className="flex items-start gap-2.5 min-w-0">
                                        <Move size={16} className={`shrink-0 mt-0.5 ${autoMove ? 'text-(--accent-light)' : 'text-(--base-06)'}`} />
                                        <div className="min-w-0">
                                            <div className="text-sm font-medium text-(--base-09)">Auto-Move enabled</div>
                                            <p className="text-xs text-(--base-06)">
                                                Allow the rebalance worker to migrate this server to a less-loaded node when the current node is overloaded. Migration only runs while the server is stopped/idle. Default off.
                                            </p>
                                        </div>
                                    </div>
                                    <button
                                        type="button"
                                        role="switch"
                                        aria-checked={autoMove}
                                        onClick={() => setAutoMove(v => !v)}
                                        className={`shrink-0 toggle-track ${autoMove ? 'toggle-track-on' : 'toggle-track-off'}`}
                                    >
                                        <span className={`toggle-knob ${autoMove ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                                    </button>
                                </section>

                                <div className="alert alert-info text-(--base-07)">
                                    <Info size={14} className="inline align-middle mr-1 text-(--primary-light)" />
                                    Java version, Minecraft software, and server name are configured in the Setup tab after deployment.
                                </div>
                            </div>
                        )}
                    </form>
                    )}
                </div>

                {formError && (
                    <div className="px-6 pb-2">
                        <p role="alert" className="text-sm text-(--error)">{formError}</p>
                    </div>
                )}

                {/* Footer */}
                <div className="modal-footer">
                    {showNoNodeMsg ? (
                        <>
                            <button onClick={onClose} className="btn btn-secondary">Close</button>
                            {/* The one place a customer reaches with the intent to spend money used
                                to dead-end on Close alone. /nodes is the correct destination for all
                                three ways to land here - no subscription, subscribed but nothing
                                deployed yet, or a node that is offline - and it already carries the
                                store link, so this needs no second opinion about which to offer. */}
                            <Link href="/nodes" onClick={onClose} className="btn btn-primary inline-flex items-center gap-1.5">
                                Add a node <ArrowRight size={14} />
                            </Link>
                        </>
                    ) : (
                    <>
                    {step > 1 ? (
                        <button onClick={() => setStep(step - 1)} className="btn btn-secondary">Back</button>
                    ) : (
                        <div></div>
                    )}

                    {step < 2 ? (
                        <button
                            onClick={() => {
                                setFormError(null);
                                if (!ownerId) return setFormError("Please assign an owner.");
                                if (targetMode === 'node' && !nodeId) return setFormError("Please select a node.");
                                if (targetMode === 'tag' && selectedTags.length === 0 && !selectedRegion) {
                                    return setFormError("Please select a region or at least one tag.");
                                }
                                setStep(2);
                            }}
                            className="btn btn-primary"
                        >
                            Next <ArrowRight size={18} className="ml-1" />
                        </button>
                    ) : (
                        <button
                            onClick={handleCreate}
                            disabled={loading}
                            className="btn bg-(--success) text-white border-(--success) hover:bg-(--success-light) disabled:opacity-40"
                        >
                            {loading ? 'Creating...' : 'CREATE SERVER'} <Rocket size={18} className="ml-1" />
                        </button>
                    )}
                    </>
                    )}
                </div>
            </div>
        </div>
    );
}
