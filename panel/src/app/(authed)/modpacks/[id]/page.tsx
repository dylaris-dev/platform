"use client";

import React, { useCallback, useEffect, useState } from 'react';

import Link from 'next/link';
import { Package, ArrowLeft, Plus, Trash2, ExternalLink, RefreshCw, CircleAlert, X, ChevronRight, Layers } from 'lucide-react';
import { systemEvents } from '@/lib/systemEvents';
import {
    getPack, listBuilds, createBuild, deleteBuild,
    type Pack, type PackBuild,
} from '@/lib/api/packs';
import { useAppData } from '@/lib/AppDataContext';
import { SkeletonHeader, SkeletonList, SkeletonText, Skeleton } from '@/components/Skeleton';
import { Badge } from '@/components/ui/Badge';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';

// Pack detail. Shows pack metadata + its builds. Each build pins a
// Minecraft version + loader and links to the per-build content editor at
// /modpacks/<id>/builds/<buildId>. The publish-target chip (modrinth) is a
// simple text badge for now.

export default function PackDetailPage() {
    const paramId = useRouteId('modpacks');
    const packId = Number(paramId);
    const { featureFlags } = useAppData();
    const modpacksDisabled = !featureFlags.modpacks;
    const [pack, setPack] = useState<Pack | null>(null);
    const [builds, setBuilds] = useState<PackBuild[]>([]);
    const [loading, setLoading] = useState(true);
    const [creatingBuild, runCreate] = useBusy();
    const [deletingBuild, runDelete] = useBusy();
    const [creating, setCreating] = useState<{
        versionString: string; minecraft: string; loader: string; loaderVersion: string;
    } | null>(null);
    const [deletePrompt, setDeletePrompt] = useState<PackBuild | null>(null);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const [p, bs] = await Promise.all([getPack(packId), listBuilds(packId)]);
        setPack(p);
        setBuilds(bs);
        setLoading(false);
    }, [packId]);

    useEffect(() => { refresh(); }, [refresh]);

    useEffect(() => {
        const unsubB = systemEvents.on('pack_builds.changed', (evt) => {
            const pid = (evt.payload as any)?.packId;
            if (pid === undefined || pid === packId) refresh();
        });
        const unsubP = systemEvents.on('packs.changed', () => { refresh(); });
        return () => { unsubB(); unsubP(); };
    }, [packId, refresh]);

    const handleCreate = async () => {
        if (!creating) return;
        if (!creating.versionString.trim()) { showToast('Version string required', false); return; }
        const res = await createBuild(packId, {
            versionString: creating.versionString.trim(),
            minecraft: creating.minecraft.trim() || undefined,
            loader: creating.loader.trim() || undefined,
            loaderVersion: creating.loaderVersion.trim() || undefined,
        });
        if (res.success && res.build) {
            setCreating(null);
            showToast('Build created.', true);
            refresh();
        } else {
            showToast(res.message || 'Create failed', false);
        }
    };

    const handleDelete = async () => {
        if (!deletePrompt) return;
        const res = await deleteBuild(packId, deletePrompt.id);
        if (res.success) {
            setDeletePrompt(null);
            showToast('Build deleted.', true);
            refresh();
        } else {
            showToast(res.message || 'Delete failed', false);
        }
    };

    if (loading) {
        return (
            <main className="flex-1 overflow-y-auto p-6 max-w-4xl">
                <SkeletonText width="w-32" className="mb-3" />
                <header className="flex items-start gap-3 mb-4">
                    <Skeleton className="w-12 h-12 rounded-md shrink-0" />
                    <div className="flex-1">
                        <SkeletonHeader />
                    </div>
                </header>
                <div className="flex items-center gap-2 mb-4">
                    <SkeletonText width="w-16" />
                    <SkeletonText width="w-20" />
                    <SkeletonText width="w-24" />
                </div>
                <SkeletonText width="w-28" className="mb-3 h-4" />
                <SkeletonList rows={4} />
            </main>
        );
    }
    if (!pack) {
        return (
            <main className="flex-1 flex flex-col items-center justify-center p-6 text-(--base-06) gap-3">
                <p className="text-sm">Pack not found.</p>
                <Link href="/modpacks" className="btn btn-secondary btn-sm">
                    <ArrowLeft size={12} />
                    Back to list
                </Link>
            </main>
        );
    }

    return (
        <main className="flex-1 overflow-y-auto p-6 max-w-4xl">
            <Link href="/modpacks" className="text-xs text-(--base-06) hover:text-(--base-09) inline-flex items-center gap-1 mb-3">
                <ArrowLeft size={11} />
                All modpacks
            </Link>

            {modpacksDisabled && (
                <div className="card p-3 border border-(--warning) bg-(--warning)/10 mb-4 flex items-start gap-2">
                    <CircleAlert size={16} className="text-(--warning) mt-0.5 shrink-0" />
                    <div className="text-xs text-(--base-09)">
                        Modpack authoring is disabled by the platform admin.
                        Existing packs remain readable and downloadable.
                    </div>
                </div>
            )}

            <header className="flex items-start gap-3 mb-4">
                <div className="w-12 h-12 rounded-md bg-(--accent-ghost) flex items-center justify-center shrink-0">
                    <Package size={20} className="text-(--accent-light)" />
                </div>
                <div className="min-w-0 flex-1">
                    <h1 className="text-lg font-display font-bold text-(--base-09)">{pack.internalName}</h1>
                    <div className="text-xs text-(--base-06) font-mono">/{pack.internalSlug}</div>
                    {pack.summary && <p className="text-sm text-(--base-07) mt-1">{pack.summary}</p>}
                </div>
            </header>

            <div className="flex items-center gap-2 flex-wrap mb-4">
                {/* Named for its axis: this is the Modrinth publish visibility. */}
                <Badge variant="neutral">modrinth visibility: {pack.modrinthVisibility}</Badge>
                {pack.modrinthProjectId ? (
                    <a
                        href={`https://modrinth.com/modpack/${pack.internalSlug}`}
                        target="_blank"
                        rel="noopener noreferrer"
                    >
                        <Badge variant="success" icon={<ExternalLink size={9} />}>on modrinth</Badge>
                    </a>
                ) : (
                    <Badge variant="neutral">local only</Badge>
                )}
            </div>

            <section>
                <div className="flex items-center gap-2 mb-3">
                    <Layers size={16} className="text-(--accent-light)" />
                    <h2 className="text-sm font-medium text-(--base-09)">Builds</h2>
                    <div className="ml-auto flex items-center gap-2">
                        <button onClick={refresh} className="btn btn-secondary btn-sm">
                            <RefreshCw size={12} />
                        </button>
                        <button
                            onClick={() => setCreating({ versionString: '', minecraft: '', loader: 'fabric', loaderVersion: '' })}
                            className="btn btn-primary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                            disabled={modpacksDisabled}
                            title={modpacksDisabled ? 'Modpack authoring is disabled' : undefined}
                        >
                            <Plus size={12} />
                            New build
                        </button>
                    </div>
                </div>

                {builds.length === 0 ? (
                    <div className="card p-6 text-center text-sm text-(--base-06)">
                        No builds yet. Create one to start adding content.
                    </div>
                ) : (
                    <div className="space-y-2">
                        {builds.map(b => (
                            <article key={b.id} className="card p-3 flex items-center gap-3">
                                <div className="min-w-0 flex-1">
                                    <div className="text-sm font-medium text-(--base-09) inline-flex items-center gap-2">
                                        {b.versionString}
                                        <span className="mono-label text-(--base-06) font-normal">
                                            MC {b.minecraft || 'any'} · {b.loader || 'any loader'}
                                            {b.loaderVersion && ` ${b.loaderVersion}`}
                                        </span>
                                    </div>
                                    <div className="text-xs text-(--base-06) mt-0.5 flex items-center gap-2 flex-wrap">
                                        <span>Created {new Date(b.createdAt).toLocaleString()}</span>
                                        <Badge variant={b.modrinthPublished ? 'success' : 'neutral'}>modrinth: {b.modrinthPublished ? 'published' : 'not published'}</Badge>
                                        {b.frozen && <Badge variant="warning">frozen</Badge>}
                                    </div>
                                </div>
                                <Link
                                    href={`/modpacks/${packId}/builds/${b.id}`}
                                    className="btn btn-secondary btn-sm"
                                >
                                    Open
                                    <ChevronRight size={12} />
                                </Link>
                                <button
                                    onClick={() => setDeletePrompt(b)}
                                    className="btn btn-secondary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                                    disabled={modpacksDisabled}
                                    title={modpacksDisabled ? 'Modpack authoring is disabled' : 'Delete build'}
                                >
                                    <Trash2 size={12} className="text-(--error)" />
                                </button>
                            </article>
                        ))}
                    </div>
                )}
            </section>

            {/* Create build */}
            {creating && (
                <div className="modal-overlay animate-fade-in" onClick={() => setCreating(null)}>
                    <div className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2">
                                <Layers size={16} />
                                New build
                            </h3>
                            <button onClick={() => setCreating(null)} className="text-(--base-06)"><X size={16} /></button>
                        </div>
                        <div className="modal-body space-y-4">
                            <div>
                                <label className="input-label">Version string</label>
                                <input
                                    type="text"
                                    value={creating.versionString}
                                    onChange={e => setCreating({ ...creating, versionString: e.target.value })}
                                    className="input-field input-mono w-full"
                                    placeholder="0.1.0"
                                />
                            </div>
                            <div className="grid grid-cols-2 gap-3">
                                <div>
                                    <label className="input-label">Minecraft</label>
                                    <input
                                        type="text"
                                        value={creating.minecraft}
                                        onChange={e => setCreating({ ...creating, minecraft: e.target.value })}
                                        className="input-field input-mono w-full"
                                        placeholder="1.20.2"
                                    />
                                </div>
                                <div>
                                    <label className="input-label">Loader</label>
                                    <input
                                        type="text"
                                        value={creating.loader}
                                        onChange={e => setCreating({ ...creating, loader: e.target.value.toLowerCase() })}
                                        className="input-field input-mono w-full"
                                        placeholder="fabric"
                                    />
                                </div>
                            </div>
                            <div>
                                <label className="input-label">Loader version (optional)</label>
                                <input
                                    type="text"
                                    value={creating.loaderVersion}
                                    onChange={e => setCreating({ ...creating, loaderVersion: e.target.value })}
                                    className="input-field input-mono w-full"
                                    placeholder="0.15.11"
                                />
                            </div>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setCreating(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runCreate(handleCreate)} disabled={creatingBuild} className="btn btn-primary disabled:opacity-40">Create</button>
                        </div>
                    </div>
                </div>
            )}

            {/* Delete build */}
            {deletePrompt && (
                <div className="modal-overlay animate-fade-in" onClick={() => setDeletePrompt(null)}>
                    <div className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <Trash2 size={16} />
                                Delete build {deletePrompt.versionString}?
                            </h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                Removes the local build + its content list. Published Modrinth
                                versions stay live on modrinth.com — manage that there.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setDeletePrompt(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runDelete(handleDelete)} disabled={deletingBuild} className="btn btn-danger disabled:opacity-40">Delete</button>
                        </div>
                    </div>
                </div>
            )}
        </main>
    );
}
