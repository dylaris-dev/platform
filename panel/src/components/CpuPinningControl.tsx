"use client";

import React, { useState, useEffect } from 'react';
import { Layers, Wand2, Grid3x3 } from 'lucide-react';
import { getNodeCpu, type NodeCpuTopology } from '@/lib/api';
import { parseCpuset, compactCpuset, pinMismatch } from '@/lib/cpuset';
import { groupCoresByCache, formatL3, formatClock } from '@/lib/cpuTopology';

interface CpuPinningControlProps {
    nodeId?: number;            // node to fetch topology for; undefined => grid hidden (e.g. tag-targeted create)
    serverId?: number;          // the server being edited: its own pin is left out of the suggestion
    mode: 'shared' | 'auto' | 'manual';
    cpuset: string;             // current manual cpuset string
    cpuLimit?: number;          // used only for an informational hint
    onChange: (next: { mode: 'shared' | 'auto' | 'manual'; cpuset: string }) => void;
    disabled?: boolean;
}

const MODES: { id: 'shared' | 'auto' | 'manual'; label: string; icon: React.ElementType; hint: string }[] = [
    { id: 'shared', label: 'Shared', icon: Layers, hint: "Uses all of the node's cores within the CPU limit." },
    { id: 'auto', label: 'Auto', icon: Wand2, hint: 'Placed automatically: one physical core per server while free ones are left, performance cores first.' },
    { id: 'manual', label: 'Manual', icon: Grid3x3, hint: 'Pick the exact cores this server runs on, as many as its CPU limit.' },
];

export default function CpuPinningControl({ nodeId, serverId, mode, cpuset, cpuLimit, onChange, disabled }: CpuPinningControlProps) {
    const [topology, setTopology] = useState<NodeCpuTopology | null>(null);
    const [load, setLoad] = useState<Record<string, number>>({});
    // The node's allowed core pool. "" means all cores are allowed (no pool).
    const [nodeCpuset, setNodeCpuset] = useState('');
    // The cores Core's allocator would pick for this limit: the spread auto
    // mode uses, offered as the starting point for a manual pin.
    const [suggested, setSuggested] = useState('');
    const [loaded, setLoaded] = useState(false);

    useEffect(() => {
        if (nodeId === undefined) {
            setTopology(null);
            setLoad({});
            setNodeCpuset('');
            setSuggested('');
            setLoaded(false);
            return;
        }
        let cancelled = false;
        getNodeCpu(nodeId, cpuLimit && cpuLimit > 0 ? { limit: cpuLimit, serverId } : undefined).then(res => {
            if (cancelled) return;
            if (res?.success) {
                setTopology(res.topology ?? null);
                setLoad(res.load ?? {});
                setNodeCpuset(res.nodeCpuset ?? '');
                setSuggested(typeof res.suggested === 'string' ? res.suggested : '');
            } else {
                setTopology(null);
                setLoad({});
                setNodeCpuset('');
            }
            setLoaded(true);
        }).catch(() => {
            if (cancelled) return;
            setTopology(null);
            setLoad({});
            setNodeCpuset('');
            setLoaded(true);
        });
        return () => { cancelled = true; };
    }, [nodeId, serverId, cpuLimit]);

    const selected = new Set(parseCpuset(cpuset));
    // Allowed core pool for this node. When empty, every core is selectable.
    const allowed = nodeCpuset ? new Set(parseCpuset(nodeCpuset)) : null;

    const toggleCore = (id: number) => {
        if (disabled) return;
        if (allowed && !allowed.has(id)) return; // outside the node's core pool
        const next = new Set(selected);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        onChange({ mode: 'manual', cpuset: compactCpuset([...next]) });
    };

    const setMode = (next: 'shared' | 'auto' | 'manual') => {
        if (disabled) return;
        if (next === 'shared') onChange({ mode: 'shared', cpuset: '' });
        else if (next === 'auto') onChange({ mode: 'auto', cpuset: '' });
        else onChange({ mode: 'manual', cpuset }); // keep current cpuset
    };

    const activeHint = MODES.find(m => m.id === mode)?.hint ?? '';
    // Pool availability: how many cores this node offers (its pool, or all cores)
    // and how many already carry at least one pinned server.
    const poolSize = allowed ? allowed.size : (topology?.logicalCount ?? 0);
    const assignedCount = topology
        ? topology.cores.filter(c => (!allowed || allowed.has(c.id)) && (load[String(c.id)] ?? 0) > 0).length
        : 0;

    // Cache-domain grouping for the manual grid. Empty when no topology; a single
    // synthetic group when the node reported no cache data (renders as a flat grid).
    const cacheGroups = topology ? groupCoresByCache(topology.cores) : [];

    return (
        <div className="flex flex-col gap-2">
            <div className="flex items-center justify-between gap-3">
                <label className="input-label mb-0">CPU pinning</label>
                <div className="flex bg-(--base-03) p-0.5 rounded-md">
                    {MODES.map(({ id, label, icon: Icon }) => (
                        <button
                            key={id}
                            type="button"
                            disabled={disabled}
                            onClick={() => setMode(id)}
                            className={`px-2.5 py-1 text-[11px] rounded-sm transition-colors inline-flex items-center gap-1 disabled:opacity-40 disabled:cursor-not-allowed ${
                                mode === id ? 'bg-(--accent) text-white' : 'text-(--base-07) hover:text-(--base-09)'
                            }`}
                        >
                            <Icon size={11} /> {label}
                        </button>
                    ))}
                </div>
            </div>

            <p className="text-[11px] text-(--base-06)">{activeHint}</p>

            {nodeId !== undefined && topology && (
                <p className="text-[11px] text-(--base-06)">
                    Pool: {poolSize} core{poolSize === 1 ? '' : 's'} available
                    {nodeCpuset ? ` (restricted to ${nodeCpuset})` : ''}, {assignedCount} already in use by other servers.
                </p>
            )}

            {mode === 'manual' && nodeId !== undefined && (
                <>
                    {topology ? (
                        <div className="flex flex-col gap-2">
                            <div className="flex flex-col gap-3">
                                {cacheGroups.map(group => (
                                    <div key={group.cacheGroup} className="flex flex-col gap-1.5">
                                        {cacheGroups.length > 1 && (
                                            <div className="flex items-center gap-2">
                                                <span className="font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-06)">
                                                    L3 {formatL3(group.l3KB)}
                                                </span>
                                                {group.isVCache && (
                                                    <span className="font-mono text-[9px] uppercase tracking-[0.08em] px-1.5 py-0.5 rounded-sm bg-(--accent-ghost) text-(--accent-light)">
                                                        3D V-Cache
                                                    </span>
                                                )}
                                            </div>
                                        )}
                                        <div className="flex flex-wrap gap-1.5">
                                            {group.cores.map(core => {
                                                const isSel = selected.has(core.id);
                                                const coreLoad = load[String(core.id)] ?? 0;
                                                const inPool = !allowed || allowed.has(core.id);
                                                const clock = formatClock(core.maxClockMHz);
                                                return (
                                                    <button
                                                        key={core.id}
                                                        type="button"
                                                        disabled={disabled || !inPool}
                                                        onClick={() => toggleCore(core.id)}
                                                        title={inPool
                                                            ? `Core ${core.id}${topology.hybrid ? ` (${core.type === 'P' ? 'Performance' : core.type === 'E' ? 'Efficiency' : 'standard'})` : ''}${clock ? ` · ${clock}Hz` : ''}${coreLoad > 0 ? ` · ${coreLoad} pinned` : ''}`
                                                            : "Outside this node's core pool"}
                                                        className={`relative w-12 h-12 rounded-md border flex flex-col items-center justify-center transition-colors disabled:cursor-not-allowed ${
                                                            !inPool
                                                                ? 'border-(--base-04) text-(--base-07) opacity-40 cursor-not-allowed'
                                                                : isSel
                                                                ? 'border-(--accent) bg-(--accent-ghost) text-(--accent-light)'
                                                                : 'border-(--base-04) text-(--base-07) hover:border-(--base-05) hover:text-(--base-09)'
                                                        } disabled:opacity-40`}
                                                    >
                                                        <span className="font-mono text-sm leading-none">{core.id}</span>
                                                        {topology.hybrid && core.type !== 'standard' && (
                                                            <span className={`font-mono text-[9px] leading-none mt-0.5 ${core.type === 'P' ? 'text-(--accent-light)' : 'text-(--base-06)'}`}>
                                                                {core.type}
                                                            </span>
                                                        )}
                                                        {clock && (
                                                            <span className="font-mono text-[8px] leading-none mt-0.5 text-(--base-06)">
                                                                {clock}
                                                            </span>
                                                        )}
                                                        {coreLoad > 0 && (
                                                            <span className="absolute top-0.5 right-1 font-mono text-[9px] text-(--base-06)" title={`${coreLoad} server(s) pinned here`}>
                                                                x{coreLoad}
                                                            </span>
                                                        )}
                                                    </button>
                                                );
                                            })}
                                        </div>
                                    </div>
                                ))}
                            </div>
                            <p className="text-[11px] text-(--base-06)">
                                {selected.size === 0
                                    ? 'No cores selected. Pick at least one.'
                                    : `${selected.size} core${selected.size === 1 ? '' : 's'} selected: ${compactCpuset([...selected])}`}
                                {cpuLimit !== undefined && cpuLimit > 0 && (
                                    <span className="ml-1">CPU limit is {cpuLimit} core{cpuLimit === 1 ? '' : 's'}.</span>
                                )}
                            </p>
                            {suggested && suggested !== compactCpuset([...selected]) && (
                                <div className="flex items-center gap-2 text-[11px] text-(--base-06)">
                                    <span>Best free cores for this limit: <span className="font-mono text-(--base-08)">{suggested}</span></span>
                                    <button
                                        type="button"
                                        disabled={disabled}
                                        onClick={() => onChange({ mode: 'manual', cpuset: suggested })}
                                        className="btn btn-secondary px-2 py-0.5 text-[11px] disabled:opacity-40 disabled:cursor-not-allowed"
                                    >
                                        Use
                                    </button>
                                </div>
                            )}
                            {(() => {
                                const off = pinMismatch(cpuLimit, 'manual', compactCpuset([...selected]));
                                return off && (
                                    <p role="alert" className="text-[11px] text-(--error-light)">
                                        The server gets {cpuLimit} CPU core{cpuLimit === 1 ? '' : 's'} but would be pinned to {off.have}. Pin exactly {off.need} core{off.need === 1 ? '' : 's'}, or set the CPU limit to {off.have}.
                                    </p>
                                );
                            })()}
                            {nodeCpuset && (
                                <p className="text-[11px] text-(--base-06)">Node core pool: {nodeCpuset}</p>
                            )}
                        </div>
                    ) : loaded ? (
                        <div className="flex flex-col gap-1.5">
                            <p className="text-[11px] text-(--base-06)">This node has not reported its CPU layout yet.</p>
                            <input
                                type="text"
                                value={cpuset}
                                disabled={disabled}
                                onChange={e => onChange({ mode: 'manual', cpuset: e.target.value })}
                                placeholder="e.g. 0-3,8 (Linux cpuset list)"
                                className="input-field input-mono w-full"
                            />
                        </div>
                    ) : (
                        <p className="text-[11px] text-(--base-06)">Loading CPU layout…</p>
                    )}
                </>
            )}

            {mode === 'manual' && nodeId === undefined && (
                <div className="flex flex-col gap-1.5">
                    <p className="text-[11px] text-(--base-06)">No specific node selected. Type a raw cpuset list.</p>
                    <input
                        type="text"
                        value={cpuset}
                        disabled={disabled}
                        onChange={e => onChange({ mode: 'manual', cpuset: e.target.value })}
                        placeholder="e.g. 0-3,8 (Linux cpuset list)"
                        className="input-field input-mono w-full"
                    />
                </div>
            )}
        </div>
    );
}
