"use client";

import React, { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { useRouter, useSearchParams } from 'next/navigation';
import ProjectIcon, { useProjectMetadata } from '@/components/modpacks/ProjectIcon';
import { Package, Plus, ExternalLink, RefreshCw, CircleAlert, X, Trash2, Users, Download } from 'lucide-react';
import { systemEvents } from '@/lib/systemEvents';
import { createPack, deletePack, type Pack, listPacksOverview, type SharedPack } from '@/lib/api/packs';
import PackCollaboratorsModal from '@/components/modpacks/PackCollaboratorsModal';
import PackActivity from '@/components/modpacks/PackActivity';
import Select from '@/components/ui/Select';
import UnlinkedContentWarning from '@/components/mods/UnlinkedContentWarning';
import { useAppData } from '@/lib/AppDataContext';
import { SkeletonCard } from '@/components/Skeleton';
import { Badge } from '@/components/ui/Badge';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import ModalPanel from '@/components/ui/ModalPanel';

// top-level packs list. Per-user authored packs on the unified pack API.
// The builder UI lives at /modpacks/<id>; this page covers create + list +
// delete + jump-to-detail.

export default function PacksListPage() {
    const { featureFlags, user } = useAppData();
    const searchParams = useSearchParams();
    const router = useRouter();
    const modpacksDisabled = !featureFlags.modpacks;
    // A pack with nowhere to store its archives can be created and then does
    // nothing: every upload answers 424. That failure used to arrive after the
    // work rather than instead of it.
    const noStorage = featureFlags.modpacks && !featureFlags.modpackStorage;
    const cannotAuthor = modpacksDisabled || noStorage;
    const [packs, setPacks] = useState<Pack[]>([]);
    const [loading, setLoading] = useState(true);
    const [creatingPack, runCreate] = useBusy();
    const [deletingPack, runDelete] = useBusy();
    const [creating, setCreating] = useState<{
        internalName: string; slug: string; summary: string; ownerId: string;
    } | null>(null);
    // Packs others invited the reader to, and whose account they may create on.
    const [shared, setShared] = useState<SharedPack[]>([]);
    const [createOn, setCreateOn] = useState<{ id: string; name: string }[]>([]);
    const [collabOpen, setCollabOpen] = useState(false);
    const [deletePrompt, setDeletePrompt] = useState<Pack | null>(null);

    const metadata = useProjectMetadata([...packs, ...shared].map(pack => pack.modrinthProjectId));
    useEffect(() => { if (searchParams.get('create') === '1' && !cannotAuthor) setCreating({ internalName: '', slug: '', summary: '', ownerId: '' }); }, [searchParams, cannotAuthor]);
    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const o = await listPacksOverview();
        setPacks(o.packs);
        setShared(o.shared);
        setCreateOn(o.createOn);
        setLoading(false);
    }, []);

    useEffect(() => { refresh(); }, [refresh]);

    useEffect(() => {
        const unsub = systemEvents.on('packs.changed', () => { refresh(); });
        return () => { unsub(); };
    }, [refresh]);

    const handleCreate = async () => {
        if (!creating) return;
        if (!creating.internalName.trim()) { showToast('Name required', false); return; }
        const res = await createPack({
            name: creating.internalName.trim(),
            slug: creating.slug.trim() || undefined,
            summary: creating.summary.trim() || undefined,
            ownerId: creating.ownerId || undefined,
        });
        if (res.success && res.pack) {
            setCreating(null);
            showToast('Pack created.', true);
            // Straight into the new pack, where its first build is created.
            router.push(`/modpacks/${res.pack.id}`);
        } else {
            showToast(res.message || 'Create failed', false);
        }
    };

    const handleDelete = async () => {
        if (!deletePrompt) return;
        const res = await deletePack(deletePrompt.id);
        if (res.success) {
            setDeletePrompt(null);
            showToast('Pack deleted.', true);
            refresh();
        } else {
            showToast(res.message || 'Delete failed', false);
        }
    };

    return (
        <main className="flex-1 min-h-0 overflow-y-auto p-5">
            <header className="flex items-center gap-3 mb-4">
                <Package size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09)">My Modpacks</h1>
                <div className="ml-auto flex items-center gap-2">
                    <button onClick={() => setCollabOpen(true)} className="btn btn-secondary btn-sm" title="Let other users work on your packs">
                        <Users size={13} /> Collaborators
                    </button>
                    <Link href="/modpacks/import" className="btn btn-secondary btn-sm" title="Import your existing Modrinth modpacks">
                        <Download size={13} /> Import from Modrinth
                    </Link>
                    <button onClick={refresh} className="btn btn-secondary btn-sm" disabled={loading}>
                        <RefreshCw size={12} className={loading ? 'animate-spin' : ''} />
                    </button>
                    <button
                        onClick={() => setCreating({ internalName: '', slug: '', summary: '', ownerId: '' })}
                        className="btn btn-primary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                        disabled={cannotAuthor}
                        title={modpacksDisabled ? 'Modpack authoring is disabled' : noStorage ? 'Modpack storage is not configured' : undefined}
                    >
                        <Plus size={13} />
                        New Pack
                    </button>
                </div>
            </header>

            {noStorage && (
                <div className="card p-3 border border-(--warning) bg-(--warning)/10 mb-4 flex items-start gap-2">
                    <CircleAlert size={16} className="text-(--warning) mt-0.5 shrink-0" />
                    <div className="text-xs text-(--base-09)">
                        Modpack storage is not configured, so archives have nowhere to go and nothing
                        can be created yet.
                        {user?.isAdmin ? (
                            <>
                                {' '}Pick local paths or a storage connection under{' '}
                                <Link href="/settings/modpacks" className="text-(--accent-light)">Settings, Modpacks</Link>.
                            </>
                        ) : (
                            ' Ask an administrator to configure it.'
                        )}
                    </div>
                </div>
            )}

            {modpacksDisabled && (
                <div className="card p-3 border border-(--warning) bg-(--warning)/10 mb-4 flex items-start gap-2">
                    <CircleAlert size={16} className="text-(--warning) mt-0.5 shrink-0" />
                    <div className="text-xs text-(--base-09)">
                        Modpack authoring is disabled by the platform admin.
                        Existing packs remain readable and downloadable.
                    </div>
                </div>
            )}

            <div className="card p-4 mb-4 text-xs text-(--base-07) flex items-start gap-2">
                <Package size={14} className="text-(--accent-light) shrink-0 mt-0.5" />
                <div>
                    Author packs here, then create builds and publish them to Modrinth (
                    <Link href="/account/modrinth" className="text-(--accent-light) inline-flex items-center gap-1">
                        connect your PAT <ExternalLink size={9} />
                    </Link>
                    ), export them as an .mrpack, or install them on a server. Each build
                    pins a Minecraft version + loader and carries its own content list.
                </div>
            </div>

            {loading ? (
                <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
                    {Array.from({ length: 6 }).map((_, i) => (
                        <SkeletonCard key={i} height="h-28" />
                    ))}
                </div>
            ) : packs.length === 0 ? (
                <div className="card p-8 flex flex-col items-center text-center gap-2">
                    <Package size={28} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">No packs yet.</p>
                    <p className="text-xs text-(--base-06)">Create your first one to get started.</p>
                </div>
            ) : (
                <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
                    {packs.map(p => (
                        <Link key={p.id} href={`/modpacks/${p.id}`} className="card p-4 hover:border-(--accent-border) transition-colors group">
                            <div className="flex items-start gap-3">
                                <div className="w-10 h-10 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
                                    <ProjectIcon name={p.internalName} url={metadata.projects[p.modrinthProjectId]?.icon_url} className="w-10 h-10" />
                                </div>
                                <div className="min-w-0 flex-1">
                                    <div className="text-sm font-medium text-(--base-09) truncate group-hover:text-(--accent-light)">{p.internalName}</div>
                                    <div className="text-[10px] font-mono text-(--base-06) truncate">/{p.internalSlug}</div>
                                </div>
                                <button
                                    onClick={(e) => { e.preventDefault(); setDeletePrompt(p); }}
                                    className="text-(--base-06) hover:text-(--error-light) shrink-0 disabled:opacity-30 disabled:hover:text-(--base-06) disabled:cursor-not-allowed"
                                    title={modpacksDisabled ? 'Modpack authoring is disabled' : 'Delete'}
                                    disabled={modpacksDisabled}
                                >
                                    <Trash2 size={12} />
                                </button>
                            </div>
                            {p.summary && <p className="text-xs text-(--base-07) line-clamp-2 mt-2">{p.summary}</p>}
                            <div className="mt-3 flex items-center gap-2 flex-wrap">
                                {p.modrinthProjectId
                                    ? <Badge variant="success">on modrinth</Badge>
                                    : <Badge variant="neutral">local only</Badge>
                                }
                            </div>
                        </Link>
                    ))}
                </div>
            )}

            {/* Packs other users share with the reader. */}
            {!loading && shared.length > 0 && (
                <section className="mt-6">
                    <h2 className="mono-label text-(--base-06) mb-2">Shared with me</h2>
                    <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
                        {shared.map(p => (
                            <Link key={p.id} href={`/modpacks/${p.id}`} className="card p-4 hover:border-(--accent-border) transition-colors group">
                                <div className="flex items-start gap-3">
                                    <div className="w-10 h-10 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
                                        <ProjectIcon name={p.internalName} url={metadata.projects[p.modrinthProjectId]?.icon_url} className="w-10 h-10" />
                                    </div>
                                    <div className="min-w-0 flex-1">
                                        <div className="text-sm font-medium text-(--base-09) truncate group-hover:text-(--accent-light)">{p.internalName}</div>
                                        <div className="text-[10px] font-mono text-(--base-06) truncate">by {p.ownerName}</div>
                                    </div>
                                </div>
                                <div className="mt-3 flex items-center gap-2 flex-wrap">
                                    <Badge variant="neutral">{p.canEdit ? (p.canDelete ? 'edit & delete' : 'edit') : p.canDelete ? 'delete' : 'view'}</Badge>
                                </div>
                            </Link>
                        ))}
                    </div>
                </section>
            )}

            {!loading && packs.length > 0 && (
                <div className="mt-6"><PackActivity showPack /></div>
            )}

            {collabOpen && <PackCollaboratorsModal packs={packs} onClose={() => { setCollabOpen(false); refresh(); }} />}

            {/* Create */}
            {creating && (
                <div className="modal-overlay animate-fade-in" onClick={() => setCreating(null)}>
                    <ModalPanel onClose={() => setCreating(null)} className="modal-panel max-w-lg" onClick={e => e.stopPropagation()}>
                        <div className="modal-header flex items-center justify-between gap-3">
                            <h3 className="modal-title flex items-center gap-2">
                                <Package size={16} />
                                New pack
                            </h3>
                            <button onClick={() => setCreating(null)} aria-label="Close" className="text-(--base-06) hover:text-(--base-09)"><X size={16} /></button>
                        </div>
                        <div className="modal-body space-y-4">
                            {/* Said here, before a pack exists, because it is the
                                one rule that decides what a pack can do LATER: a
                                file uploaded by hand can never be version-checked
                                or carried to a newer Minecraft version. */}
                            <UnlinkedContentWarning context="pack" />
                            {createOn.length > 0 && (
                                <div>
                                    <label className="input-label">Account</label>
                                    <Select
                                        value={creating.ownerId}
                                        onChange={v => setCreating({ ...creating, ownerId: v })}
                                        options={[{ value: '', label: 'Mine' }, ...createOn.map(o => ({ value: o.id, label: `${o.name} (invited)` }))]}
                                        ariaLabel="Account"
                                    />
                                    <p className="text-xs text-(--base-06) mt-1">A pack on someone else&apos;s account is theirs and publishes through their Modrinth token.</p>
                                </div>
                            )}
                            <div>
                                <label htmlFor="new-pack-name" className="input-label">Pack name</label>
                                <input
                                    id="new-pack-name"
                                    autoFocus
                                    type="text"
                                    value={creating.internalName}
                                    onChange={e => setCreating({ ...creating, internalName: e.target.value })}
                                    className="input-field w-full"
                                    placeholder="Skyblock Reborn"
                                    maxLength={128}
                                />
                            </div>
                            <div>
                                <label htmlFor="new-pack-slug" className="input-label">Slug (URL-safe, optional)</label>
                                <input
                                    id="new-pack-slug"
                                    type="text"
                                    value={creating.slug}
                                    onChange={e => setCreating({ ...creating, slug: e.target.value.toLowerCase() })}
                                    className="input-field input-mono w-full"
                                    placeholder="auto-derived from name"
                                />
                                <p className="text-xs text-(--base-06) mt-1">Lowercase, 2-64 chars: letters, digits, - or _</p>
                            </div>
                            <div>
                                <label htmlFor="new-pack-summary" className="input-label">Summary (optional)</label>
                                <input
                                    id="new-pack-summary"
                                    type="text"
                                    value={creating.summary}
                                    onChange={e => setCreating({ ...creating, summary: e.target.value })}
                                    className="input-field w-full"
                                    placeholder="One-line description"
                                    maxLength={255}
                                />
                            </div>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setCreating(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runCreate(handleCreate)} disabled={creatingPack} className="btn btn-primary disabled:opacity-40">Create</button>
                        </div>
                    </ModalPanel>
                </div>
            )}

            {/* Delete */}
            {deletePrompt && (
                <div className="modal-overlay animate-fade-in" onClick={() => setDeletePrompt(null)}>
                    <ModalPanel onClose={() => setDeletePrompt(null)} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <Trash2 size={16} />
                                Delete {deletePrompt.internalName}?
                            </h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                Removes the local copy and all its builds. If this pack was
                                published to Modrinth, the Modrinth project is NOT deleted — manage
                                that on modrinth.com.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setDeletePrompt(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runDelete(handleDelete)} disabled={deletingPack} className="btn btn-danger disabled:opacity-40">Delete</button>
                        </div>
                    </ModalPanel>
                </div>
            )}

        </main>
    );
}
