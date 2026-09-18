"use client";

import React, { useCallback, useEffect, useRef, useState } from 'react';

import Link from 'next/link';
import {
    Package, ArrowLeft, Trash2, Search,
    CircleCheck, CircleAlert, Box, Upload, Lock,
    Download, Share2, X, Loader2, Replace, AlertTriangle, ArrowUpCircle, FileText, Copy,
} from 'lucide-react';
import { systemEvents } from '@/lib/systemEvents';
import {
    listBuilds, listContent, addModrinthContent, removeContent, setContentSide,
    uploadContent, getPack, modrinthTypeToContentType, type Pack, type PackBuild, type BuildContentEntry,
} from '@/lib/api/packs';
import { publishModrinth, replaceWithModrinth, updateMods, mrpackDownloadUrl } from '@/lib/api/packsPublish';
import BuildMigrationPanel from '@/components/mods/BuildMigrationPanel';
import { createShareLink, listShareLinks, revokeShareLink, publicShareUrl, type ShareLink, type ShareLinkKind } from '@/lib/api/packsShare';
import { getAuthHeader } from '@/lib/api/core';
import { useAppData } from '@/lib/AppDataContext';
import { SkeletonHeader, SkeletonList, SkeletonText, Skeleton } from '@/components/Skeleton';
import ModrinthVersionBrowser from '@/components/modrinth/ModrinthVersionBrowser';
import ConfigEditorModal from '@/components/modpacks/ConfigEditorModal';
import { Badge } from '@/components/ui/Badge';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';

// Build content editor. Two panels:
//   left:  the build's content list (mods / resource-packs / plugins), with a
//          per-row side selector (client/server/both) and a source chip
//          (green Modrinth = linked, yellow Upload = local file). Remove inline.
//   right: Modrinth search filtered to the build's loader + MC version, plus a
//          direct file upload. Frozen builds are read-only.
//
// Header actions: Export .mrpack (auth-blob download), Publish to Modrinth dialog.
// Per-row action: Replace with Modrinth dialog.
// Badge: Modrinth chip when build.modrinthPublished.

const SIDES: BuildContentEntry['side'][] = ['both', 'client', 'server'];

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
        (build.channel as 'beta' | 'release') || 'beta',
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
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
        window.addEventListener('keydown', onKey);
        return () => window.removeEventListener('keydown', onKey);
    }, [onClose]);

    return (
        <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
            onClick={onClose}
        >
            <div
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
            </div>
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
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
        window.addEventListener('keydown', onKey);
        return () => window.removeEventListener('keydown', onKey);
    }, [onClose]);

    return (
        <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
            onClick={onClose}
        >
            <div
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
            </div>
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
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose(); };
        window.addEventListener('keydown', onKey);
        return () => window.removeEventListener('keydown', onKey);
    }, [onClose]);

    return (
        <div
            className="fixed inset-0 z-50 flex items-center justify-center bg-black/50"
            onClick={onClose}
        >
            <div
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
            </div>
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
    const [content, setContent] = useState<BuildContentEntry[]>([]);
    const [loading, setLoading] = useState(true);

    // Upload state
    const fileInputRef = useRef<HTMLInputElement | null>(null);
    const [uploading, setUploading] = useState(false);
    const [uploadType, setUploadType] = useState<'mod' | 'resourcepack' | 'shaderpack' | 'config'>('mod');

    // Modrinth search state
    const [modrinthType, setModrinthType] = useState<'mod' | 'resourcepack' | 'shader'>('mod');

    // Export state
    const [exporting, setExporting] = useState(false);

    // Dialog state
    const [publishDialogOpen, setPublishDialogOpen] = useState(false);
    const [replaceEntry, setReplaceEntry] = useState<BuildContentEntry | null>(null);
    const [upgradeEntry, setUpgradeEntry] = useState<BuildContentEntry | null>(null);
    const [updatingAll, setUpdatingAll] = useState(false);
    const [editing, setEditing] = useState<{ modversionId: number; title: string } | null>(null);

    // Share-link state
    const [shareLinks, setShareLinks] = useState<ShareLink[]>([]);
    const [shareKind, setShareKind] = useState<ShareLinkKind>('client-mrpack');
    const [shareExpiry, setShareExpiry] = useState(0);
    const [creatingShare, setCreatingShare] = useState(false);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const [p, bs, list, sl] = await Promise.all([
            getPack(packId),
            listBuilds(packId),
            listContent(packId, buildId),
            listShareLinks(packId, buildId).catch(() => ({ success: false, links: [] as ShareLink[] })),
        ]);
        setPack(p);
        setBuild(bs.find(b => b.id === buildId) || null);
        setContent(list);
        setShareLinks(sl.links || []);
        setLoading(false);
    }, [packId, buildId]);

    useEffect(() => { refresh(); }, [refresh]);

    useEffect(() => {
        const unsubC = systemEvents.on('pack_content.changed', (evt) => {
            const bid = (evt.payload as any)?.buildId;
            if (bid === undefined || bid === buildId) refresh();
        });
        const unsubB = systemEvents.on('pack_builds.changed', () => { refresh(); });
        return () => { unsubC(); unsubB(); };
    }, [buildId, refresh]);

    const isFrozen = !!build?.frozen;
    const disabled = modpacksDisabled || isFrozen;
    const hasAnyUpdate = content.some(hasUpdateAvailable);

    const handleUpload = async (e: React.ChangeEvent<HTMLInputElement>) => {
        const file = e.target.files?.[0];
        if (fileInputRef.current) fileInputRef.current.value = '';
        if (!file) return;
        setUploading(true);
        const res = await uploadContent(packId, buildId, file, 'both', uploadType);
        setUploading(false);
        if (res.success) {
            showToast(`Uploaded ${file.name}`, true);
            refresh();
        } else {
            showToast(res.message || 'Upload failed', false);
        }
    };

    const handleSetSide = async (entry: BuildContentEntry, side: string) => {
        const res = await setContentSide(packId, buildId, entry.id, side);
        if (res.success) {
            refresh();
        } else {
            showToast(res.message || 'Failed to set side', false);
        }
    };

    const handleRemove = async (entry: BuildContentEntry) => {
        const res = await removeContent(packId, buildId, entry.id);
        if (res.success) {
            showToast(`Removed ${entry.prettyName || entry.modSlug}`, true);
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

    // Auth-blob mrpack download — cannot use a bare <a href> because the route
    // requires an Authorization header that a plain anchor cannot send.
    const handleExport = async () => {
        if (!build || !pack) return;
        setExporting(true);
        try {
            const res = await fetch(mrpackDownloadUrl(packId, buildId), {
                headers: getAuthHeader(),
            });
            if (!res.ok) {
                showToast(`Export failed: ${res.status}`, false);
                return;
            }
            const blob = await res.blob();
            const blobUrl = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = blobUrl;
            a.download = `${pack.internalSlug}-${build.versionString}.mrpack`;
            document.body.appendChild(a);
            a.click();
            document.body.removeChild(a);
            URL.revokeObjectURL(blobUrl);
        } catch {
            showToast('Export failed.', false);
        } finally {
            setExporting(false);
        }
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

    if (loading) return (
        <main className="flex-1 flex flex-col overflow-hidden">
            <header className="shrink-0 p-6 pb-3 max-w-6xl">
                <SkeletonText width="w-32" className="mb-3" />
                <div className="flex items-start gap-3">
                    <Skeleton className="w-10 h-10 rounded-md shrink-0" />
                    <div className="flex-1">
                        <SkeletonHeader />
                    </div>
                </div>
            </header>
            <div className="flex-1 grid grid-cols-1 lg:grid-cols-2 gap-4 px-6 pb-6 max-w-6xl w-full overflow-hidden">
                <section className="card p-4 flex flex-col overflow-hidden">
                    <SkeletonText width="w-32" className="mb-3 h-4" />
                    <SkeletonList rows={4} />
                </section>
                <section className="card p-4 flex flex-col overflow-hidden">
                    <SkeletonText width="w-32" className="mb-3 h-4" />
                    <Skeleton className="h-9 w-full rounded mb-3" />
                    <SkeletonList rows={5} />
                </section>
            </div>
        </main>
    );
    if (!build) {
        return (
            <main className="flex-1 flex flex-col items-center justify-center p-6 text-(--base-06) gap-3">
                <p className="text-sm">Build not found.</p>
                <Link href={`/modpacks/${packId}`} className="btn btn-secondary btn-sm">
                    <ArrowLeft size={12} />
                    Back
                </Link>
            </main>
        );
    }

    return (
        <main className="flex-1 flex flex-col overflow-hidden">
            <header className="shrink-0 p-6 pb-3 max-w-6xl">
                <Link href={`/modpacks/${packId}`} className="text-xs text-(--base-06) hover:text-(--base-09) inline-flex items-center gap-1 mb-3">
                    <ArrowLeft size={11} />
                    Back to pack
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
                <div className="flex items-start gap-3">
                    <div className="w-10 h-10 rounded-md bg-(--accent-ghost) flex items-center justify-center shrink-0">
                        <Package size={18} className="text-(--accent-light)" />
                    </div>
                    <div className="min-w-0 flex-1">
                        <h1 className="text-lg font-display font-bold text-(--base-09) inline-flex items-center gap-2 flex-wrap">
                            Build Content
                            <span className="font-mono text-sm text-(--base-07) font-normal">
                                {build.versionString}
                            </span>
                            {isFrozen && (
                                <Lock size={14} className="text-(--accent-light)" aria-label="Frozen" />
                            )}
                            {/* Piece 4: Modrinth published badge */}
                            {build.modrinthPublished && (
                                <Badge variant="success" icon={<CircleCheck size={10} />}>Modrinth</Badge>
                            )}
                        </h1>
                        <p className="text-xs text-(--base-06)">
                            Search filtered to <code className="font-mono">{build.loader || 'any loader'}</code> ·{' '}
                            <code className="font-mono">MC {build.minecraft || 'any'}</code>
                        </p>
                        {isFrozen && (
                            <p className="text-xs text-(--base-06) italic mt-1">
                                This build is frozen — it was persisted on first publish/export.
                                Create a new build to change its content list.
                            </p>
                        )}
                    </div>
                    <div className="flex items-center gap-2">
                        {/* Update all — only shown when at least one linked entry has a newer Modrinth version cached */}
                        {hasAnyUpdate && (
                            <button
                                type="button"
                                onClick={handleUpdateAll}
                                disabled={disabled || updatingAll}
                                title={disabled ? (isFrozen ? 'Build is frozen' : 'Modpack authoring is disabled') : 'Upgrade all mods with an available update'}
                                className="btn btn-secondary btn-sm inline-flex items-center gap-1.5 disabled:opacity-40 disabled:cursor-not-allowed"
                            >
                                {updatingAll
                                    ? <Loader2 size={12} className="animate-spin" />
                                    : <ArrowUpCircle size={12} />
                                }
                                {updatingAll ? 'Updating…' : 'Update all'}
                            </button>
                        )}

                        {/* Piece 3: Export .mrpack */}
                        <button
                            type="button"
                            onClick={handleExport}
                            disabled={exporting || !pack}
                            title="Export .mrpack"
                            className="btn btn-secondary btn-sm inline-flex items-center gap-1.5 disabled:opacity-40 disabled:cursor-not-allowed"
                        >
                            {exporting
                                ? <Loader2 size={12} className="animate-spin" />
                                : <Download size={12} />
                            }
                            {exporting ? 'Exporting…' : 'Export'}
                        </button>

                        {/* Piece 1: Publish to Modrinth */}
                        <button
                            type="button"
                            onClick={() => setPublishDialogOpen(true)}
                            disabled={modpacksDisabled}
                            title={modpacksDisabled ? 'Modpack authoring is disabled' : 'Publish to Modrinth'}
                            className="btn btn-primary btn-sm inline-flex items-center gap-1.5 disabled:opacity-40 disabled:cursor-not-allowed"
                        >
                            <Share2 size={12} />
                            Publish
                        </button>

                        <select
                            value={uploadType}
                            onChange={e => setUploadType(e.target.value as typeof uploadType)}
                            className="input-field py-1.5 text-xs w-auto"
                            disabled={disabled || uploading}
                            title="Content type for the uploaded file"
                        >
                            <option value="mod">Mod</option>
                            <option value="resourcepack">Resource pack</option>
                            <option value="shaderpack">Shader pack</option>
                            <option value="config">Config</option>
                        </select>
                        <input
                            ref={fileInputRef}
                            type="file"
                            className="hidden"
                            onChange={handleUpload}
                        />
                        <button
                            onClick={() => fileInputRef.current?.click()}
                            className="btn btn-secondary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                            disabled={disabled || uploading}
                            title={disabled ? (isFrozen ? 'Build is frozen' : 'Modpack authoring is disabled') : 'Upload a local file'}
                        >
                            <Upload size={12} />
                            {uploading ? 'Uploading…' : 'Upload'}
                        </button>
                    </div>
                </div>
            </header>

            {build.minecraft && build.loader && (
                <BuildMigrationPanel
                    packId={packId}
                    build={build}
                    content={content}
                    disabled={modpacksDisabled}
                    disabledReason="Modpack authoring is disabled"
                    showToast={showToast}
                />
            )}

            {(featureFlags.shareLinks || shareLinks.length > 0) && (
                <section className="card p-4 mx-6 mb-4 max-w-6xl">
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
                                disabled={creatingShare}
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
                                                    expires {new Date(link.expiresAt).toLocaleDateString()}
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

            <div className="flex-1 grid grid-cols-1 lg:grid-cols-2 gap-4 px-6 pb-6 max-w-6xl w-full overflow-hidden">
                {/* Current content */}
                <section className="card p-4 flex flex-col overflow-hidden">
                    <header className="flex items-center gap-2 mb-3 shrink-0">
                        <Box size={14} className="text-(--accent-light)" />
                        <h2 className="text-sm font-medium text-(--base-09)">In this build ({content.length})</h2>
                    </header>
                    <div className="flex-1 overflow-y-auto space-y-2">
                        {content.length === 0 ? (
                            <p className="text-xs text-(--base-06) text-center py-8">No content yet. Add some from the search panel →</p>
                        ) : (
                            content.map(entry => (
                                <article key={entry.id} className="flex items-center gap-2 p-2 rounded-md border border-(--base-04)">
                                    <Box size={14} className="text-(--accent-light) shrink-0" />
                                    <div className="min-w-0 flex-1">
                                        <div className="text-sm font-medium text-(--base-09) truncate">{entry.prettyName || entry.modSlug}</div>
                                        <div className="text-[10px] font-mono text-(--base-06) truncate">
                                            {entry.version || entry.contentType}
                                        </div>
                                    </div>
                                    <Badge variant={entry.linked ? 'success' : 'warning'} className="shrink-0">
                                        {entry.linked ? 'Modrinth' : 'Upload'}
                                    </Badge>
                                    {/* Content-type chip — only shown for non-mod types to avoid noise on the common case */}
                                    {entry.contentType && entry.contentType !== 'mod' && (
                                        <Badge variant="neutral" className="shrink-0">{entry.contentType}</Badge>
                                    )}
                                    {/* Auto-update: badge shown once the cron has cached a newer Modrinth version */}
                                    {hasUpdateAvailable(entry) && (
                                        <Badge variant="accent" className="shrink-0">Update available</Badge>
                                    )}
                                    {/* Piece 2: Replace with Modrinth — only for non-linked (uploaded) entries */}
                                    {!entry.linked && (
                                        <button
                                            type="button"
                                            onClick={() => setReplaceEntry(entry)}
                                            disabled={disabled}
                                            title={disabled ? (isFrozen ? 'Build is frozen' : 'Modpack authoring is disabled') : 'Replace with a Modrinth version'}
                                            className="p-1.5 rounded text-(--base-07) hover:bg-(--base-04) hover:text-(--accent-light) transition-colors disabled:opacity-40 disabled:cursor-not-allowed shrink-0"
                                        >
                                            <Replace size={12} />
                                        </button>
                                    )}
                                    {/* Auto-update: Upgrade — only for linked entries with an update available */}
                                    {hasUpdateAvailable(entry) && !disabled && (
                                        <button
                                            type="button"
                                            onClick={() => setUpgradeEntry(entry)}
                                            title="Upgrade to the latest Modrinth version"
                                            className="p-1.5 rounded text-(--base-07) hover:bg-(--base-04) hover:text-(--accent-light) transition-colors shrink-0"
                                        >
                                            <ArrowUpCircle size={12} />
                                        </button>
                                    )}
                                    {/* In-panel config text editor — config content only */}
                                    {entry.contentType === 'config' && !disabled && (
                                        <button
                                            type="button"
                                            onClick={() => setEditing({ modversionId: entry.id, title: entry.prettyName || entry.modSlug })}
                                            title="Edit config text"
                                            className="p-1.5 rounded text-(--base-07) hover:bg-(--base-04) hover:text-(--accent-light) transition-colors shrink-0"
                                        >
                                            <FileText size={12} />
                                        </button>
                                    )}
                                    <select
                                        value={entry.side}
                                        onChange={e => handleSetSide(entry, e.target.value)}
                                        className="input-field input-mono py-1 text-[11px] w-24 shrink-0 disabled:opacity-40 disabled:cursor-not-allowed"
                                        disabled={disabled}
                                        title={disabled ? (isFrozen ? 'Build is frozen' : 'Modpack authoring is disabled') : 'Set side'}
                                    >
                                        {SIDES.map(s => <option key={s} value={s}>{s}</option>)}
                                    </select>
                                    <button
                                        onClick={() => handleRemove(entry)}
                                        className="btn btn-secondary btn-sm shrink-0 disabled:opacity-40 disabled:cursor-not-allowed"
                                        title={disabled ? (isFrozen ? 'Build is frozen' : 'Modpack authoring is disabled') : 'Remove'}
                                        disabled={disabled}
                                    >
                                        <Trash2 size={11} className="text-(--error)" />
                                    </button>
                                </article>
                            ))
                        )}
                    </div>
                </section>

                {/* Modrinth search */}
                <section className="card p-4 flex flex-col overflow-hidden">
                    <header className="flex items-center gap-2 mb-3 shrink-0">
                        <Search size={14} className="text-(--accent-light)" />
                        <h2 className="text-sm font-medium text-(--base-09)">Add from Modrinth</h2>
                        <select
                            value={modrinthType}
                            onChange={e => setModrinthType(e.target.value as typeof modrinthType)}
                            className="input-field py-1.5 text-xs w-auto ml-auto"
                            disabled={disabled}
                            title="Project type to search for"
                        >
                            <option value="mod">Mods</option>
                            <option value="resourcepack">Resource packs</option>
                            <option value="shader">Shaders</option>
                        </select>
                    </header>
                    <ModrinthVersionBrowser
                        loader={build.loader || undefined}
                        mcVersion={build.minecraft || undefined}
                        projectType={modrinthType}
                        installedProjectIds={new Set(content.map(c => c.modrinthProjectId).filter(Boolean) as string[])}
                        disabled={disabled}
                        disabledTitle={isFrozen ? 'Build is frozen' : 'Modpack authoring is disabled'}
                        onPick={(projectId, versionId, hit) => {
                            if (disabled) return;
                            const contentType = modrinthTypeToContentType(hit.project_type);
                            addModrinthContent(packId, buildId, { projectId, versionId, contentType, resolveDeps: contentType === 'mod' })
                                .then(res => {
                                    if (res.success) {
                                        showToast(`Added ${hit.title}`, true);
                                        refresh();
                                    } else {
                                        showToast(res.message || 'Add failed', false);
                                    }
                                });
                        }}
                    />
                </section>
            </div>

            {/* Piece 1: Publish dialog */}
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

        </main>
    );
}
