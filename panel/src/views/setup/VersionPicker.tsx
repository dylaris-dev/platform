"use client";

import React, { useState } from 'react';
import { createPortal } from 'react-dom';
import { Info } from 'lucide-react';
import { Skeleton } from '@/components/Skeleton';

const SOFTWARE_META: Record<string, { desc: string; url: string }> = {
    paper:      { desc: 'High-performance Spigot fork with major optimizations.',    url: 'https://papermc.io' },
    vanilla:    { desc: 'The official, unmodified Minecraft server.',                url: 'https://www.minecraft.net/download/server' },
    fabric:     { desc: 'Lightweight, modern modding toolchain.',                    url: 'https://fabricmc.net' },
    forge:      { desc: 'The long-established large-mod platform.',                  url: 'https://files.minecraftforge.net' },
    neoforge:   { desc: 'Community-driven fork of Forge.',                           url: 'https://neoforged.net' },
    velocity:   { desc: 'Modern, high-performance Minecraft proxy.',                 url: 'https://papermc.io/software/velocity' },
    waterfall:  { desc: 'Legacy BungeeCord fork (succeeded by Velocity).',           url: 'https://papermc.io' },
    bungeecord: { desc: 'The classic Minecraft proxy.',                              url: 'https://www.spigotmc.org/wiki/bungeecord/' },
};

export interface VersionEntry {
    major: string;
    build: string;
}

interface VersionPickerProps {
    software: string;
    onSoftwareChange: (s: string) => void;
    softwareList?: string[];
    allVersions: VersionEntry[];
    selectedMajor: string;
    onMajorChange: (m: string) => void;
    selectedBuild: string;
    onBuildChange: (b: string) => void;
    loading: boolean;
}

/**
 * Compare two dotted-numeric version strings semantically (e.g. "1.20.2", "48.10.1").
 * Returns a negative number if `a` should sort before `b` (i.e. a > b for descending order).
 * Non-numeric or missing segments are treated as 0, sorting them last / lowest.
 */
// Newest first. The part before the first "-" or "+" compares numerically per
// dot component; a release sorts above its own pre-releases ("1.21.11" over
// "1.21.11-rc3"), and pre-releases compare naturally among themselves
// (alpha < beta < pre < rc, "alpha.10" over "alpha.2"). Before, parseInt read
// "11-rc3" as 11, so a release and its candidates tied and landed in whatever
// order Core's map produced.
export function compareVersionsDesc(a: string, b: string): number {
    const [aBase, aSuffix] = splitVersion(a);
    const [bBase, bSuffix] = splitVersion(b);
    const aParts = aBase.split('.').map(s => { const n = parseInt(s, 10); return isNaN(n) ? 0 : n; });
    const bParts = bBase.split('.').map(s => { const n = parseInt(s, 10); return isNaN(n) ? 0 : n; });
    const len = Math.max(aParts.length, bParts.length);
    for (let i = 0; i < len; i++) {
        const diff = (bParts[i] ?? 0) - (aParts[i] ?? 0);
        if (diff !== 0) return diff;
    }
    if (!aSuffix !== !bSuffix) return aSuffix ? 1 : -1;
    return compareNatural(bSuffix, aSuffix);
}

function splitVersion(v: string): [string, string] {
    const i = v.search(/[-+]/);
    return i < 0 ? [v, ''] : [v.slice(0, i), v.slice(i + 1)];
}

// Ascending; runs of digits compare as numbers, everything else as text.
function compareNatural(a: string, b: string): number {
    const at = a.match(/\d+|\D+/g) ?? [];
    const bt = b.match(/\d+|\D+/g) ?? [];
    for (let i = 0; i < Math.max(at.length, bt.length); i++) {
        const x = at[i], y = bt[i];
        if (x === undefined) return -1;
        if (y === undefined) return 1;
        const nx = /^\d/.test(x), ny = /^\d/.test(y);
        if (nx && ny) {
            const d = Number(x) - Number(y);
            if (d !== 0) return d;
        } else if (x !== y) {
            return x < y ? -1 : 1;
        }
    }
    return 0;
}

export default function VersionPicker({
    software, onSoftwareChange, softwareList,
    allVersions, selectedMajor, onMajorChange, selectedBuild, onBuildChange,
    loading,
}: VersionPickerProps) {
    const [hoveredInfo, setHoveredInfo] = useState<string | null>(null);
    // Tooltip needs to escape the software box's overflow-y-auto, which
    // otherwise clips the popup of the FIRST entry (positioned above
    // its anchor). Anchor coords are captured per-row and the popup
    // renders into document.body via a portal with position: fixed.
    const [tooltipAt, setTooltipAt] = useState<{ x: number; y: number } | null>(null);
    const portalRoot = typeof document !== 'undefined' ? document.body : null;

    const isProxySoftware = ['velocity', 'waterfall', 'bungeecord'].includes(software);
    const availableSoftware = softwareList || (isProxySoftware ? ['velocity', 'waterfall', 'bungeecord'] : ['paper', 'vanilla', 'fabric', 'forge', 'neoforge']);

    const majorVersions = [...new Set(allVersions.map(v => v.major))].sort(compareVersionsDesc);
    const buildsForMajor = selectedMajor
        ? allVersions.filter(v => v.major === selectedMajor).map(v => v.build).sort(compareVersionsDesc)
        : [];

    const versionSkeleton = (
        <div className="space-y-0.5">
            {Array.from({ length: 8 }).map((_, i) => (
                <Skeleton key={i} className="w-full h-6 rounded-md" />
            ))}
        </div>
    );
    const buildSkeleton = (
        <div className="space-y-0.5">
            {Array.from({ length: 8 }).map((_, i) => (
                <Skeleton key={i} className="w-full h-5 rounded-md" />
            ))}
        </div>
    );

    // Hard cap at ~10 visible items per column. Above that the panel would
    // start eating the rest of the page; we'd rather have a tight self-
    // contained box that scrolls internally than a wizard that grows every
    // time a new MC build appears. The columns differ in row height
    // (software is the tallest), so we set per-column maxes.
    const SOFTWARE_MAX_H = 'max-h-[360px]'; // ~9 entries @ 38px
    const VERSION_MAX_H = 'max-h-[300px]';  // ~10 entries @ 28px
    const BUILD_MAX_H = 'max-h-[260px]';    // ~10 entries @ 24px

    return (
        <div className="grid grid-cols-3 gap-3 items-start">
            {/* Software column */}
            <div className="flex flex-col gap-[5px]">
                <label className="input-label">Software</label>
                <div className={`border border-(--base-03) rounded-md overflow-y-auto bg-(--base-03) p-1 space-y-0.5 ${SOFTWARE_MAX_H}`}>
                    {availableSoftware.map(s => {
                        const meta = SOFTWARE_META[s];
                        return (
                            <div key={s} className="flex items-center gap-1">
                                <button type="button" onClick={() => onSoftwareChange(s)}
                                    className={`flex-1 p-2 rounded-md text-left capitalize text-sm ${
                                        software === s ? 'bg-(--accent) text-white font-medium' : 'hover:bg-(--base-04) text-(--base-07)'
                                    }`}>
                                    {s}
                                </button>
                                {meta && (
                                    <div className="relative shrink-0">
                                        <button
                                            type="button"
                                            aria-label={`Info: ${s}`}
                                            onMouseEnter={e => {
                                                // Capture viewport coords for a portal-rendered tooltip
                                                // so the popup escapes the software column's
                                                // overflow-y-auto -- otherwise the first row's
                                                // tooltip is clipped against the top of the box.
                                                const rect = (e.currentTarget as HTMLElement).getBoundingClientRect();
                                                setTooltipAt({ x: rect.right, y: rect.top });
                                                setHoveredInfo(s);
                                            }}
                                            onMouseLeave={() => { setHoveredInfo(null); setTooltipAt(null); }}
                                            onClick={e => {
                                                e.stopPropagation();
                                                window.open(meta.url, '_blank', 'noopener,noreferrer');
                                            }}
                                            className={`inline-flex items-center justify-center rounded p-0.5 transition-colors ${
                                                software === s
                                                    ? 'text-white/60 hover:text-white'
                                                    : 'text-(--base-05) hover:text-(--base-08)'
                                            }`}
                                        >
                                            <Info size={13} />
                                        </button>
                                    </div>
                                )}
                            </div>
                        );
                    })}
                </div>
            </div>

            {/* Major Version column */}
            <div className="flex flex-col gap-[5px]">
                <label className="input-label">Version</label>
                <div className={`border border-(--base-03) rounded-md overflow-y-auto bg-(--base-03) p-1 space-y-0.5 ${VERSION_MAX_H}`}>
                    {loading ? versionSkeleton : majorVersions.map(m => (
                        <button key={m} type="button" onClick={() => {
                            onMajorChange(m);
                            const builds = allVersions.filter(v => v.major === m).map(v => v.build).sort(compareVersionsDesc);
                            if (builds.length > 0 && !builds.includes(selectedBuild)) {
                                onBuildChange(builds[0]);
                            }
                        }}
                            className={`w-full p-1.5 text-xs rounded-md text-left font-mono ${
                                selectedMajor === m ? 'bg-(--accent) text-white font-medium' : 'hover:bg-(--base-04) text-(--base-07)'
                            }`}>
                            {m}
                        </button>
                    ))}
                </div>
            </div>

            {/* Build column */}
            <div className="flex flex-col gap-[5px]">
                <label className="input-label">Build</label>
                <div className={`border border-(--base-03) rounded-md overflow-y-auto bg-(--base-03) p-1 space-y-0.5 ${BUILD_MAX_H}`}>
                    {loading ? buildSkeleton : buildsForMajor.map(b => (
                        <button key={b} type="button" onClick={() => onBuildChange(b)}
                            className={`w-full p-1 text-xs rounded-md text-left font-mono ${
                                selectedBuild === b ? 'bg-(--accent) text-white font-medium' : 'hover:bg-(--base-04) text-(--base-07)'
                            }`}>
                            {b}
                        </button>
                    ))}
                    {!loading && buildsForMajor.length === 0 && selectedMajor && (
                        <p className="text-xs text-(--base-06) text-center py-4 font-mono">No builds</p>
                    )}
                    {!loading && !selectedMajor && (
                        <p className="text-xs text-(--base-06) text-center py-4 font-mono">Select a version</p>
                    )}
                </div>
            </div>

            {/* Tooltip rendered into document.body to escape the
                software column's overflow clipping. transform-translate
                shifts it above-left of the anchor (mirrors the original
                bottom-full right-0 positioning). */}
            {hoveredInfo && tooltipAt && portalRoot && SOFTWARE_META[hoveredInfo] && createPortal(
                <div
                    className="fixed z-1000 w-48 rounded-md bg-(--base-02) border border-(--base-03) px-2.5 py-1.5 text-[11px] text-(--base-07) leading-snug shadow-lg pointer-events-none whitespace-normal"
                    style={{ left: tooltipAt.x, top: tooltipAt.y, transform: 'translate(-100%, calc(-100% - 6px))' }}
                >
                    {SOFTWARE_META[hoveredInfo].desc}
                </div>,
                portalRoot,
            )}
        </div>
    );
}
