"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { Search, Package, Download, RefreshCw, AlertTriangle, Check, ExternalLink } from 'lucide-react';
import {
    searchModrinth, getModrinthVersions, pickPrimaryFile,
    type ModrinthSearchHit, type ModrinthVersion,
} from '@/lib/api/modrinth';
import { Skeleton, SkeletonText } from '@/components/Skeleton';

// Modpack picker for the setup flow. Hooked into the existing
// install-tab strip in SetupEditMode. The user searches Modrinth modpacks,
// picks one, then picks a version — the parent SetupView collects the
// chosen project + version IDs and the .mrpack download URL.

export interface ModpackSelection {
    projectId: string;
    projectSlug: string;
    versionId: string;
    title: string;
    downloadUrl: string;
    loader?: string;
    mcVersion?: string;
}

interface ModpackPickerProps {
    selection: ModpackSelection | null;
    onSelect: (s: ModpackSelection | null) => void;
}

export default function ModpackPicker({ selection, onSelect }: ModpackPickerProps) {
    const [query, setQuery] = useState('');
    const [searching, setSearching] = useState(false);
    const [hits, setHits] = useState<ModrinthSearchHit[]>([]);
    const [expandedSlug, setExpandedSlug] = useState<string | null>(null);
    const [versions, setVersions] = useState<ModrinthVersion[]>([]);
    const [versionsLoading, setVersionsLoading] = useState(false);

    const runSearch = useCallback(async () => {
        setSearching(true);
        const res = await searchModrinth({
            query: query.trim() || undefined,
            projectType: 'modpack',
            limit: 20,
            index: 'downloads',
        });
        setHits(res?.hits || []);
        setSearching(false);
    }, [query]);

    useEffect(() => {
        const t = setTimeout(runSearch, 350);
        return () => clearTimeout(t);
    }, [runSearch]);

    const handleExpand = async (hit: ModrinthSearchHit) => {
        if (expandedSlug === hit.slug) {
            setExpandedSlug(null);
            setVersions([]);
            return;
        }
        setExpandedSlug(hit.slug);
        setVersionsLoading(true);
        const v = await getModrinthVersions(hit.slug);
        setVersions(v);
        setVersionsLoading(false);
    };

    const handlePickVersion = (hit: ModrinthSearchHit, v: ModrinthVersion) => {
        const file = pickPrimaryFile(v);
        if (!file) return;
        onSelect({
            projectId: hit.project_id,
            projectSlug: hit.slug,
            versionId: v.id,
            title: hit.title,
            downloadUrl: file.url,
            loader: v.loaders[0],
            mcVersion: v.game_versions[0],
        });
    };

    return (
        <div className="min-h-[220px] space-y-3">
            {selection ? (
                <div className="card p-3 border-(--accent) bg-(--accent-ghost)/30 flex items-start gap-3">
                    <Check size={16} className="text-(--accent-light) shrink-0 mt-0.5" />
                    <div className="min-w-0 flex-1">
                        <div className="text-sm font-medium text-(--base-09)">{selection.title}</div>
                        <div className="text-xs text-(--base-06) font-mono mt-0.5">
                            {selection.loader && <>{selection.loader} · </>}
                            {selection.mcVersion && <>MC {selection.mcVersion} · </>}
                            version {selection.versionId.slice(0, 8)}…
                        </div>
                        <a
                            href={`https://modrinth.com/modpack/${selection.projectSlug}`}
                            target="_blank"
                            rel="noopener noreferrer"
                            className="text-[10px] text-(--accent-light) inline-flex items-center gap-1 mt-1"
                        >
                            modrinth.com <ExternalLink size={9} />
                        </a>
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
                    <div className="relative">
                        <Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-05)" />
                        <input
                            type="text"
                            value={query}
                            onChange={e => setQuery(e.target.value)}
                            placeholder="Search Modrinth modpacks…"
                            className="input-field input-icon-left w-full"
                        />
                    </div>

                    {searching ? (
                        <div className="space-y-1.5 max-h-[280px] overflow-y-auto">
                            {Array.from({ length: 5 }).map((_, i) => (
                                <div key={i} className="card p-2 flex items-center gap-3">
                                    <Skeleton className="w-8 h-8 rounded-md shrink-0" />
                                    <div className="min-w-0 flex-1 space-y-1.5">
                                        <SkeletonText width="w-1/3" className="h-3" />
                                        <SkeletonText width="w-1/4" className="h-2" />
                                    </div>
                                </div>
                            ))}
                        </div>
                    ) : hits.length === 0 ? (
                        <div className="text-center py-6 text-sm text-(--base-06)">No modpacks match.</div>
                    ) : (
                        <div className="space-y-1.5 max-h-[280px] overflow-y-auto">
                            {hits.map(hit => (
                                <div key={hit.project_id}>
                                    <button
                                        type="button"
                                        onClick={() => handleExpand(hit)}
                                        className="w-full card p-2 flex items-center gap-3 text-left hover:border-(--accent-border)"
                                    >
                                        {hit.icon_url ? (
                                            // eslint-disable-next-line @next/next/no-img-element
                                            <img src={hit.icon_url} alt="" className="w-8 h-8 rounded-md shrink-0" />
                                        ) : (
                                            <div className="w-8 h-8 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
                                                <Package size={12} className="text-(--base-05)" />
                                            </div>
                                        )}
                                        <div className="min-w-0 flex-1">
                                            <div className="text-sm font-medium text-(--base-09) truncate">{hit.title}</div>
                                            <div className="text-[10px] font-mono text-(--base-06) truncate">by {hit.author} · {hit.downloads.toLocaleString()} downloads</div>
                                        </div>
                                    </button>
                                    {expandedSlug === hit.slug && (
                                        <div className="ml-11 mt-1 mb-2 space-y-1">
                                            {versionsLoading ? (
                                                <div className="text-xs text-(--base-06) flex items-center gap-1.5 px-2 py-1">
                                                    <RefreshCw size={11} className="animate-spin" />
                                                    Loading versions…
                                                </div>
                                            ) : versions.length === 0 ? (
                                                <div className="text-xs text-(--base-06) px-2 py-1">No versions found.</div>
                                            ) : (
                                                versions.slice(0, 8).map(v => (
                                                    <button
                                                        key={v.id}
                                                        type="button"
                                                        onClick={() => handlePickVersion(hit, v)}
                                                        className="w-full flex items-center gap-2 px-2 py-1.5 rounded-md border border-(--base-04) hover:border-(--accent-border) hover:bg-(--accent-ghost)/30 text-left"
                                                    >
                                                        <Download size={11} className="text-(--accent-light) shrink-0" />
                                                        <div className="min-w-0 flex-1">
                                                            <div className="text-xs text-(--base-09)">{v.version_number}</div>
                                                            <div className="text-[10px] font-mono text-(--base-06) truncate">
                                                                {v.version_type} · {v.loaders.join(', ')} · MC {v.game_versions.join(', ')}
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
                        Modpack installs download every listed mod + apply overrides. Can take a few minutes.
                    </p>
                </>
            )}
        </div>
    );
}
