"use client";

import React, { useEffect, useState } from 'react';
import { Package, Search, RefreshCw, AlertTriangle, Plus } from 'lucide-react';
import {
    searchModrinth, getModrinthVersions,
    type ModrinthSearchHit, type ModrinthVersion,
} from '@/lib/api/modrinth';
import { envLabel, envParam, matchesEnv, type EnvFilter } from '@/lib/modSide';
import { EnvironmentFilter, EnvTag } from '@/components/mods/EnvironmentFilter';

interface ModrinthVersionBrowserProps {
    /** Loader filter passed to Modrinth search (e.g. "fabric", "forge"). */
    loader?: string;
    /** Minecraft version filter (e.g. "1.20.1"). */
    mcVersion?: string;
    /** Project type filter for the search query. Defaults to "mod". */
    projectType?: string;
    /**
     * Called when the user picks a version for a search hit.
     * The parent is responsible for the actual add/replace API call.
     */
    onPick: (projectId: string, versionId: string, hit: ModrinthSearchHit, version: ModrinthVersion) => void;
    /**
     * Set (or array) of Modrinth project IDs already in the build.
     * When provided, hits whose project_id is in this set show an "added" badge.
     */
    installedProjectIds?: ReadonlySet<string> | string[];
    /** When true, version pick buttons are disabled and show disabledTitle on hover. */
    disabled?: boolean;
    /** Tooltip text shown on disabled version buttons. */
    disabledTitle?: string;
}

/**
 * Self-contained Modrinth search box + hit list + per-hit version picker.
 * Presentational: it does not call any add/replace API itself — it only fires
 * onPick and lets the parent decide what to do.
 */
export default function ModrinthVersionBrowser({
    loader,
    mcVersion,
    projectType = 'mod',
    onPick,
    installedProjectIds,
    disabled,
    disabledTitle,
}: ModrinthVersionBrowserProps) {
    // Normalize installedProjectIds to a Set once for O(1) lookups.
    const installedSet = React.useMemo<ReadonlySet<string>>(() => {
        if (!installedProjectIds) return new Set();
        if (installedProjectIds instanceof Set) return installedProjectIds;
        return new Set(installedProjectIds);
    }, [installedProjectIds]);
    const [query, setQuery] = useState('');
    // modrinth.com's Environment filter: a pack is built for players, so
    // "where does it run" is the right question here. Mods only.
    const [env, setEnv] = useState<EnvFilter>({ client: false, server: false });
    const isMod = projectType === 'mod';
    const side = isMod ? envParam(env) : undefined;
    const [searchHits, setSearchHits] = useState<ModrinthSearchHit[]>([]);
    const [searching, setSearching] = useState(false);
    const [searchError, setSearchError] = useState(false);

    const [expandedSlug, setExpandedSlug] = useState<string | null>(null);
    const [versions, setVersions] = useState<ModrinthVersion[]>([]);
    const [versionsLoading, setVersionsLoading] = useState(false);

    // Debounced search — re-runs when query, loader, or mcVersion changes.
    useEffect(() => {
        const t = setTimeout(async () => {
            setSearching(true);
            setSearchError(false);
            const res = await searchModrinth({
                query: query.trim() || undefined,
                loaders: loader ? [loader] : undefined,
                versions: mcVersion ? [mcVersion] : undefined,
                projectType: projectType as Parameters<typeof searchModrinth>[0]['projectType'],
                side,
                limit: 20,
                index: 'relevance',
            });
            if (res === null) {
                setSearchError(true);
                setSearchHits([]);
            } else {
                // Modrinth matches the facets per version; the project's own sides decide.
                setSearchHits(isMod ? res.hits.filter(h => matchesEnv(env, h)) : res.hits);
            }
            setSearching(false);
        }, 350);
        return () => clearTimeout(t);
    }, [query, loader, mcVersion, projectType, isMod, side, env]);

    const handleExpand = async (hit: ModrinthSearchHit) => {
        if (expandedSlug === hit.slug) {
            setExpandedSlug(null);
            setVersions([]);
            return;
        }
        setExpandedSlug(hit.slug);
        setVersionsLoading(true);
        const v = await getModrinthVersions(hit.slug, {
            loaders: loader ? [loader] : undefined,
            versions: mcVersion ? [mcVersion] : undefined,
        });
        setVersions(v);
        setVersionsLoading(false);
    };

    return (
        <div className="flex flex-col h-full">
            {/* Search input */}
            <div className="relative mb-3 shrink-0">
                <Search size={12} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-05)" />
                <input
                    type="text"
                    value={query}
                    onChange={e => setQuery(e.target.value)}
                    placeholder="Search mods…"
                    className="input-field input-icon-left w-full"
                />
            </div>

            {isMod && (
                <div className="mb-3 shrink-0">
                    <EnvironmentFilter value={env} onChange={setEnv} inline />
                </div>
            )}

            {/* Results */}
            <div className="flex-1 overflow-y-auto space-y-1.5">
                {searching ? (
                    <p className="text-xs text-(--base-06) text-center py-6 flex items-center justify-center gap-1.5">
                        <RefreshCw size={11} className="animate-spin" />
                        Searching…
                    </p>
                ) : searchError ? (
                    <p className="text-xs text-(--base-06) text-center py-6 flex items-center justify-center gap-1.5">
                        <AlertTriangle size={11} className="text-(--warning-light)" />
                        Search failed. Check your connection.
                    </p>
                ) : searchHits.length === 0 ? (
                    <p className="text-xs text-(--base-06) text-center py-6">
                        {side ? 'No matches with this Environment filter. Turn it off to see more.' : 'No matches.'}
                    </p>
                ) : (
                    searchHits.map(hit => (
                        <div key={hit.project_id}>
                            <button
                                onClick={() => handleExpand(hit)}
                                className="w-full flex items-center gap-2 p-2 rounded-md border border-(--base-04) hover:border-(--accent-border) text-left transition-colors"
                            >
                                {hit.icon_url ? (
                                    // eslint-disable-next-line @next/next/no-img-element
                                    <img src={hit.icon_url} alt="" className="w-8 h-8 rounded-sm shrink-0" />
                                ) : (
                                    <div className="w-8 h-8 rounded-sm bg-(--base-03) flex items-center justify-center shrink-0">
                                        <Package size={12} className="text-(--base-05)" />
                                    </div>
                                )}
                                <div className="min-w-0 flex-1">
                                    <div className="text-sm font-medium text-(--base-09) truncate">{hit.title}</div>
                                    <div className="text-[10px] font-mono text-(--base-06) flex items-center gap-1.5 min-w-0">
                                        <span className="truncate">by {hit.author} · {hit.downloads.toLocaleString()}</span>
                                        {isMod && envLabel(hit) && <EnvTag label={envLabel(hit)!} />}
                                    </div>
                                </div>
                                {installedSet.has(hit.project_id) && (
                                    <span className="mono-label bg-(--success-ghost) text-(--success-light) px-1.5 rounded-sm shrink-0">added</span>
                                )}
                            </button>

                            {expandedSlug === hit.slug && (
                                <div className="ml-10 mt-1 mb-2 space-y-1">
                                    {versionsLoading ? (
                                        <p className="text-xs text-(--base-06) py-1 flex items-center gap-1">
                                            <RefreshCw size={10} className="animate-spin" />
                                            Loading versions…
                                        </p>
                                    ) : versions.length === 0 ? (
                                        <p className="text-xs text-(--base-06) py-1">
                                            <AlertTriangle size={10} className="inline mr-1 text-(--warning-light)" />
                                            No versions match this filter.
                                        </p>
                                    ) : (
                                        versions.slice(0, 6).map(v => (
                                            <button
                                                key={v.id}
                                                onClick={() => onPick(hit.project_id, v.id, hit, v)}
                                                disabled={disabled}
                                                title={disabled ? disabledTitle : undefined}
                                                className="w-full flex items-center gap-2 px-2 py-1.5 rounded-md border border-(--base-04) hover:border-(--accent-border) hover:bg-(--accent-ghost)/30 text-left transition-colors disabled:opacity-40 disabled:cursor-not-allowed"
                                            >
                                                <Plus size={10} className="text-(--accent-light) shrink-0" />
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
                    ))
                )}
            </div>
        </div>
    );
}
