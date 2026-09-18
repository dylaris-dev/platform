"use client";

import React, { useEffect, useState } from 'react';
import { Package, Boxes, RefreshCw, AlertTriangle, Check } from 'lucide-react';
import { listPacks, listBuilds, type Pack, type PackBuild } from '@/lib/api/packs';
import { Skeleton, SkeletonText } from '@/components/Skeleton';

// Pack picker for the setup flow. Mirrors ModpackPicker's shape (pick →
// expand → pick sub-item) but reads the user's own Core packs (Unified
// Modpack Builder) instead of searching Modrinth. Draft builds
// (mrpackStorageKey empty) are installable too — Core renders the
// .mrpack on demand — so we never disable them, we only show a
// published/draft hint.

export interface PackSelection {
    packId: number;
    buildId: number;
    packName: string;
    versionString: string;
    loader?: string;
    mcVersion?: string;
    published?: boolean;
}

interface PackPickerProps {
    selection: PackSelection | null;
    onSelect: (s: PackSelection | null) => void;
}

export default function PackPicker({ selection, onSelect }: PackPickerProps) {
    const [packs, setPacks] = useState<Pack[]>([]);
    const [loadingPacks, setLoadingPacks] = useState(false);
    const [packsError, setPacksError] = useState('');

    const [expandedPackId, setExpandedPackId] = useState<number | null>(null);
    const [builds, setBuilds] = useState<PackBuild[]>([]);
    const [loadingBuilds, setLoadingBuilds] = useState(false);
    const [buildsError, setBuildsError] = useState('');

    useEffect(() => {
        let cancelled = false;
        (async () => {
            setLoadingPacks(true);
            setPacksError('');
            try {
                const res = await listPacks();
                if (!cancelled) setPacks(res || []);
            } catch {
                if (!cancelled) setPacksError('Could not load your packs.');
            }
            if (!cancelled) setLoadingPacks(false);
        })();
        return () => { cancelled = true; };
    }, []);

    const handleExpand = async (pack: Pack) => {
        if (expandedPackId === pack.id) {
            setExpandedPackId(null);
            setBuilds([]);
            setBuildsError('');
            return;
        }
        setExpandedPackId(pack.id);
        setBuilds([]);
        setBuildsError('');
        setLoadingBuilds(true);
        try {
            const res = await listBuilds(pack.id);
            setBuilds(res || []);
        } catch {
            setBuildsError('Could not load builds for this pack.');
        }
        setLoadingBuilds(false);
    };

    const handlePickBuild = (pack: Pack, build: PackBuild) => {
        onSelect({
            packId: pack.id,
            buildId: build.id,
            packName: pack.internalName,
            versionString: build.versionString,
            loader: build.loader || undefined,
            mcVersion: build.minecraft || undefined,
            published: build.modrinthPublished,
        });
    };

    return (
        <div className="min-h-[220px] space-y-3">
            {selection ? (
                <div className="card p-3 border-(--accent) bg-(--accent-ghost)/30 flex items-start gap-3">
                    <Check size={16} className="text-(--accent-light) shrink-0 mt-0.5" />
                    <div className="min-w-0 flex-1">
                        <div className="text-sm font-medium text-(--base-09)">{selection.packName}</div>
                        <div className="text-xs text-(--base-06) font-mono mt-0.5">
                            {selection.loader && <>{selection.loader} · </>}
                            {selection.mcVersion && <>MC {selection.mcVersion} · </>}
                            {selection.versionString}
                        </div>
                        <div className="text-[10px] text-(--base-06) mt-1">
                            {selection.published ? 'Published build' : 'Draft build (rendered on install)'}
                        </div>
                    </div>
                    <button
                        type="button"
                        onClick={() => onSelect(null)}
                        className="text-xs text-(--base-06) hover:text-(--base-09)"
                    >
                        Change
                    </button>
                </div>
            ) : (
                <>
                    {loadingPacks ? (
                        <div className="space-y-1.5 max-h-[280px] overflow-y-auto">
                            {Array.from({ length: 3 }).map((_, i) => (
                                <div key={i} className="card p-2 flex items-center gap-3">
                                    <Skeleton className="w-8 h-8 rounded-md shrink-0" />
                                    <div className="min-w-0 flex-1 space-y-1.5">
                                        <SkeletonText width="w-1/3" className="h-3" />
                                        <SkeletonText width="w-1/4" className="h-2" />
                                    </div>
                                </div>
                            ))}
                        </div>
                    ) : packsError ? (
                        <div className="text-center py-6 text-sm text-(--error-light) flex flex-col items-center gap-2">
                            <AlertTriangle size={16} />
                            {packsError}
                        </div>
                    ) : packs.length === 0 ? (
                        <div className="text-center py-6 text-sm text-(--base-06)">
                            No packs yet. Build one under <span className="font-mono">Modpacks</span> first.
                        </div>
                    ) : (
                        <div className="space-y-1.5 max-h-[280px] overflow-y-auto">
                            {packs.map(pack => (
                                <div key={pack.id}>
                                    <button
                                        type="button"
                                        onClick={() => handleExpand(pack)}
                                        className="w-full card p-2 flex items-center gap-3 text-left hover:border-(--accent-border)"
                                    >
                                        <div className="w-8 h-8 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
                                            <Package size={12} className="text-(--base-05)" />
                                        </div>
                                        <div className="min-w-0 flex-1">
                                            <div className="text-sm font-medium text-(--base-09) truncate">
                                                {pack.internalName}
                                            </div>
                                            <div className="text-[10px] font-mono text-(--base-06) truncate">
                                                /{pack.internalSlug}
                                            </div>
                                        </div>
                                    </button>
                                    {expandedPackId === pack.id && (
                                        <div className="ml-11 mt-1 mb-2 space-y-1">
                                            {loadingBuilds ? (
                                                <div className="text-xs text-(--base-06) flex items-center gap-1.5 px-2 py-1">
                                                    <RefreshCw size={11} className="animate-spin" />
                                                    Loading builds…
                                                </div>
                                            ) : buildsError ? (
                                                <div className="text-xs text-(--error-light) flex items-center gap-1.5 px-2 py-1">
                                                    <AlertTriangle size={11} />
                                                    {buildsError}
                                                </div>
                                            ) : builds.length === 0 ? (
                                                <div className="text-xs text-(--base-06) px-2 py-1">No builds found.</div>
                                            ) : (
                                                builds.map(build => (
                                                    <button
                                                        key={build.id}
                                                        type="button"
                                                        onClick={() => handlePickBuild(pack, build)}
                                                        className="w-full flex items-center gap-2 px-2 py-1.5 rounded-md border border-(--base-04) hover:border-(--accent-border) hover:bg-(--accent-ghost)/30 text-left"
                                                    >
                                                        <Boxes size={11} className="text-(--accent-light) shrink-0" />
                                                        <div className="min-w-0 flex-1">
                                                            <div className="text-xs text-(--base-09)">{build.versionString}</div>
                                                            <div className="text-[10px] font-mono text-(--base-06) truncate">
                                                                {build.loader} · MC {build.minecraft} ·{' '}
                                                                {build.modrinthPublished ? 'published' : 'draft'}
                                                            </div>
                                                        </div>
                                                    </button>
                                                ))
                                            )}
                                        </div>
                                    )}
                                </div>
                            ))}
                        </div>
                    )}

                    <p className="text-[11px] text-(--base-06) flex items-start gap-1.5">
                        <AlertTriangle size={10} className="mt-0.5 shrink-0 text-(--warning-light)" />
                        Pack installs render the build's mods, resource packs and overrides, then install like a modpack. Draft builds install fine — they render on demand.
                    </p>
                </>
            )}
        </div>
    );
}
