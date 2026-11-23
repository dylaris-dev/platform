"use client";

import React, { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { useRouter, useSearchParams } from 'next/navigation';
import ProjectIcon, { useProjectMetadata } from '@/components/modpacks/ProjectIcon';
import { Package, Plus, ExternalLink, RefreshCw, CircleAlert, X, Trash2 } from 'lucide-react';
import { systemEvents } from '@/lib/systemEvents';
import { clearModrinthMemberships, createPack, deletePack, type Pack, listPacksOverview, type SharedPack } from '@/lib/api/packs';
import PackActivity from '@/components/modpacks/PackActivity';
import UnlinkedContentWarning from '@/components/mods/UnlinkedContentWarning';
import { useAppData } from '@/lib/AppDataContext';
import { SkeletonCard } from '@/components/Skeleton';
import { Badge } from '@/components/ui/Badge';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import ModalPanel from '@/components/ui/ModalPanel';
import PageHeader from '@/components/ui/PageHeader';
import Section from '@/components/ui/Section';
import EmptyState from '@/components/ui/EmptyState';
import Callout from '@/components/server/Callout';

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
        internalName: string; slug: string; summary: string;
    } | null>(null);
    // Packs others created that the reader reaches through a Modrinth team.
    const [shared, setShared] = useState<SharedPack[]>([]);
    const [modrinth, setModrinth] = useState({ connected: true, tokenInvalid: false });
    const [clearing, runClear] = useBusy();
    const [deletePrompt, setDeletePrompt] = useState<Pack | null>(null);

    const metadata = useProjectMetadata([...packs, ...shared].map(pack => pack.modrinthProjectId));
    useEffect(() => { if (searchParams.get('create') === '1' && !cannotAuthor) setCreating({ internalName: '', slug: '', summary: '' }); }, [searchParams, cannotAuthor]);
    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const o = await listPacksOverview();
        setPacks(o.packs);
        setShared(o.shared);
        setModrinth(o.modrinth);
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

    const handleClear = async () => {
        const res = await clearModrinthMemberships();
        if (res.success) {
            showToast(res.removed ? `Removed ${res.removed} pack${res.removed === 1 ? '' : 's'} from your list.` : 'Nothing to clear.', true);
            refresh();
        } else {
            showToast(res.message || 'Clear failed', false);
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

    const packCard = (p: Pack | SharedPack, trailing: React.ReactNode, menu?: React.ReactNode) => (
        <div key={p.id} className="mp-card">
            <Link href={`/modpacks/${p.id}`} className="mp-card-link">
                <span className="mp-card-icon">
                    <ProjectIcon name={p.internalName} url={metadata.projects[p.modrinthProjectId]?.icon_url} className="size-10" />
                </span>
                <span className="min-w-0 flex-1">
                    <span className="block truncate text-sm font-medium text-(--base-09)">{p.internalName}</span>
                    <span className="srv-num block truncate text-xs text-(--base-06)">/{p.internalSlug}</span>
                </span>
            </Link>
            {p.summary && <p className="line-clamp-2 px-4 text-[13px] text-(--base-07)">{p.summary}</p>}
            <div className="mt-auto flex items-center gap-2 px-4 pb-4 pt-3">{trailing}</div>
            {menu && <div className="mp-card-menu">{menu}</div>}
        </div>
    );

    return (
        <main className="flex-1 min-h-0 overflow-y-auto app-page space-y-6">
            <PageHeader
                title="My modpacks"
                description="Every pack is a Modrinth project. Build here, publish to Modrinth, export an .mrpack or install it on a server."
                actions={<>
                    <button onClick={refresh} className="btn btn-secondary btn-sm" disabled={loading} aria-label="Refresh">
                        <RefreshCw size={13} className={loading ? 'animate-spin' : ''} />
                    </button>
                    <button
                        onClick={() => setCreating({ internalName: '', slug: '', summary: '' })}
                        className="btn btn-primary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                        disabled={cannotAuthor || !modrinth.connected}
                        title={modpacksDisabled ? 'Modpack authoring is disabled' : noStorage ? 'Modpack storage is not configured' : !modrinth.connected ? 'Connect your Modrinth token first' : undefined}
                    >
                        <Plus size={13} />
                        New Pack
                    </button>
                </>}
            />

            {(noStorage || modrinth.tokenInvalid || (!modrinth.connected && !modrinth.tokenInvalid && !cannotAuthor) || modpacksDisabled) && (
                <div className="space-y-2">
                    {noStorage && (
                        <Callout tone="warning" icon={<CircleAlert size={16} />} title="Modpack storage is not configured">
                            Archives have nowhere to go, so nothing can be created yet.
                            {user?.isAdmin ? (
                                <>
                                    {' '}Pick local paths or a storage connection under{' '}
                                    <Link href="/settings/modpacks" className="text-(--accent-light)">Settings, Modpacks</Link>.
                                </>
                            ) : (
                                ' Ask an administrator to configure it.'
                            )}
                        </Callout>
                    )}
                    {modrinth.tokenInvalid && (
                        <Callout
                            tone="warning"
                            icon={<CircleAlert size={16} />}
                            title="Modrinth no longer accepts your token"
                            actions={<>
                                <Link href="/account/modrinth" className="btn btn-primary btn-sm">Renew token</Link>
                                <button onClick={() => runClear(handleClear)} disabled={clearing} className="btn btn-secondary btn-sm disabled:opacity-40">Clear</button>
                            </>}
                        >
                            Packs you reach through a Modrinth team are read only for a day, then locked. Renew the token to work on them
                            again, or clear the ones you no longer have access to from your list.
                        </Callout>
                    )}
                    {!modrinth.connected && !modrinth.tokenInvalid && !cannotAuthor && (
                        <Callout tone="info" icon={<CircleAlert size={16} />}>
                            Every pack is a Modrinth project, so a new pack needs your Modrinth token.{' '}
                            <Link href="/account/modrinth" className="text-(--accent-light)">Connect it under Account, Modrinth</Link>.
                        </Callout>
                    )}
                    {modpacksDisabled && (
                        <Callout tone="warning" icon={<CircleAlert size={16} />}>
                            Modpack authoring is disabled by the platform admin. Existing packs remain readable and downloadable.
                        </Callout>
                    )}
                </div>
            )}

            <Section
                title="Your packs"
                description={<>Who may work on a pack is its Modrinth team, managed on modrinth.com. <Link href="/account/modrinth" className="inline-flex items-center gap-1 text-(--accent-light)">Connect your token <ExternalLink size={10} /></Link></>}
            >
                {loading ? (
                    <div className="mp-grid" aria-busy="true">
                        {Array.from({ length: 6 }).map((_, i) => (
                            <SkeletonCard key={i} height="h-28" />
                        ))}
                    </div>
                ) : packs.length === 0 ? (
                    <EmptyState icon={<Package size={20} />} title="No packs yet" description="Create your first one to get started." />
                ) : (
                    <div className="mp-grid">
                        {packs.map(p => packCard(
                            p,
                            p.modrinthProjectId
                                ? <Badge variant="success">on modrinth</Badge>
                                : <Badge variant="neutral">local only</Badge>,
                            <button
                                onClick={() => setDeletePrompt(p)}
                                className="app-icon-btn"
                                aria-label={`Delete ${p.internalName}`}
                                title={modpacksDisabled ? 'Modpack authoring is disabled' : 'Delete'}
                                disabled={modpacksDisabled}
                            >
                                <Trash2 size={14} />
                            </button>,
                        ))}
                    </div>
                )}
            </Section>

            {/* Packs the reader reaches through a Modrinth team. */}
            {!loading && shared.length > 0 && (
                <Section title="Modrinth team" description="Packs of Modrinth teams you are in.">
                    <div className="mp-grid">
                        {shared.map(p => packCard(
                            p,
                            p.tokenInvalid
                                ? <Badge variant="warning">renew token</Badge>
                                : <Badge variant="neutral">{p.canEdit ? (p.canDelete ? 'edit & delete' : 'edit') : p.canDelete ? 'delete' : 'view'}</Badge>,
                        ))}
                    </div>
                </Section>
            )}

            {!loading && packs.length > 0 && (
                <PackActivity showPack />
            )}

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
                                Removes the pack and all its builds here, for everyone on its
                                Modrinth team. The Modrinth project itself is NOT deleted; manage
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
