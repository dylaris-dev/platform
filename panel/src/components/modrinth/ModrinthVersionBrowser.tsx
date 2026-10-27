"use client";

import React, { useEffect, useState } from 'react';
import { Search, RefreshCw, AlertTriangle, Plus, ExternalLink, ChevronDown } from 'lucide-react';
import {
    searchModrinth, getModrinthVersionsOrThrow,
    type ModrinthSearchHit, type ModrinthVersion,
} from '@/lib/api/modrinth';
import { envLabel, envParam, matchesEnv, type EnvFilter } from '@/lib/modSide';
import { EnvironmentFilter, EnvTag } from '@/components/mods/EnvironmentFilter';
import ProjectIcon from '@/components/modpacks/ProjectIcon';
import ModProjectDetails from '@/components/mods/ModProjectDetails';

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
    const [searchRetry, setSearchRetry] = useState(0);

    const [expandedSlug, setExpandedSlug] = useState<string | null>(null);
    const [versions, setVersions] = useState<ModrinthVersion[]>([]);
    const [versionsLoading, setVersionsLoading] = useState(false);
    const [versionsError, setVersionsError] = useState('');
    const [versionRetry, setVersionRetry] = useState(0);
    const [visibleVersions, setVisibleVersions] = useState(6);

    // Debounced search — re-runs when query, loader, or mcVersion changes.
    useEffect(() => {
        let live = true;
        setSearching(true);
        setExpandedSlug(null);
        setVersions([]);
        const t = setTimeout(async () => {
            setSearching(true);
            setSearchError(false);
            const res = await searchModrinth({
                query: query.trim() || undefined,
                loaders: isMod && loader && loader !== 'vanilla' ? [loader] : undefined,
                versions: mcVersion ? [mcVersion] : undefined,
                projectType: projectType as Parameters<typeof searchModrinth>[0]['projectType'],
                side,
                limit: 20,
                index: 'relevance',
            });
            if (!live) return;
            if (res === null) {
                setSearchError(true);
                setSearchHits([]);
            } else {
                // Modrinth matches the facets per version; the project's own sides decide.
                setSearchHits(isMod ? res.hits.filter(h => matchesEnv(env, h)) : res.hits);
            }
            setSearching(false);
        }, 350);
        return () => { live = false; clearTimeout(t); };
    }, [query, loader, mcVersion, projectType, isMod, side, env, searchRetry]);

    useEffect(() => {
        let live = true;
        setVersions([]);
        setVersionsError('');
        setVisibleVersions(6);
        if (!expandedSlug) { setVersionsLoading(false); return () => { live = false; }; }
        setVersionsLoading(true);
        getModrinthVersionsOrThrow(expandedSlug, {
            loaders: isMod && loader && loader !== 'vanilla' ? [loader] : undefined,
            versions: mcVersion ? [mcVersion] : undefined,
        }).then(result => { if (live) setVersions(result); })
            .catch(error => { if (live) setVersionsError(error instanceof Error ? error.message : 'Could not load versions.'); })
            .finally(() => { if (live) setVersionsLoading(false); });
        return () => { live = false; };
    }, [expandedSlug, loader, mcVersion, isMod, versionRetry]);

    return (
        <div className="flex flex-col h-full">
            {/* Search input */}
            <div className="relative mb-3 shrink-0">
                <Search size={12} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-05)" />
                <input
                    type="text"
                    value={query}
                    onChange={e => setQuery(e.target.value)}
                    placeholder={projectType === 'mod' ? 'Search mods...' : projectType === 'resourcepack' ? 'Search resource packs...' : 'Search shaders...'}
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
                    <p role="status" className="text-xs text-(--base-06) text-center py-6 flex items-center justify-center gap-1.5">
                        <RefreshCw size={11} className="animate-spin" />
                        Searching…
                    </p>
                ) : searchError ? (
                    <div role="alert" className="text-xs text-(--base-06) text-center py-6 flex items-center justify-center gap-1.5 flex-wrap">
                        <AlertTriangle size={11} className="text-(--warning-light)" />
                        Search failed. Check your connection.
                        <button onClick={() => setSearchRetry(v => v + 1)} className="btn btn-secondary btn-sm">Retry</button>
                    </div>
                ) : searchHits.length === 0 ? (
                    <p className="text-xs text-(--base-06) text-center py-6">
                        {side ? 'No matches with this Environment filter. Turn it off to see more.' : 'No matches.'}
                    </p>
                ) : (
                    searchHits.map(hit => (
                        <article key={hit.project_id} className="card p-3">
                            <div className="flex items-start gap-3">
                            <button
                                onClick={() => setExpandedSlug(current => current === hit.slug ? null : hit.slug)}
                                aria-expanded={expandedSlug === hit.slug}
                                className="min-w-0 flex-1 flex items-start gap-3 text-left rounded-md hover:bg-(--base-02) transition-colors"
                            >
                                <ProjectIcon url={hit.icon_url} name={hit.title} className="w-11 h-11" />
                                <div className="min-w-0 flex-1">
                                    <div className="text-sm font-medium text-(--base-09) truncate">{hit.title}</div>
                                    <p className="text-xs text-(--base-06) mt-1 line-clamp-2">{hit.description}</p>
                                    <div className="text-[10px] font-mono text-(--base-06) flex items-center gap-1.5 min-w-0">
                                        <span className="truncate">by {hit.author} · {hit.downloads.toLocaleString()}</span>
                                        {isMod && envLabel(hit) && <EnvTag label={envLabel(hit)!} />}
                                    </div>
                                </div>
                                {installedSet.has(hit.project_id) && (
                                    <span className="mono-label bg-(--success-ghost) text-(--success-light) px-1.5 rounded-sm shrink-0">added</span>
                                )}
                                <ChevronDown size={14} className={`shrink-0 mt-1 text-(--base-06) ${expandedSlug === hit.slug ? 'rotate-180' : ''}`} />
                            </button>
                            <a href={`https://modrinth.com/project/${encodeURIComponent(hit.project_id)}`} target="_blank" rel="noopener noreferrer" aria-label={`Open ${hit.title} on Modrinth`} title="Open on Modrinth" className="btn btn-ghost btn-sm"><ExternalLink size={14} /></a>
                            </div>

                            {expandedSlug === hit.slug && (
                                <div className="mt-4 pt-4 border-t border-(--base-03) space-y-3">
                                    <ModProjectDetails projectId={hit.project_id} />
                                    <h3 className="input-label mt-4">Compatible versions</h3>
                                    {versionsLoading ? (
                                        <p className="text-xs text-(--base-06) py-1 flex items-center gap-1">
                                            <RefreshCw size={10} className="animate-spin" />
                                            Loading versions…
                                        </p>
                                    ) : versionsError ? (
                                        <div role="alert" className="text-xs text-(--error-light) py-1 space-y-2"><p>{versionsError}</p><button onClick={() => setVersionRetry(v => v + 1)} className="btn btn-secondary btn-sm">Retry versions</button></div>
                                    ) : versions.length === 0 ? (
                                        <p className="text-xs text-(--base-06) py-1">
                                            <AlertTriangle size={10} className="inline mr-1 text-(--warning-light)" />
                                            No versions match this filter.
                                        </p>
                                    ) : (
                                        versions.slice(0, visibleVersions).map(v => (
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
                                    {!versionsLoading && !versionsError && versions.length > visibleVersions && <button onClick={() => setVisibleVersions(count => count + 12)} className="btn btn-secondary btn-sm">Show more versions ({versions.length - visibleVersions} remaining)</button>}
                                </div>
                            )}
                        </article>
                    ))
                )}
            </div>
        </div>
    );
}
