"use client";

import React, { useCallback, useEffect, useState } from 'react';

import Link from 'next/link';
import { useRouter } from 'next/navigation';
import {
    ArrowLeft, Trash2, Search,
    Upload, Lock,
    ExternalLink, ChevronDown, Share2, X, Loader2, Replace, AlertTriangle, ArrowUpCircle, FileText, Copy,
} from 'lucide-react';
import { systemEvents } from '@/lib/systemEvents';
import {
    listBuilds, listContent, addModrinthContent, removeContent, setContentSide,
    getPackDetails, modrinthTypeToContentType, type Pack, type PackBuild, type BuildContentEntry,
} from '@/lib/api/packs';
import { publishModrinth, replaceWithModrinth, updateMods } from '@/lib/api/packsPublish';
import { ModrinthProjectActions } from '@/components/mods/ModrinthLibrary';
import PackConfigFiles from '@/components/modpacks/PackConfigFiles';
import { createShareLink, listShareLinks, revokeShareLink, publicShareUrl, type ShareLink, type ShareLinkKind } from '@/lib/api/packsShare';
import { useAppData } from '@/lib/AppDataContext';
import { SkeletonHeader, SkeletonList } from '@/components/Skeleton';
import ModrinthVersionBrowser from '@/components/modrinth/ModrinthVersionBrowser';
import ConfigEditorModal from '@/components/modpacks/ConfigEditorModal';
import UploadContentModal from '@/components/modpacks/UploadContentModal';
import { Badge } from '@/components/ui/Badge';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';
import ModalPanel from '@/components/ui/ModalPanel';
import { fmtDate } from '@/lib/time';
import { WorkspaceDetails } from '@/components/modpacks/PackWorkspace';
import ProjectIcon, { useProjectMetadata } from '@/components/modpacks/ProjectIcon';
import { packContentGroup } from '@/lib/packWorkspace';
import { filterPackContent, type PackContentSort, type PackEnvironmentFilter as EnvironmentFilterValue } from '@/lib/packContent';
import { PackEnvironmentSelector, PackEnvironmentFilter } from '@/components/modpacks/PackEnvironmentControls';
import ModProjectDetails from '@/components/mods/ModProjectDetails';
import PackDownloadDialog from '@/components/modpacks/PackDownloadDialog';
import ForkBuildDialog from '@/components/modpacks/ForkBuildDialog';
import DeleteBuildButton from '@/components/modpacks/DeleteBuildButton';
import styles from '@/components/modpacks/workspace.module.css';

// Build editing keeps the pack/build navigation mounted in the workspace.


// A linked entry has an update available when Modrinth's cached latest version
// differs from what's currently installed. modrinthLatestVersionId is only
// populated once the auto-update cron has checked the entry at least once.
function hasUpdateAvailable(entry: BuildContentEntry): boolean {
    return !!(entry.linked && entry.modrinthLatestVersionId && entry.modrinthLatestVersionId !== entry.modrinthVersionId);
}

// ---------------------------------------------------------------------------
// Publish dialog
// ---------------------------------------------------------------------------
interface PublishDialogProps {
    build: PackBuild;
    onClose: () => void;
    onPublished: () => void;
    showToast: (msg: string, ok?: boolean) => void;
    packId: number;
}

function PublishDialog({ build, onClose, onPublished, showToast, packId }: PublishDialogProps) {
    const [channel, setChannel] = useState<'beta' | 'release'>(
        build.channel === 'release' ? 'release' : 'beta',
    );
    const [busy, setBusy] = useState(false);
    const [warnings, setWarnings] = useState<string[]>([]);
    const [needsAck, setNeedsAck] = useState(false);

    const submit = async (ackNonModrinth = false) => {
        setBusy(true);
        setWarnings([]);
        const res = await publishModrinth(packId, build.id, { channel, ackNonModrinth });
        setBusy(false);
        if (res.success) {
            showToast(res.message || 'Published to Modrinth.', true);
            onPublished();
            onClose();
        } else if (!res.success && res.warnings && res.warnings.length > 0) {
            // 409 gate: non-Modrinth content warning
            setWarnings(res.warnings);
            setNeedsAck(true);
        } else {
            showToast(res.message || 'Publish failed.', false);
        }
    };

    // Close on Escape
    return (
        <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
            onClick={onClose}
        >
            <ModalPanel onClose={onClose}
                className="card w-full max-w-md mx-4"
                onClick={e => e.stopPropagation()}
            >
                <div className="modal-header flex items-center justify-between">
                    <h3 className="modal-title">Publish</h3>
                    <button
                        type="button"
                        onClick={onClose}
                        className="text-(--base-07) hover:text-(--error-light) transition-colors"
                        aria-label="Close"
                    >
                        <X size={18} />
                    </button>
                </div>

                <div className="p-6 space-y-4">
                    {/* Channel selector */}
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">Release channel</label>
                        <select
                            value={channel}
                            onChange={e => setChannel(e.target.value as 'beta' | 'release')}
                            className="input-field w-full"
                            disabled={busy}
                        >
                            <option value="beta">Beta</option>
                            <option value="release">Release</option>
                        </select>
                    </div>

                    {/* Non-Modrinth content warnings */}
                    {needsAck && warnings.length > 0 && (
                        <div className="rounded-md border border-(--warning) bg-(--warning-ghost) p-3 space-y-2">
                            <div className="flex items-center gap-2 text-(--warning-light) text-xs font-medium">
                                <AlertTriangle size={13} />
                                Non-Modrinth content detected
                            </div>
                            <ul className="space-y-1">
                                {warnings.map((w, i) => (
                                    <li key={i} className="text-xs text-(--base-07) font-mono pl-1">
                                        {w}
                                    </li>
                                ))}
                            </ul>
                            <p className="text-xs text-(--base-06)">
                                These files will be embedded in the .mrpack under overrides/.
                                Ensure you have redistribution rights before publishing.
                            </p>
                        </div>
                    )}

                    {/* Publish targets */}
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label">Publish targets</label>
                        <div className="space-y-2">
                            <label className="flex items-center gap-2 cursor-pointer select-none">
                                <input
                                    type="checkbox"
                                    checked
                                    readOnly
                                    className="checkbox"
                                />
                                <span className="text-sm">Modrinth</span>
                            </label>
                        </div>
                    </div>
                </div>

                <div className="modal-footer flex gap-2 justify-end">
                    <button
                        type="button"
                        onClick={onClose}
                        disabled={busy}
                        className="btn btn-secondary disabled:opacity-40 disabled:cursor-not-allowed"
                    >
                        Cancel
                    </button>
                    {needsAck ? (
                        <button
                            type="button"
                            onClick={() => submit(true)}
                            disabled={busy}
                            className="btn btn-primary inline-flex items-center gap-2 disabled:opacity-40 disabled:cursor-not-allowed"
                        >
                            {busy && <Loader2 size={14} className="animate-spin" />}
                            Publish anyway
                        </button>
                    ) : (
                        <button
                            type="button"
                            onClick={() => submit(false)}
                            disabled={busy}
                            className="btn btn-primary inline-flex items-center gap-2 disabled:opacity-40 disabled:cursor-not-allowed"
                        >
                            {busy && <Loader2 size={14} className="animate-spin" />}
                            Publish
                        </button>
                    )}
                </div>
            </ModalPanel>
        </div>
    );
}

// ---------------------------------------------------------------------------
// Replace-with-Modrinth dialog (per row)
// ---------------------------------------------------------------------------
interface ReplaceDialogProps {
    entry: BuildContentEntry;
    build: PackBuild;
    packId: number;
    disabled: boolean;
    isFrozen: boolean;
    onClose: () => void;
    onReplaced: () => void;
    showToast: (msg: string, ok?: boolean) => void;
}

function ReplaceDialog({ entry, build, packId, disabled, isFrozen, onClose, onReplaced, showToast }: ReplaceDialogProps) {
    // Close on Escape
    return (
        <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
            onClick={onClose}
        >
            <ModalPanel onClose={onClose}
                className="card w-full max-w-2xl mx-4 flex flex-col"
                style={{ maxHeight: 'min(90vh, 720px)' }}
                onClick={e => e.stopPropagation()}
            >
                <div className="modal-header flex items-center justify-between shrink-0">
                    <div>
                        <h3 className="modal-title">Replace with Modrinth</h3>
                        <p className="text-xs text-(--base-06) mt-0.5">
                            Replacing <span className="font-mono text-(--accent-light)">{entry.prettyName || entry.modSlug}</span>
                        </p>
                    </div>
                    <button
                        type="button"
                        onClick={onClose}
                        className="text-(--base-07) hover:text-(--error-light) transition-colors"
                        aria-label="Close"
                    >
                        <X size={18} />
                    </button>
                </div>

                <div className="flex-1 overflow-hidden p-4">
                    <ModrinthVersionBrowser
                        loader={build.loader || undefined}
                        mcVersion={build.minecraft || undefined}
                        projectType={entry.contentType === 'resourcepack' ? 'resourcepack' : entry.contentType === 'shaderpack' ? 'shader' : 'mod'}
                        disabled={disabled}
                        disabledTitle={isFrozen ? 'Build is frozen' : 'Modpack authoring is disabled'}
                        onPick={(projectId, versionId) => {
                            replaceWithModrinth(packId, build.id, entry.id, versionId).then(r => {
                                showToast(r.success ? 'Replaced' : (r.message || 'Replace failed'), r.success);
                                if (r.success) {
                                    onReplaced();
                                    onClose();
                                }
                            });
                        }}
                    />
                </div>
            </ModalPanel>
        </div>
    );
}

// ---------------------------------------------------------------------------
// Update-mods dialog (per row) — same scaffold as ReplaceDialog, but hits the
// update-mods endpoint scoped to a single modversion instead of the generic
// replace endpoint.
// ---------------------------------------------------------------------------
interface UpdateModsDialogProps {
    entry: BuildContentEntry;
    build: PackBuild;
    packId: number;
    disabled: boolean;
    isFrozen: boolean;
    onClose: () => void;
    onUpdated: () => void;
    showToast: (msg: string, ok?: boolean) => void;
}

function UpdateModsDialog({ entry, build, packId, disabled, isFrozen, onClose, onUpdated, showToast }: UpdateModsDialogProps) {
    // Close on Escape
    return (
        <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
            onClick={onClose}
        >
            <ModalPanel onClose={onClose}
                className="card w-full max-w-2xl mx-4 flex flex-col"
                style={{ maxHeight: 'min(90vh, 720px)' }}
                onClick={e => e.stopPropagation()}
            >
                <div className="modal-header flex items-center justify-between shrink-0">
                    <div>
                        <h3 className="modal-title">Upgrade</h3>
                        <p className="text-xs text-(--base-06) mt-0.5">
                            Choose a version for <span className="font-mono text-(--accent-light)">{entry.prettyName || entry.modSlug}</span>
                        </p>
                    </div>
                    <button
                        type="button"
                        onClick={onClose}
                        className="text-(--base-07) hover:text-(--error-light) transition-colors"
                        aria-label="Close"
                    >
                        <X size={18} />
                    </button>
                </div>

                <div className="flex-1 overflow-hidden p-4">
                    <ModrinthVersionBrowser
                        loader={build.loader || undefined}
                        mcVersion={build.minecraft || undefined}
                        projectType={entry.contentType === 'resourcepack' ? 'resourcepack' : entry.contentType === 'shaderpack' ? 'shader' : 'mod'}
                        disabled={disabled}
                        disabledTitle={isFrozen ? 'Build is frozen' : 'Modpack authoring is disabled'}
                        onPick={(projectId, versionId) => {
                            updateMods(packId, build.id, { modversionId: entry.id, versionId }).then(res => {
                                // UpdateMods returns success:true unconditionally once past its guards;
                                // a failed single swap still yields success:true with upgraded:0 and the
                                // real error in results[0]. Only treat it as success when something upgraded.
                                const ok = !!(res.success && res.upgraded && res.upgraded > 0);
                                showToast(ok ? 'Upgraded' : (res.results?.[0]?.error || res.message || 'Upgrade failed'), ok);
                                if (ok) {
                                    onUpdated();
                                    onClose();
                                }
                            });
                        }}
                    />
                </div>
            </ModalPanel>
        </div>
    );
}

// ---------------------------------------------------------------------------
// Main page
// ---------------------------------------------------------------------------
export default function BuildContentEditorPage() {
    const paramId = useRouteId('modpacks');
    const paramBuildId = useRouteId('builds');
    const packId = Number(paramId);
    const buildId = Number(paramBuildId);
    const { featureFlags } = useAppData();
    const modpacksDisabled = !featureFlags.modpacks;

    const [pack, setPack] = useState<Pack | null>(null);
    const [build, setBuild] = useState<PackBuild | null>(null);
    const [builds,setBuilds]=useState<PackBuild[]>([]);
    const [content, setContent] = useState<BuildContentEntry[]>([]);
    const [loading, setLoading] = useState(true);

    // Upload state
    const [uploadOpen, setUploadOpen] = useState(false);
    const [canEdit, setCanEdit] = useState(false);
    const [canDelete, setCanDelete] = useState(false);
    const [addingContent, setAddingContent] = useState(false);

    // Modrinth search state
    const [modrinthType, setModrinthType] = useState<'mod' | 'resourcepack' | 'shader'>('mod');

    // Export state
    const [forkOpen,setForkOpen]=useState(false);
    const router=useRouter();
    const [downloadKind, setDownloadKind] = useState<'client' | 'server' | null>(null);
    const [expandedEntry, setExpandedEntry] = useState<number | null>(null);
    const [environment, setEnvironment] = useState<EnvironmentFilterValue>('all');
    const [sort, setSort] = useState<PackContentSort>('name');
    const [activeTab, setActiveTab] = useState<'mods' | 'resources' | 'configs' | 'tools'>('mods');
    const [browsing, setBrowsing] = useState(false);
    const [query, setQuery] = useState('');
    const projectMetadata = useProjectMetadata(content.map(entry => entry.modrinthProjectId));

    // Dialog state
    const [publishDialogOpen, setPublishDialogOpen] = useState(false);
    const [replaceEntry, setReplaceEntry] = useState<BuildContentEntry | null>(null);
    const [upgradeEntry, setUpgradeEntry] = useState<BuildContentEntry | null>(null);
    const [updatingAll, setUpdatingAll] = useState(false);
    const [editing, setEditing] = useState<{ modversionId: number; title: string } | null>(null);

    // Share-link state
    const [shareLinks, setShareLinks] = useState<ShareLink[]>([]);
    const [shareKind, setShareKind] = useState<ShareLinkKind>('client-mrpack');
    const [shareExpiry, setShareExpiry] = useState(7);
    const [creatingShare, setCreatingShare] = useState(false);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const [p, bs, list, sl] = await Promise.all([
            getPackDetails(packId),
            listBuilds(packId),
            listContent(packId, buildId),
            listShareLinks(packId, buildId).catch(() => ({ success: false, links: [] as ShareLink[] })),
        ]);
        setPack(p.pack);
        setCanEdit(p.permissions.canEdit);
        setCanDelete(p.permissions.canDelete);
        setBuild(bs.find(b => b.id === buildId) || null);
        setBuilds(bs);
        setContent(list);
        setShareLinks(sl.links || []);
        setLoading(false);
    }, [packId, buildId]);

    useEffect(() => { refresh(); }, [refresh]);

    useEffect(() => {
        if (build?.loader === 'vanilla' && modrinthType === 'mod') setModrinthType('resourcepack');
    }, [build?.loader, modrinthType]);

    useEffect(() => {
        const unsubC = systemEvents.on('pack_content.changed', (evt) => {
            const bid = (evt.payload as any)?.buildId;
            if (bid === undefined || bid === buildId) refresh();
        });
        const unsubB = systemEvents.on('pack_builds.changed', () => { refresh(); });
        return () => { unsubC(); unsubB(); };
    }, [buildId, refresh]);

    const isFrozen = !!build?.frozen;
    const disabled = modpacksDisabled || isFrozen || !canEdit;
    const deleteDisabled = modpacksDisabled || isFrozen || !canDelete;
    const editDisabledReason = isFrozen ? 'Build is frozen' : modpacksDisabled ? 'Modpack authoring is disabled' : 'You do not have edit access to this pack';
    const hasAnyUpdate = content.some(hasUpdateAvailable);

    const handleSetSide = async (entry: BuildContentEntry, side: string) => {
        const res = await setContentSide(packId, buildId, entry.id, side);
        if (res.success) {
            await refresh();
        } else {
            showToast(res.message || 'Failed to set side', false);
        }
    };

    const handleRemove = async (entry: BuildContentEntry) => {
        const res = await removeContent(packId, buildId, entry.id);
        if (res.success) {
            showToast(`Removed ${contentName(entry)}`, true);
            refresh();
        } else {
            showToast(res.message || 'Remove failed', false);
        }
    };

    const handleUpdateAll = async () => {
        setUpdatingAll(true);
        const res = await updateMods(packId, buildId, { all: true });
        setUpdatingAll(false);
        if (!res.success) {
            showToast(res.message || 'Update all failed', false);
            return;
        }
        const failed = (res.results || []).filter(r => r.error);
        const upgraded = res.upgraded ?? ((res.results?.length || 0) - failed.length);
        if (failed.length > 0) {
            showToast(`${upgraded} upgraded, ${failed.length} failed`, false);
        } else {
            showToast(`${upgraded} upgraded`, true);
        }
        refresh();
    };

    const handleCreateShareLink = async () => {
        setCreatingShare(true);
        try {
            const res = await createShareLink(packId, buildId, shareKind, shareExpiry);
            if (res.success) { showToast('Share link created'); refresh(); }
            else showToast(res.message || 'Create failed', false);
        } catch { showToast('Create failed', false); }
        finally { setCreatingShare(false); }
    };

    const handleCopyShareLink = async (token: string) => {
        try {
            await navigator.clipboard.writeText(publicShareUrl(token));
            showToast('Copied to clipboard');
        } catch { showToast('Copy failed', false); }
    };

    const handleRevokeShareLink = async (linkId: number) => {
        try {
            const res = await revokeShareLink(packId, buildId, linkId);
            if (res.success) { showToast('Link revoked'); refresh(); }
            else showToast(res.message || 'Revoke failed', false);
        } catch { showToast('Revoke failed', false); }
    };

    const contentName = (entry: BuildContentEntry) => projectMetadata.projects[entry.modrinthProjectId]?.title || entry.prettyName || entry.modSlug;
    const groups = { mods: content.filter(entry => packContentGroup(entry.contentType) === 'mods'), resources: content.filter(entry => packContentGroup(entry.contentType) === 'resources'), configs: content.filter(entry => packContentGroup(entry.contentType) === 'configs') };
    const visibleContent = activeTab === 'tools' ? [] : filterPackContent(groups[activeTab], query, environment, sort, contentName);
    if (loading) return <div className={styles.editor}><SkeletonHeader /><SkeletonList rows={8} /></div>;
    if (!build || !pack) return <div className="p-6 text-sm text-(--base-06)">Build not found. <Link href={`/modpacks/${packId}`} className="text-(--accent-light)">Back to pack</Link></div>;
    return <div className={styles.editor}>
        <WorkspaceDetails>
            <div className="flex items-center justify-between gap-2 mb-4"><span className="mono-label text-(--base-06)">Build details</span>{isFrozen && <Lock size={13} aria-label="Frozen" />}</div>
            <h2 className="text-xl font-mono text-(--base-09) mb-4 break-all">{build.versionString}</h2>
            <dl className={styles.metadata}><div><dt>Minecraft</dt><dd>{build.minecraft || 'Unspecified'}</dd></div><div><dt>Loader</dt><dd>{build.loader || 'Vanilla'}</dd></div><div><dt>Loader build</dt><dd>{build.loaderVersion || 'None'}</dd></div><div><dt>Mods / plugins</dt><dd>{groups.mods.length}</dd></div><div><dt>Resource / shader packs</dt><dd>{groups.resources.length}</dd></div><div><dt>Config files / bundles</dt><dd>{groups.configs.length}</dd></div><div><dt>Channel</dt><dd>{build.channel}</dd></div><div><dt>Modrinth</dt><dd>{build.modrinthPublished ? 'Published' : 'Not published'}</dd></div></dl>
            <div className="flex flex-col gap-2 mt-5">
                <button onClick={()=>setForkOpen(true)} disabled={modpacksDisabled} className="btn btn-secondary btn-sm">Fork build into another pack</button>
                <button onClick={() => setDownloadKind('client')} className="btn btn-secondary btn-sm">Download client .mrpack</button>
                <button onClick={() => setDownloadKind('server')} className="btn btn-secondary btn-sm">Download server ZIP</button>
                <button onClick={() => setPublishDialogOpen(true)} disabled={modpacksDisabled || !canEdit || build.modrinthPublished} className="btn btn-primary btn-sm disabled:opacity-40"><Share2 size={12} />{build.modrinthPublished ? 'Published to Modrinth' : isFrozen ? 'Retry publication' : 'Publish'}</button>
                {canDelete && !modpacksDisabled && <DeleteBuildButton packId={packId} build={build} onDeleted={() => router.push(`/modpacks/${packId}`)} />}
                {canEdit && !modpacksDisabled && <Link href={`/modpacks/${packId}/builds/new`} className="btn btn-secondary btn-sm">Create another build</Link>}
            </div>
        </WorkspaceDetails>
        {isFrozen && <div role="status" className="rounded-md border border-(--base-04) p-3 text-xs text-(--base-07)"><Lock size={13} className="inline mr-2" />This build is read-only. {build.modrinthPublished ? 'Its published contents stay reproducible.' : 'Its publication artifact has already been frozen; you can retry publishing it.'} Create or fork a new build to make changes.</div>}
        <header className="flex items-center justify-between gap-3 flex-wrap shrink-0"><div><h2 className="text-base font-medium text-(--base-09)">{browsing ? 'Add from Modrinth' : `Build ${build.versionString}`}</h2><p className="text-xs text-(--base-06) mt-1">{groups.mods.length} mods / plugins · {groups.configs.length} configs · MC {build.minecraft} · {build.loader}</p></div><div className={styles.tools}>
            {hasAnyUpdate && <button onClick={handleUpdateAll} disabled={disabled || updatingAll} className="btn btn-secondary btn-sm"><ArrowUpCircle size={12} /> {updatingAll ? 'Updating...' : 'Update all'}</button>}
            <button onClick={() => setUploadOpen(true)} disabled={disabled} title={disabled ? editDisabledReason : 'Upload files or folders'} className="btn btn-secondary btn-sm disabled:opacity-40"><Upload size={12} /> Upload</button>
            <button onClick={() => setBrowsing(value => !value)} disabled={disabled && !browsing} className="btn btn-primary btn-sm disabled:opacity-40">{browsing ? <ArrowLeft size={12} /> : <Search size={12} />}{browsing ? 'Installed' : 'Add content'}</button>
        </div></header>
        {isFrozen && <p className="text-xs text-(--base-06) bg-(--base-02) p-3 rounded-md">This build is frozen. Create a copy to change its content.</p>}
        {browsing ? <section className={styles.searchPanel} aria-label="Add from Modrinth"><label className="flex items-center gap-3 text-xs text-(--base-06) mb-3">Project type<select value={modrinthType} onChange={event => setModrinthType(event.target.value as typeof modrinthType)} className="input-field text-xs w-auto"><option value="mod" disabled={build.loader === 'vanilla'}>Mods</option><option value="resourcepack">Resource packs</option><option value="shader">Shaders</option></select></label>
            <ModrinthVersionBrowser loader={build.loader || undefined} mcVersion={build.minecraft || undefined} projectType={modrinthType} installedProjectIds={new Set(content.map(entry => entry.modrinthProjectId).filter(Boolean))} disabled={disabled || addingContent} disabledTitle={addingContent ? 'Adding content...' : editDisabledReason} onPick={async (projectId, versionId, hit) => { if (disabled || addingContent) return; setAddingContent(true); const contentType = modrinthTypeToContentType(hit.project_type); const result = await addModrinthContent(packId, buildId, { projectId, versionId, contentType, resolveDeps: contentType === 'mod' }); setAddingContent(false); if (result.success) { showToast(`Added ${hit.title}`); refresh(); } else showToast(result.message || 'Add failed', false); }} />
        </section> : <>
            <nav className={styles.tabs} role="tablist" aria-label="Build content">{([['mods', 'Mods', groups.mods.length], ['resources', 'Resources', groups.resources.length], ['configs', 'Configs & scripts', groups.configs.length], ['tools', 'Sharing', null]] as const).map(([id, label, count]) => <button key={id} className={styles.tab} role="tab" aria-selected={activeTab === id} onClick={() => { setActiveTab(id); setQuery(''); setExpandedEntry(null); }}>{label}{count !== null && <span className="text-(--base-06)">{count}</span>}</button>)}</nav>
            {activeTab === 'configs' ? <PackConfigFiles packId={packId} buildId={buildId} canEdit={!disabled} canDelete={!deleteDisabled} onChanged={refresh}/> : activeTab === 'tools' ? <div className={styles.rows}>
                <p className="text-xs text-(--base-06) mb-4">Share links grant access to this build without a panel account. Use the download buttons for private downloads, Publish for Modrinth, and New build for Minecraft or loader changes.</p>
                            {!featureFlags.shareLinks && shareLinks.length === 0 && <div className="card p-4 text-sm text-(--base-06)">Public share links are disabled by the administrator. Use the private client/server download buttons in Build details.</div>}
                {(featureFlags.shareLinks || shareLinks.length > 0) && (
                <section className="card p-4 mb-4">
                    {isFrozen && <div role="status" className="rounded-md border border-(--base-04) p-3 text-xs text-(--base-07)"><Lock size={13} className="inline mr-2" />This build is read-only. {build.modrinthPublished ? 'Its published contents stay reproducible.' : 'Its publication artifact has already been frozen; you can retry publishing it.'} Create or fork a new build to make changes.</div>}
        <header className="flex items-center gap-2 mb-3">
                        <Share2 size={14} className="text-(--accent-light)" />
                        <h2 className="text-sm font-medium text-(--base-09)">Share links</h2>
                    </header>

                    {featureFlags.shareLinks ? (
                        <div className="flex flex-wrap items-end gap-3 mb-3">
                            <label className="flex flex-col gap-1">
                                <span className="mono-label text-(--base-06)">Kind</span>
                                <select
                                    value={shareKind}
                                    onChange={e => setShareKind(e.target.value as ShareLinkKind)}
                                    className="input-field text-sm"
                                >
                                    <option value="client-mrpack">Client (.mrpack)</option>
                                    <option value="server-pack">Server pack (.zip)</option>
                                </select>
                            </label>
                            <label className="flex flex-col gap-1">
                                <span className="mono-label text-(--base-06)">Expires (days, 0 = never)</span>
                                <input
                                    type="number"
                                    min={0}
                                    value={shareExpiry}
                                    onChange={e => setShareExpiry(Math.max(0, Number(e.target.value) || 0))}
                                    className="input-field text-sm w-40"
                                />
                            </label>
                            <button
                                type="button"
                                onClick={handleCreateShareLink}
                                disabled={creatingShare || !canEdit || modpacksDisabled}
                                className="btn btn-primary btn-sm inline-flex items-center gap-1.5 disabled:opacity-40 disabled:cursor-not-allowed"
                            >
                                {creatingShare ? <Loader2 size={12} className="animate-spin" /> : <Share2 size={12} />}
                                Create link
                            </button>
                        </div>
                    ) : (
                        <p className="text-xs text-(--base-06) mb-3">
                            Creating new share links is disabled by the platform admin. Existing links stay usable.
                        </p>
                    )}

                    {shareLinks.length === 0 ? (
                        <p className="text-xs text-(--base-06)">No share links yet.</p>
                    ) : (
                        <ul className="space-y-2">
                            {shareLinks.map(link => (
                                <li key={link.id} className="flex items-center gap-2 text-xs">
                                    <Badge variant={link.kind === 'server-pack' ? 'accent' : 'success'} className="shrink-0">
                                        {link.kind === 'server-pack' ? 'Server' : 'Client'}
                                    </Badge>
                                    <code className="font-mono text-(--base-07) truncate flex-1">{publicShareUrl(link.token)}</code>
                                    {link.revoked ? (
                                        <Badge variant="neutral" className="shrink-0">revoked</Badge>
                                    ) : (
                                        <>
                                            {link.expiresAt && (
                                                <span className="text-(--base-06) shrink-0">
                                                    expires {fmtDate(link.expiresAt)}
                                                </span>
                                            )}
                                            <button
                                                type="button"
                                                onClick={() => handleCopyShareLink(link.token)}
                                                title="Copy link"
                                                className="btn btn-secondary btn-sm inline-flex items-center gap-1"
                                            >
                                                <Copy size={11} /> Copy
                                            </button>
                                            <button
                                                type="button"
                                                onClick={() => handleRevokeShareLink(link.id)}
                                                disabled={!canEdit}
                                                title="Revoke link"
                                                className="btn btn-secondary btn-sm text-(--error-light)"
                                            >
                                                <X size={11} />
                                            </button>
                                        </>
                                    )}
                                </li>
                            ))}
                        </ul>
                    )}
                </section>
            )}


            </div> : <>
                <input className="input-field w-full text-sm" aria-label="Search installed content" placeholder="Search installed content..." value={query} onChange={event => setQuery(event.target.value)} />
                <div className="flex items-center justify-between gap-3 flex-wrap">
                    <PackEnvironmentFilter value={environment} onChange={setEnvironment} />
                    <label className="flex items-center gap-2 text-xs text-(--base-06)">Sort<select aria-label="Sort installed content" className="input-field text-xs w-auto" value={sort} onChange={event => setSort(event.target.value as PackContentSort)}><option value="name">Name A–Z</option><option value="name-desc">Name Z–A</option><option value="environment">Environment</option><option value="updates">Updates first</option></select></label>
                </div>
                {projectMetadata.error && <p className="text-xs text-(--base-06)">Some project icons could not be loaded. <button className="text-(--accent-light)" onClick={projectMetadata.retry}>Retry icons</button></p>}
                <div className="flex justify-between text-[10px] uppercase tracking-wider text-(--base-06)"><span>Installed · {visibleContent.length}</span><span>Environment / actions</span></div>
                <section className={styles.rows} aria-label="Installed content">
                    {visibleContent.length === 0 && <p className="text-sm text-(--base-06) text-center py-12">{query ? 'No matching content.' : 'No content in this category yet.'}</p>}
                    {visibleContent.map(entry => <article key={entry.id} className="card p-3 mb-2.5">
                        <div className="flex items-start gap-3">
                            <ProjectIcon name={contentName(entry)} contentType={entry.contentType} url={projectMetadata.projects[entry.modrinthProjectId]?.icon_url} className="w-11 h-11" />
                            <div className="min-w-0 flex-1">
                                <div className="flex items-start gap-2">
                                    <button onClick={() => setExpandedEntry(value => value === entry.id ? null : entry.id)} aria-expanded={expandedEntry === entry.id} aria-label={`Details for ${contentName(entry)}`} className="min-w-0 flex-1 flex gap-2 items-center text-left text-sm font-medium text-(--base-09) hover:text-(--accent-light)"><span className="truncate" title={contentName(entry)}>{contentName(entry)}</span><ChevronDown size={13} className={`shrink-0 ${expandedEntry === entry.id ? 'rotate-180' : ''}`} /></button>
                                    {entry.modrinthProjectId && <a href={`https://modrinth.com/project/${encodeURIComponent(entry.modrinthProjectId)}`} target="_blank" rel="noopener noreferrer" title="Open on Modrinth" aria-label={`Open ${contentName(entry)} on Modrinth`} className="btn btn-ghost btn-sm"><ExternalLink size={13} /></a>}
                                    <ModrinthProjectActions projectId={entry.modrinthProjectId} title={contentName(entry)} loader={build.loader}/><button onClick={() => handleRemove(entry)} disabled={deleteDisabled} title={deleteDisabled ? 'Delete access is required; frozen builds cannot be changed' : 'Remove content'} aria-label={`Remove ${contentName(entry)}`} className="btn btn-ghost btn-sm disabled:opacity-30"><Trash2 size={13} className="text-(--error-light)" /></button>
                                </div>
                                <p className="text-[11px] text-(--base-06) font-mono truncate mt-1" title={entry.targetPath}>{entry.contentType === 'config' ? entry.targetPath : `${entry.modrinthVersionNumber || entry.version} · ${entry.linked ? 'Modrinth' : 'Upload'}`}</p>
                                <div className="flex items-center gap-2 flex-wrap mt-3">
                                    <PackEnvironmentSelector value={entry.side} disabled={disabled} name={contentName(entry)} onChange={side => handleSetSide(entry, side)} />
                                    {entry.contentType === 'config' && entry.targetPath && !entry.targetPath.toLowerCase().endsWith('.zip') && !disabled && <button onClick={() => setEditing({ modversionId: entry.id, title: contentName(entry) })} aria-label={`Edit ${contentName(entry)}`} title="Edit config text" className="btn btn-secondary btn-sm"><FileText size={13} /> Edit</button>}
                                    {!entry.linked && entry.contentType !== 'config' && <button onClick={() => setReplaceEntry(entry)} disabled={disabled} aria-label={`Link ${contentName(entry)} to Modrinth`} title="Replace with Modrinth" className="btn btn-secondary btn-sm"><Replace size={13} /> Link</button>}
                                    {hasUpdateAvailable(entry) && <button onClick={() => setUpgradeEntry(entry)} disabled={disabled} title="Update available" aria-label={`Update ${contentName(entry)}`} className="btn btn-secondary btn-sm text-(--accent-light)"><ArrowUpCircle size={13} /> Update</button>}
                                </div>
                            </div>
                        </div>
                        {expandedEntry === entry.id && <div className="mt-4 pt-4 border-t border-(--base-03)">
                            {entry.modrinthProjectId ? <ModProjectDetails projectId={entry.modrinthProjectId} /> : <div className="text-xs text-(--base-06) space-y-2"><p>This file was uploaded directly. {entry.contentType !== 'config' ? 'Link it to Modrinth to show the project description and available versions.' : 'Use Edit to inspect or change this configuration file.'}</p><dl className={styles.metadata}><div><dt>Path</dt><dd>{entry.targetPath || entry.modSlug}</dd></div><div><dt>Version</dt><dd>{entry.version}</dd></div><div><dt>Type</dt><dd>{entry.contentType}</dd></div><div><dt>Environment</dt><dd>{entry.side}</dd></div></dl></div>}
                        </div>}
                    </article>)}
                </section>
            </>}
        </>}
                    {/* Piece 1: Publish dialog */}
            {forkOpen && <ForkBuildDialog pack={pack} build={build} onClose={()=>setForkOpen(false)} onCreated={(target,copied)=>{setForkOpen(false);router.push(`/modpacks/${target.id}/builds/${copied.id}`);}}/>}
            {downloadKind && <PackDownloadDialog pack={pack} build={build} kind={downloadKind} onClose={() => setDownloadKind(null)} />}
            {uploadOpen && <UploadContentModal packId={packId} buildId={buildId} onClose={() => { setUploadOpen(false); refresh(); }} onUploaded={count => { setUploadOpen(false); showToast(`Uploaded ${count} file${count === 1 ? '' : 's'}.`); refresh(); }} />}
            {publishDialogOpen && (
                <PublishDialog
                    build={build}
                    packId={packId}
                    onClose={() => setPublishDialogOpen(false)}
                    onPublished={refresh}
                    showToast={showToast}
                />
            )}

            {/* Piece 2: Replace-with-Modrinth dialog */}
            {replaceEntry && (
                <ReplaceDialog
                    entry={replaceEntry}
                    build={build}
                    packId={packId}
                    disabled={disabled}
                    isFrozen={isFrozen}
                    onClose={() => setReplaceEntry(null)}
                    onReplaced={refresh}
                    showToast={showToast}
                />
            )}

            {/* Auto-update: per-row Upgrade dialog */}
            {upgradeEntry && (
                <UpdateModsDialog
                    entry={upgradeEntry}
                    build={build}
                    packId={packId}
                    disabled={disabled}
                    isFrozen={isFrozen}
                    onClose={() => setUpgradeEntry(null)}
                    onUpdated={refresh}
                    showToast={showToast}
                />
            )}

            {/* In-panel config text editor */}
            {editing && (
                <ConfigEditorModal
                    packId={packId}
                    buildId={buildId}
                    modversionId={editing.modversionId}
                    title={editing.title}
                    onClose={() => setEditing(null)}
                    onSaved={() => {
                        setEditing(null);
                        refresh();
                    }}
                />
            )}


    </div>;
}
