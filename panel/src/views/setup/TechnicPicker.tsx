"use client";

import { useEffect, useRef, useState } from 'react';
import { Search, Package, RefreshCw, AlertTriangle, Info, Check, ExternalLink } from 'lucide-react';
import { searchTechnic, getTechnicPack, type TechnicSearchHit, type TechnicPack } from '@/lib/api/technic';
import { Skeleton, SkeletonText } from '@/components/Skeleton';
import { technicNotice, type TechnicSelection } from './technic';

// Technic Platform picker for the setup flow. Search, open a pack, see what
// will be installed (the author's server pack or the client pack), pick a
// build for Solder packs. Core resolves the actual download at install time.

interface TechnicPickerProps {
    selection: TechnicSelection | null;
    onSelect: (s: TechnicSelection | null) => void;
}

// Technic rate limits hard, so typing waits longer than the Modrinth picker.
const SEARCH_DEBOUNCE_MS = 600;

function Notice({ variant }: { variant: TechnicPack['variant'] }) {
    const n = technicNotice(variant);
    const Icon = n.tone === 'warning' ? AlertTriangle : Info;
    return (
        <p className={`text-xs flex items-start gap-1.5 ${n.tone === 'warning' ? 'text-(--warning-light)' : 'text-(--base-07)'}`}>
            <Icon size={12} className="mt-0.5 shrink-0" />
            {n.text}
        </p>
    );
}

function Source({ url }: { url: string }) {
    return (
        <p className="text-[11px] text-(--base-06)">
            From{' '}
            {url ? (
                <a href={url} target="_blank" rel="noopener noreferrer" className="text-(--accent-light) inline-flex items-center gap-1 hover:underline">
                    Technic Platform <ExternalLink size={9} />
                </a>
            ) : 'Technic Platform'}
            . Mod permissions are the pack author&apos;s responsibility.
        </p>
    );
}

export default function TechnicPicker({ selection, onSelect }: TechnicPickerProps) {
    const [query, setQuery] = useState('');
    const [searching, setSearching] = useState(false);
    const [hits, setHits] = useState<TechnicSearchHit[]>([]);
    const [searchError, setSearchError] = useState('');
    const [openSlug, setOpenSlug] = useState<string | null>(null);
    const [pack, setPack] = useState<TechnicPack | null>(null);
    const [packLoading, setPackLoading] = useState(false);
    const [packError, setPackError] = useState('');
    const [build, setBuild] = useState('');
    // The pack the operator opened last; an answer for an earlier click is dropped.
    const openRef = useRef<string | null>(null);

    useEffect(() => {
        const q = query.trim();
        if (!q) {
            setHits([]);
            setSearchError('');
            return;
        }
        let cancelled = false;
        const t = setTimeout(async () => {
            setSearching(true);
            const res = await searchTechnic(q);
            if (cancelled) return;
            setSearching(false);
            if (res.ok) {
                setHits(res.data.packs);
                setSearchError('');
            } else {
                setHits([]);
                setSearchError(res.message);
            }
        }, SEARCH_DEBOUNCE_MS);
        return () => { cancelled = true; clearTimeout(t); };
    }, [query]);

    const openPack = async (slug: string) => {
        if (openSlug === slug) {
            openRef.current = null;
            setOpenSlug(null);
            setPack(null);
            return;
        }
        openRef.current = slug;
        setOpenSlug(slug);
        setPack(null);
        setPackError('');
        setPackLoading(true);
        const res = await getTechnicPack(slug);
        if (openRef.current !== slug) return;
        setPackLoading(false);
        if (!res.ok) {
            setPackError(res.message);
            return;
        }
        setPack(res.data);
        setBuild(res.data.builds?.recommended || '');
    };

    const choose = (p: TechnicPack) => {
        onSelect({
            slug: p.slug,
            displayName: p.displayName || p.slug,
            variant: p.variant,
            build: p.variant === 'client-solder' ? build : undefined,
            mcVersion: p.minecraft || undefined,
        });
    };

    if (selection) {
        return (
            <div className="min-h-[220px] space-y-2">
                <div className="card p-3 border-(--accent) bg-(--accent-ghost)/30 flex items-start gap-3">
                    <Check size={16} className="text-(--accent-light) shrink-0 mt-0.5" />
                    <div className="min-w-0 flex-1">
                        <div className="text-sm font-medium text-(--base-09)">{selection.displayName}</div>
                        <div className="text-xs text-(--base-06) font-mono mt-0.5">
                            {selection.mcVersion && <>MC {selection.mcVersion}</>}
                            {selection.build && <> · build {selection.build}</>}
                        </div>
                    </div>
                    <button type="button" onClick={() => onSelect(null)} className="text-xs text-(--base-06) hover:text-(--base-09)">
                        Change
                    </button>
                </div>
                <Notice variant={selection.variant} />
            </div>
        );
    }

    return (
        <div className="min-h-[220px] space-y-3">
            <div className="relative">
                <Search size={13} className="absolute left-3 top-1/2 -translate-y-1/2 text-(--base-05)" />
                <input
                    type="text"
                    value={query}
                    onChange={e => setQuery(e.target.value)}
                    placeholder="Search Technic modpacks…"
                    className="input-field w-full pl-8"
                />
            </div>

            {searchError && <p className="text-xs text-(--error-light)">{searchError}</p>}

            {searching ? (
                <div className="space-y-1.5">
                    {Array.from({ length: 4 }).map((_, i) => (
                        <div key={i} className="card p-2 flex items-center gap-3">
                            <Skeleton className="w-8 h-8 rounded-md shrink-0" />
                            <SkeletonText width="w-1/3" className="h-3" />
                        </div>
                    ))}
                </div>
            ) : !query.trim() ? (
                <div className="text-center py-6 text-sm text-(--base-06)">Type a pack name to search Technic.</div>
            ) : hits.length === 0 && !searchError ? (
                <div className="text-center py-6 text-sm text-(--base-06)">No modpacks match.</div>
            ) : (
                <div className="space-y-1.5 max-h-[320px] overflow-y-auto">
                    {hits.map(hit => (
                        <div key={hit.slug}>
                            <button
                                type="button"
                                onClick={() => openPack(hit.slug)}
                                className="w-full card p-2 flex items-center gap-3 text-left hover:border-(--accent-border)"
                            >
                                {hit.iconUrl ? (
                                    // eslint-disable-next-line @next/next/no-img-element
                                    <img src={hit.iconUrl} alt="" className="w-8 h-8 rounded-md shrink-0" />
                                ) : (
                                    <div className="w-8 h-8 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
                                        <Package size={12} className="text-(--base-05)" />
                                    </div>
                                )}
                                <div className="min-w-0 flex-1 text-sm font-medium text-(--base-09) truncate">{hit.name}</div>
                            </button>
                            {openSlug === hit.slug && (
                                <div className="ml-11 mt-1 mb-2 space-y-2">
                                    {packLoading ? (
                                        <div className="text-xs text-(--base-06) flex items-center gap-1.5 px-2 py-1">
                                            <RefreshCw size={11} className="animate-spin" />
                                            Loading pack…
                                        </div>
                                    ) : packError ? (
                                        <p className="text-xs text-(--error-light) px-2">{packError}</p>
                                    ) : pack && (
                                        <>
                                            <div className="text-[11px] font-mono text-(--base-06)">
                                                {pack.author && <>by {pack.author} · </>}
                                                {pack.minecraft && <>MC {pack.minecraft} · </>}
                                                version {pack.version || 'unknown'}
                                            </div>
                                            {pack.variant === 'client-solder' && pack.builds && (
                                                <label className="flex items-center gap-2 text-xs text-(--base-07)">
                                                    Build
                                                    <select value={build} onChange={e => setBuild(e.target.value)} className="input-field py-1 text-xs">
                                                        {pack.builds.list.map(b => (
                                                            <option key={b} value={b}>
                                                                {b}{b === pack.builds?.recommended ? ' (recommended)' : b === pack.builds?.latest ? ' (latest)' : ''}
                                                            </option>
                                                        ))}
                                                    </select>
                                                </label>
                                            )}
                                            <Notice variant={pack.variant} />
                                            <Source url={pack.platformUrl} />
                                            <button
                                                type="button"
                                                disabled={!technicNotice(pack.variant).installable}
                                                onClick={() => choose(pack)}
                                                className="btn btn-primary py-1.5 px-3 text-xs"
                                            >
                                                Use this pack
                                            </button>
                                        </>
                                    )}
                                </div>
                            )}
                        </div>
                    ))}
                </div>
            )}
        </div>
    );
}
