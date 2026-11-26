"use client";

import React, { useState, useEffect, useMemo } from 'react';
import Link from 'next/link';
import {
    getUsers, User, createServer, getNodes, Node,
    getAvailableTags, pickNode, NodeCandidate,
    updateServerResources, API_URL, getPlacementSettings,
} from '../lib/api';
import { X, Server, CircleCheck, Info, ArrowRight, Rocket, Network, HardDrive, Tag as TagIcon, Move, Cpu, ChevronDown } from 'lucide-react';
import CpuPinningControl from './CpuPinningControl';
import { useAppData } from '@/lib/AppDataContext';
import { sortUsersForPicker } from '@/lib/userOrder';
import { nodeLabel } from '@/lib/nodeLabel';
import ModalPanel from '@/components/ui/ModalPanel';
import ResourceSlider from '@/components/ResourceSlider';
import { DISK_SCALE_GB, cpuScale, ramScale } from '@/lib/resourceSlider';
import { formatMB } from '@/lib/format';
import { MAX_RAM_PADDING_MB, RAM_PADDING_HELP, inheritedRamPadding, parseRamPaddingInput } from '@/lib/ramPadding';
import { pinMismatch } from '@/lib/cpuset';
import Select from '@/components/ui/Select';
import Switch from '@/components/ui/Switch';
import Segmented from '@/components/ui/Segmented';
import NumberField from '@/components/ui/NumberField';
import EmptyState from '@/components/ui/EmptyState';
import RadioCards from '@/components/settings/RadioCards';

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
    // Admins get the full scheduling wizard (assign owner, any node, tags).
    // A BYON tenant gets a slimmed flow: owner is themselves, and only
    // their own nodes are selectable. The 'placement' scope is what decides
    // that, for both: it offers unowned machines to an operator and an owned one
    // only to its owner, so the picker cannot suggest a target the create call
    // would refuse.
    const { user, featureFlags } = useAppData();
    const hosted = featureFlags.hosted;
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
    // RAM headroom override as typed; empty = the node's default. The global
    // default is only readable with settings.read, so tenants may not know it.
    const [paddingText, setPaddingText] = useState('');
    const [globalPaddingMb, setGlobalPaddingMb] = useState<number | undefined>(undefined);
    const [advancedOpen, setAdvancedOpen] = useState(false);

    useEffect(() => {
        if (!isOpen) return;
        setStep(1);
        setNodeId(""); setOwnerId(null); setServerType('game'); setRam(2048); setCpuLimit(0); setDiskLimit(20);
        setSearchTerm(""); setNodeSearch("");
        setTargetMode('node'); setSelectedTags([]); setTagPreview(null); setTagPreviewReason('');
        setAutoMove(false);
        setCpuMode('shared'); setCpuset('');
        setPaddingText(''); setAdvancedOpen(false);
        setNodesLoaded(false);

        // Owner assignment + tag scheduling are admin-only. A tenant owns
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
            getPlacementSettings().then(res => {
                if (res.success && typeof res.settings?.ramPaddingMb === 'number') setGlobalPaddingMb(res.settings.ramPaddingMb);
            }).catch(() => { /* non-fatal: the hint just names no number */ });
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

    // Admin-only: tenants never see the tag scheduler.
    useEffect(() => {
        if (!isOpen || !isAdmin) return;
        getAvailableTags().then(res => {
            if (res.success) {
                const tags = res.tags || [];
                setAvailableTags(tags);
                // Drop any selected tags that are no longer in the pool.
                setSelectedTags(prev => prev.filter(t => tags.includes(t)));
            }
        });
    }, [isOpen, isAdmin]);

    // Scheduler preview: ask which node would be picked whenever the tags
    // or the resource shape change. Surfaces the choice + reason in the
    // wizard so admins aren't deploying blind.
    useEffect(() => {
        if (targetMode !== 'tag') {
            setTagPreview(null);
            setTagPreviewReason('');
            return;
        }
        let cancelled = false;
        // Same override the create sends, so preview and create agree.
        const parsed = parseRamPaddingInput(paddingText);
        const previewPadding = parsed !== null && !Number.isNaN(parsed) ? parsed : null;
        // A slider drag is a burst of changes; preview the value it settles on.
        const timer = setTimeout(() => pickNode({
            tags: selectedTags.length > 0 ? selectedTags : undefined,
            ramMb: ram,
            cpuCores: cpuLimit,
            diskGb: diskLimit,
            ...(previewPadding !== null ? { ramPaddingMb: previewPadding } : {}),
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
        }), 300);
        return () => { cancelled = true; clearTimeout(timer); };
    }, [targetMode, selectedTags, ram, cpuLimit, diskLimit, paddingText]);

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

    // The node the server would land on: picked, or the scheduler's preview.
    const targetNode = targetMode === 'node'
        ? nodes.find(n => String(n.id) === nodeId)
        : nodes.find(n => n.id === tagPreview?.nodeId);
    // Only a node the user picked bounds the sliders: in tag mode the preview
    // node follows the RAM being dragged, and would move the scale mid-drag.
    const pickedNode = targetMode === 'node' ? targetNode : undefined;
    const defaultPadding = targetNode?.ramPaddingMb != null || globalPaddingMb !== undefined
        ? inheritedRamPadding(targetNode?.ramPaddingMb, globalPaddingMb)
        : null;
    const paddingOverride = parseRamPaddingInput(paddingText);

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
        if (Number.isNaN(paddingOverride)) {
            fail(`RAM headroom must be a whole number of MB from 0 to ${MAX_RAM_PADDING_MB}, or empty for the node's default.`);
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
            docker: {
                ram, cpuLimit, diskLimit: diskLimit > 0 ? diskLimit * 1024 : 0,
                ...(paddingOverride !== null ? { ramPaddingMb: paddingOverride } : {}),
            },
        };
        if (targetMode === 'node') {
            payload.nodeId = nodeId;
        } else if (selectedTags.length > 0) {
            payload.tags = selectedTags;
        }
        // No nodeId and no tags: Core auto-places across every eligible node.
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
            <ModalPanel className="modal-panel w-full max-w-4xl flex flex-col max-h-[90vh]">

                {/* Header */}
                <div className="flex items-start justify-between gap-4 px-6 pt-6">
                    <div>
                        <h2 className="modal-title">Create server</h2>
                        <p className="srv-label mt-1">Step {step} of 2. Software setup happens after deployment.</p>
                    </div>
                    <button type="button" onClick={onClose} aria-label="Close" className="app-icon-btn">
                        <X size={16} />
                    </button>
                </div>

                {/* Progress */}
                <div className="mx-6 mt-4 srv-meter-track" role="progressbar" aria-label="Create server progress" aria-valuemin={0} aria-valuemax={2} aria-valuenow={step}>
                    <div className="srv-meter-fill" style={{ width: `${step * 50}%`, background: 'var(--accent)' }} />
                </div>

                {/* Body */}
                <div className="modal-body overflow-y-auto flex-1 p-6">
                    {showNoNodeMsg ? (
                        <EmptyState
                            bare
                            icon={<Server size={20} />}
                            title="No nodes yet"
                            description={hosted
                                ? 'To host a server you need your own node. Adding a node requires a subscription. Renting a managed server is coming soon.'
                                : 'No node is online. Ask an administrator to add one.'}
                        />
                    ) : (
                    <form onSubmit={(e) => e.preventDefault()}>

                        {step === 1 && (
                            <div className="space-y-5 animate-fade-in">
                                <div className={`grid grid-cols-1 gap-6 ${isAdmin ? 'md:grid-cols-2' : ''}`}>
                                {/* Left: Owner (admin assigns; a tenant owns what they create) */}
                                {isAdmin && (
                                <section className="space-y-2 min-h-80 flex flex-col">
                                    <div className="flex items-center justify-between pb-1 min-h-9">
                                        <h3 className="text-[15px] font-semibold text-(--base-09)">Assign owner</h3>
                                        <span className="srv-num text-xs text-(--base-06)">
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
                                    <div className="flex-1 flex flex-col gap-0.5 overflow-y-auto rounded-lg bg-(--base-01) p-1.5">
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
                                                                ? 'bg-(--base-03) [box-shadow:inset_0_0_0_1px_var(--accent)]'
                                                                : 'hover:bg-(--base-03)'
                                                        }`}
                                                    >
                                                        <div className="w-7 h-7 rounded-md bg-(--base-02) flex items-center justify-center font-medium text-xs text-(--base-08) shrink-0">
                                                            {user.username.charAt(0).toUpperCase()}
                                                        </div>
                                                        <span className="flex-1 truncate text-sm text-(--base-09)">{user.username}</span>
                                                        {user.isAdmin && (
                                                            <span className="srv-chip h-5 px-2 text-[11px]">Admin</span>
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
                                    <div className="flex items-center justify-between pb-1 min-h-9">
                                        <h3 className="text-[15px] font-semibold text-(--base-09)">Target</h3>
                                        {isAdmin && (
                                        <Segmented
                                            ariaLabel="Placement"
                                            value={targetMode}
                                            onChange={setTargetMode}
                                            options={[{ id: 'node', label: 'Node' }, { id: 'tag', label: 'Tag' }]}
                                        />
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
                                            <div className="flex-1 flex flex-col gap-0.5 overflow-y-auto rounded-lg bg-(--base-01) p-1.5">
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
                                                                        ? 'bg-(--base-03) [box-shadow:inset_0_0_0_1px_var(--accent)]'
                                                                        : 'hover:bg-(--base-03)'
                                                                }`}
                                                            >
                                                                <div className="w-7 h-7 rounded-md bg-(--base-02) flex items-center justify-center shrink-0">
                                                                    <Server size={14} className="text-(--base-07)" />
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
                                                <div className="flex-1 flex items-center justify-center rounded-lg bg-(--base-01) p-6 text-center">
                                                    <p className="text-xs text-(--base-06)">
                                                        No tags advertised by any online node.<br/>The server is placed automatically on any eligible node.
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
                                                                    aria-pressed={active}
                                                                    className={`srv-chip h-7 cursor-pointer hover:bg-(--base-03) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] ${
                                                                        active ? 'bg-(--base-03) text-(--accent-light) [box-shadow:inset_0_0_0_1px_var(--accent)]' : ''
                                                                    }`}
                                                                >
                                                                    {active && <CircleCheck size={11} className="inline mr-1 -mt-0.5" />}
                                                                    {t}
                                                                </button>
                                                            );
                                                        })}
                                                    </div>
                                                    <p className="srv-label">
                                                        {selectedTags.length === 0
                                                            ? 'any tag - every eligible node is a candidate'
                                                            : `${selectedTags.length} selected — node must have all of them (AND)`}
                                                    </p>
                                                    <div className="flex-1 rounded-lg bg-(--base-01) p-3 text-sm">
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
                                    <h3 className="text-[15px] font-semibold text-(--base-09)">Server type</h3>
                                    <RadioCards
                                        ariaLabel="Server type"
                                        value={serverType}
                                        onChange={setServerType}
                                        className={`grid gap-3 ${proxiesEnabled ? 'grid-cols-2' : 'grid-cols-1'}`}
                                        cardClassName={checked => `flex cursor-pointer items-center gap-3 rounded-lg bg-(--base-01) p-4 transition-colors hover:bg-(--base-03) ${checked ? '[box-shadow:inset_0_0_0_1px_var(--accent)]' : ''}`}
                                        options={[
                                            { value: 'game', label: 'Game server', description: 'Minecraft, Paper, Forge, etc.' },
                                            ...(proxiesEnabled ? [{ value: 'proxy' as const, label: 'Proxy server', description: 'BungeeCord, Waterfall, Velocity' }] : []),
                                        ]}
                                    />
                                </section>

                                {/* Resources */}
                                <section className="space-y-4">
                                    <h3 className="text-[15px] font-semibold text-(--base-09)">Resources</h3>

                                    <div className="grid grid-cols-3 gap-4">
                                        <div className="flex flex-col gap-[5px]">
                                            <NumberField label="RAM limit (MB)" value={ram} onChange={setRam} min={0} />
                                            <ResourceSlider label="RAM" value={ram} onChange={setRam} scale={ramScale(pickedNode?.totalRamMb)} format={formatMB} />
                                            <p className="text-xs text-(--base-06)">
                                                {paddingOverride !== null && !Number.isNaN(paddingOverride)
                                                    ? `+${paddingOverride} MB RAM headroom (set below)`
                                                    : defaultPadding
                                                        ? `+${defaultPadding.value} MB RAM headroom (${defaultPadding.source} default)`
                                                        : "+ the node's default RAM headroom"}
                                            </p>
                                        </div>
                                        <div className="flex flex-col gap-[5px]">
                                            <NumberField label="CPU limit (cores)" value={cpuLimit} onChange={setCpuLimit} min={0} step={0.5} />
                                            <ResourceSlider label="CPU limit" value={cpuLimit} onChange={setCpuLimit} scale={cpuScale(pickedNode?.totalCpu)} format={v => `${v} cores`} />
                                            <p className="text-xs text-(--base-06)">0 = no limit</p>
                                        </div>
                                        <div className="flex flex-col gap-[5px]">
                                            <NumberField label="Storage limit (GB)" value={diskLimit} onChange={setDiskLimit} min={0} step={1} />
                                            <ResourceSlider label="Storage limit" value={diskLimit} onChange={setDiskLimit} scale={DISK_SCALE_GB} format={v => `${v} GB`} />
                                            <p className="text-xs text-(--base-06)">0 = unlimited</p>
                                        </div>
                                    </div>

                                    <div>
                                        <button type="button" onClick={() => setAdvancedOpen(o => !o)} aria-expanded={advancedOpen} className="flex items-center gap-2 text-xs text-(--base-06) hover:text-(--base-08) transition-colors">
                                            <ChevronDown size={13} className={`transition-transform ${advancedOpen ? 'rotate-180' : ''}`} />
                                            Advanced
                                        </button>
                                        {advancedOpen && (
                                            <div className="mt-3 flex flex-col gap-[5px] max-w-xs">
                                                <label htmlFor="create-ram-padding" className="srv-label">RAM headroom (MB)</label>
                                                <input
                                                    id="create-ram-padding"
                                                    type="number"
                                                    min={0}
                                                    max={MAX_RAM_PADDING_MB}
                                                    step={64}
                                                    value={paddingText}
                                                    placeholder={defaultPadding ? String(defaultPadding.value) : 'Node default'}
                                                    onChange={e => setPaddingText(e.target.value)}
                                                    aria-invalid={Number.isNaN(paddingOverride)}
                                                    className="input-field w-full"
                                                />
                                                <p className="text-xs text-(--base-06)">Empty uses the node&apos;s default. {RAM_PADDING_HELP}</p>
                                            </div>
                                        )}
                                    </div>

                                    {/* Storage Path Selection */}
                                    {storagePaths.length > 1 && (
                                        <div className="flex flex-col gap-[5px]">
                                            <label className="srv-label flex items-center gap-1.5">
                                                <HardDrive size={13} /> Storage path
                                            </label>
                                            <Select
                                                value={storagePath}
                                                onChange={setStoragePath}
                                                ariaLabel="Storage path"
                                                options={[
                                                    { value: 'auto', label: 'Auto (most free space)' },
                                                    ...storagePaths.map(s => ({ value: s.path, label: `${s.path} (${fmtGB(s.free_bytes)} GB free, ${s.server_count} servers)` })),
                                                ]}
                                            />
                                            <p className="text-xs text-(--base-06)">Auto distributes servers evenly across storage paths.</p>
                                        </div>
                                    )}
                                </section>

                                {/* CPU pinning. The node is only known in node-target
                                    mode; in tag mode the grid is hidden and Shared/Auto
                                    still work (Manual falls back to a raw cpuset input). */}
                                <section className="space-y-3">
                                    <h3 className="text-[15px] font-semibold text-(--base-09) flex items-center gap-2">
                                        <Cpu size={16} className="text-(--base-06)" /> CPU pinning
                                    </h3>
                                    <CpuPinningControl
                                        nodeId={targetMode === 'node' ? Number(nodeId) || undefined : undefined}
                                        mode={cpuMode}
                                        cpuset={cpuset}
                                        cpuLimit={cpuLimit}
                                        onChange={({ mode, cpuset: cs }) => { setCpuMode(mode); setCpuset(cs); }}
                                    />
                                </section>

                                <section className="srv-surface p-4 flex items-center justify-between gap-4">
                                    <div className="flex items-start gap-2.5 min-w-0">
                                        <Move size={16} className="shrink-0 mt-0.5 text-(--base-06)" />
                                        <div className="min-w-0">
                                            <div className="text-sm font-medium text-(--base-09)">Auto-move enabled</div>
                                            <p className="text-xs text-(--base-06)">
                                                Allow the rebalance worker to migrate this server to a less-loaded node when the current node is overloaded. Migration only runs while the server is stopped/idle. Default off.
                                            </p>
                                        </div>
                                    </div>
                                    <Switch checked={autoMove} onChange={setAutoMove} ariaLabel="Auto-Move enabled" className="shrink-0" />
                                </section>

                                <div className="srv-callout" data-tone="info">
                                    <Info size={14} className="mt-0.5 shrink-0 text-(--accent-light)" />
                                    Java version, Minecraft software, and server name are configured in the Setup tab after deployment.
                                </div>
                            </div>
                        )}
                    </form>
                    )}
                </div>

                {formError && (
                    <div className="px-6 pb-2">
                        <p role="alert" className="text-sm text-(--error-light)">{formError}</p>
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
                                setStep(2);
                            }}
                            className="btn btn-primary"
                        >
                            Next <ArrowRight size={16} />
                        </button>
                    ) : (
                        <button
                            onClick={handleCreate}
                            disabled={loading || !!pinMismatch(cpuLimit, cpuMode, cpuset)}
                            title={pinMismatch(cpuLimit, cpuMode, cpuset) ? 'Pin exactly as many cores as the CPU limit' : undefined}
                            className="btn btn-primary"
                        >
                            {loading ? 'Creating...' : 'Create server'} <Rocket size={16} />
                        </button>
                    )}
                    </>
                    )}
                </div>
            </ModalPanel>
        </div>
    );
}
