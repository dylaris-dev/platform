"use client";

import React, { useCallback, useEffect, useState } from 'react';

import { LayoutGrid, Plus, Pencil, Trash2, MoreHorizontal, ExternalLink, AlertTriangle, X, Link2, Copy, RotateCw, Clock, Lock } from 'lucide-react';
import { systemEvents } from '@/lib/systemEvents';
import {
    listServerTabs, createServerTab, updateServerTab, deleteServerTab,
    rotateShareLink, revokeShareLink,
    type ServerTab, type ServerTabInput,
} from '@/lib/api/serverTabs';
import { getFiles } from '@/lib/api/files';
import { useAppData } from '@/lib/AppDataContext';
import { shareLinkUrl } from '@/lib/tabProxy';
import { toLocalInput, fromLocalInput } from '@/lib/localDateTime';
import { shareLinkExpired } from '@/lib/shareLink';
import { DynamicIcon, TAB_ICON_NAMES } from '@/lib/icons';
import { SkeletonList } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';
import Menu from '@/components/ui/Menu';
import Segmented from '@/components/ui/Segmented';
import Tooltip from '@/components/ui/Tooltip';
import NumberField from '@/components/ui/NumberField';
import Callout from '@/components/server/Callout';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { Input } from '@ui-sdk/react/input';
import { Dialog, DialogBackdrop, DialogClose, DialogContent, DialogDescription, DialogTitle } from '@ui-sdk/react/dialog';
import Select from '@/components/ui/Select';
import Switch from '@/components/ui/Switch';
import { fmtDateTime } from '@/lib/time';

// Custom Tabs management.
//
// The two modes differ in WHO fetches the page, which is the whole thing and
// was nowhere on this screen:
//
//   direct   - the BROWSER fetches the URL. It therefore has to be reachable
//              from wherever the reader is sitting, and the platform is not
//              involved in serving it at all. The Dylaris edge carries raw
//              Minecraft TCP only (its sole HTTP is a /healthz readiness
//              port), so a direct URL can never point at something the edge
//              publishes - it is for an address the operator or the customer
//              already exposes themselves.
//
//   proxied  - CORE fetches it, streaming the container's HTTP/WebSocket over
//              the existing gRPC mesh so the browser only ever talks to the
//              panel origin. Nothing has to be exposed, and it works the same
//              on every routing mode including gateway-only.
//
// So proxied is the right default for anything running IN the server
// container, and direct stays valid for an external address. Neither is
// invalid on a gateway-routed platform; direct simply cannot be fulfilled BY
// the platform there.


interface EditingTab extends Partial<ServerTab> {
    isNew?: boolean;
}

// Default web ports of the map plugins/mods, from each project's own default
// config (checked 2026-10-23): squaremap and Pl3xMap 8080, BlueMap 8100,
// Dynmap 8123.
const MAP_PRESETS = [
    { name: 'squaremap', port: 8080 },
    { name: 'BlueMap', port: 8100 },
    { name: 'Pl3xMap', port: 8080 },
    { name: 'Dynmap', port: 8123 },
];

export default function ServerConfigTabsPage() {
    const paramId = useRouteId('servers');
    const { servers, gatewayEnabled, coreInfo } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);

    const [tabs, setTabs] = useState<ServerTab[]>([]);
    const [loading, setLoading] = useState(true);
    const [loadError, setLoadError] = useState<string | null>(null);
    const [savingTab, runSave] = useBusy();
    const [editing, setEditing] = useState<EditingTab | null>(null);
    const [iconQuery, setIconQuery] = useState('');

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        if (!serverId) return;
        try {
            setTabs(await listServerTabs(serverId));
            setLoadError(null);
        } catch (e) {
            // "No custom tabs yet." next to a Create button is an invitation to
            // build a second copy of a tab that already exists.
            setLoadError(e instanceof Error ? e.message : 'Could not load the tabs for this server.');
        } finally {
            setLoading(false);
        }
    }, [serverId]);

    useEffect(() => { refresh(); }, [refresh]);

    useEffect(() => {
        const unsub = systemEvents.on('server_tabs.changed', (evt) => {
            const sid = (evt.payload as any)?.serverId;
            if (sid === undefined || sid === serverId) refresh();
        });
        return () => { unsub(); };
    }, [serverId, refresh]);

    const openCreate = () => setEditing({
        isNew: true,
        name: '',
        url: '',
        icon: 'layout-grid',
        enabled: true,
        openInPanel: true,
        position: tabs.length,
        mode: 'direct',
        targetPort: 8100,
        targetPath: '/',
        surface: 'tab',
        visibility: 'private',
        shareExpiresAt: null,
        subServerName: '',
    });
    const [subServers, setSubServers] = useState<string[]>([]);
    // slugFor is the tab whose custom-link field is open; null means none.
    const [slugFor, setSlugFor] = useState<number | null>(null);
    const [slugDraft, setSlugDraft] = useState('');

    // The sub-servers are the directories at the server root; that is how
    // SetupView enumerates them too, and there is no separate list endpoint.
    // A failed listing leaves the picker with just "every sub-server", which is
    // the safe default rather than an empty dropdown.
    // Keyed on the UUID, not on the servers array: AppDataContext replaces that
    // array on every status flip, and re-listing a server's directories each
    // time one of its neighbours starts is a request for nothing.
    const serverUuid = servers.find(s => s.id === serverId)?.uuid || '';
    useEffect(() => {
        if (!serverUuid) return;
        let cancelled = false;
        (async () => {
            const res = await getFiles('', serverUuid);
            if (cancelled || !res.success || !Array.isArray(res.files)) return;
            setSubServers((res.files as any[]).filter(f => f.is_dir).map(f => String(f.name)));
        })();
        return () => { cancelled = true; };
    }, [serverUuid]);

    const openEdit = (t: ServerTab) => setEditing({ ...t });

    const handleSave = async () => {
        if (!editing) return;
        const name = (editing.name || '').trim();
        if (!name) { showToast('Name required', false); return; }
        const mode = editing.mode || 'direct';

        let payload: ServerTabInput;
        if (mode === 'proxied') {
            const port = Number(editing.targetPort || 0);
            const path = (editing.targetPath || '/').trim();
            if (!Number.isInteger(port) || port < 1 || port > 65535) {
                showToast('Container port must be 1-65535', false); return;
            }
            if (!path.startsWith('/')) { showToast('Path must start with /', false); return; }
            payload = {
                name,
                icon: editing.icon || 'layout-grid',
                enabled: editing.enabled ?? true,
                openInPanel: editing.openInPanel ?? true,
                position: editing.position ?? tabs.length,
                mode: 'proxied',
                targetPort: port,
                targetPath: path,
                surface: editing.surface || 'tab',
                visibility: editing.visibility || 'private',
                // Always sent on a proxied save, never omitted: omitting it is
                // how Core is told to KEEP the stored value, so clearing the
                // field in the form has to arrive as an explicit "".
                shareExpiresAt: editing.shareExpiresAt || '',
                // Same reasoning as shareExpiresAt above: "" is a real value
                // here ("every sub-server"), so it has to be sent rather than
                // omitted, or Core keeps whatever was stored.
                subServerName: editing.subServerName || '',
            };
        } else {
            const url = (editing.url || '').trim();
            if (!url) { showToast('URL required', false); return; }
            try { new URL(url); } catch { showToast('Invalid URL', false); return; }
            payload = {
                name,
                url,
                icon: editing.icon || 'layout-grid',
                enabled: editing.enabled ?? true,
                openInPanel: editing.openInPanel ?? true,
                position: editing.position ?? tabs.length,
                mode: 'direct',
            };
        }
        const res = editing.isNew
            ? await createServerTab(serverId, payload)
            : await updateServerTab(serverId, editing.id!, payload);
        if (res.success) {
            setEditing(null);
            showToast(editing.isNew ? 'Tab created.' : 'Tab saved.', true);
            refresh();
        } else {
            showToast(res.message || 'Save failed', false);
        }
    };

    const handleToggleEnabled = async (t: ServerTab) => {
        const res = await updateServerTab(serverId, t.id, { enabled: !t.enabled });
        if (res.success) refresh(); else showToast(res.message || 'Toggle failed', false);
    };

    const handleDelete = async (t: ServerTab) => {
        if (!(await confirmDialog({ title: 'Delete tab?', message: `Remove ${t.name} from the nav.` }))) return;
        const res = await deleteServerTab(serverId, t.id);
        if (res.success) {
            showToast('Tab deleted.', true);
            refresh();
        } else {
            showToast(res.message || 'Delete failed', false);
        }
    };

    // slug empty = the server picks an unguessable one. A chosen slug trades
    // that for something readable, which only weakens a PUBLIC link (it is meant
    // to be handed out anyway); a private link is gated by the ticket, not by
    // the slug.
    const handleRotate = async (t: ServerTab, slug?: string) => {
        const res = await rotateShareLink(serverId, t.id, slug);
        if (res.success) {
            showToast(slug ? 'Custom link set.' : 'Share link generated.', true);
            setSlugFor(null);
            setSlugDraft('');
            refresh();
        } else {
            showToast(res.message || 'Failed to generate link', false);
        }
    };

    const handleRevoke = async (t: ServerTab) => {
        const res = await revokeShareLink(serverId, t.id);
        if (res.success) { showToast('Share link revoked.', true); refresh(); }
        else showToast(res.message || 'Failed to revoke link', false);
    };

    const handleCopy = (token: string) => {
        navigator.clipboard.writeText(shareLinkUrl(token, coreInfo?.tabProxyHostSuffix));
        showToast('Link copied.', true);
    };

    if (!server) return null;
    // Two tabs with the same icon are indistinguishable in the sidebar, which is
    // the only place they are ever seen. Icons in use elsewhere are therefore
    // withheld - except the one this tab already has, or editing it would drop
    // its own icon out of the grid.
    const takenIcons = new Set(
        tabs.filter(t => t.id !== editing?.id).map(t => t.icon).filter(Boolean) as string[],
    );
    const availableIcons = TAB_ICON_NAMES.filter(
        n => !takenIcons.has(n) && n.includes(iconQuery.trim().toLowerCase()),
    );

    const editingMode = editing?.mode || 'direct';
    const editingSurfaceHasPage = editingMode === 'proxied' && (editing?.surface === 'page' || editing?.surface === 'both');

    return (
        <div className="flex flex-col gap-6">
            <div className="srv-config-head">
                <div className="min-w-0">
                    <h1 className="srv-config-title">Custom tabs</h1>
                    <p className="srv-config-desc max-w-2xl">
                        Direct tabs render a browser-reachable URL. Proxied tabs stream your
                        server container&apos;s web UI (BlueMap, squaremap, Dynmap) through
                        Dylaris - no public port needed.
                    </p>
                </div>
                <button type="button" onClick={openCreate} className="btn btn-primary btn-sm">
                    <Plus size={14} />
                    New tab
                </button>
            </div>

            {loading ? (
                <div className="srv-surface overflow-hidden" aria-busy="true" aria-label="Loading tabs">
                    <SkeletonList rows={3} />
                </div>
            ) : loadError ? (
                <Callout
                    tone="error"
                    role="alert"
                    icon={<AlertTriangle size={16} />}
                    title="Could not load tabs"
                    actions={<button type="button" onClick={refresh} className="btn btn-secondary btn-sm">Try again</button>}
                >
                    {loadError}
                </Callout>
            ) : tabs.length === 0 ? (
                <div className="srv-surface flex flex-col items-center gap-2 px-6 py-14 text-center">
                    <LayoutGrid size={32} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">No custom tabs yet.</p>
                    <button type="button" onClick={openCreate} className="btn btn-secondary btn-sm mt-2">
                        <Plus size={14} />
                        New tab
                    </button>
                </div>
            ) : (
                <ul className="srv-surface overflow-hidden">
                    {tabs.map(t => (
                        <li key={t.id} className="srv-config-item flex-col gap-3">
                            <div className="flex w-full items-center gap-4">
                                <div className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-md bg-(--base-03) ${
                                    t.enabled ? 'text-(--base-08)' : 'text-(--base-05)'
                                }`}>
                                    <DynamicIcon name={t.icon} size={16} />
                                </div>
                                <div className={`min-w-0 flex-1 ${t.enabled ? '' : 'opacity-60'}`}>
                                    <div className="flex flex-wrap items-center gap-2">
                                        <span className="text-sm font-medium text-(--base-09)">{t.name}</span>
                                        <span className="srv-config-pill">{t.mode}</span>
                                        {t.mode === 'proxied' && <span className="srv-config-pill">{t.surface}</span>}
                                        {t.mode === 'proxied' && t.visibility === 'public' && (
                                            <span className="srv-config-pill" data-tone="warning">Public</span>
                                        )}
                                    </div>
                                    {t.mode === 'direct' ? (
                                        <a href={t.url} target="_blank" rel="noopener noreferrer"
                                            className="srv-num mt-1 inline-flex max-w-full items-center gap-1 text-xs text-(--accent-light) hover:underline">
                                            <span className="truncate">{t.url}</span><ExternalLink size={12} className="shrink-0" />
                                        </a>
                                    ) : (
                                        <span className="srv-num mt-1 block text-xs text-(--base-06)">
                                            container :{t.targetPort}{t.targetPath}
                                            {t.subServerName && <> &middot; only on {t.subServerName}</>}
                                        </span>
                                    )}
                                </div>
                                <div className="flex shrink-0 items-center gap-2">
                                    <Switch
                                        checked={t.enabled}
                                        onChange={() => handleToggleEnabled(t)}
                                        ariaLabel={`${t.enabled ? 'Disable' : 'Enable'} tab ${t.name}`}
                                    />
                                    <Menu
                                        trigger={
                                            <button type="button" aria-label={`Actions for tab ${t.name}`} className="btn btn-ghost btn-icon btn-sm">
                                                <MoreHorizontal size={16} />
                                            </button>
                                        }
                                        items={[
                                            { label: 'Edit', icon: <Pencil size={14} />, onSelect: () => openEdit(t) },
                                            { type: 'separator' },
                                            { label: 'Delete', icon: <Trash2 size={14} />, danger: true, onSelect: () => handleDelete(t) },
                                        ]}
                                    />
                                </div>
                            </div>

                            {/* Share link row for proxied page tabs */}
                            {t.mode === 'proxied' && (t.surface === 'page' || t.surface === 'both') && (
                                <div className="flex w-full flex-col gap-2 rounded-lg bg-(--base-03) p-3">
                                    <div className="flex flex-wrap items-center gap-2">
                                        <Link2 size={14} className="shrink-0 text-(--base-06)" />
                                        {t.shareToken ? (
                                            <>
                                                <code className={`srv-num max-w-[240px] truncate text-xs ${
                                                    shareLinkExpired(t.shareExpiresAt) ? 'text-(--base-06) line-through' : 'text-(--base-08)'
                                                }`}>
                                                    {shareLinkUrl(t.shareToken, coreInfo?.tabProxyHostSuffix)}
                                                </code>
                                                <Tooltip content="Copy link">
                                                    <button type="button" onClick={() => handleCopy(t.shareToken)} aria-label="Copy link" className="btn btn-ghost btn-icon btn-sm">
                                                        <Copy size={14} />
                                                    </button>
                                                </Tooltip>
                                                <Tooltip content="Rotate (invalidates the old link)">
                                                    <button type="button" onClick={() => handleRotate(t)} aria-label="Rotate link" className="btn btn-ghost btn-icon btn-sm">
                                                        <RotateCw size={14} />
                                                    </button>
                                                </Tooltip>
                                                <Tooltip content="Choose your own link">
                                                    <button
                                                        type="button"
                                                        onClick={() => { setSlugFor(slugFor === t.id ? null : t.id); setSlugDraft(''); }}
                                                        className="btn btn-ghost btn-icon btn-sm"
                                                        aria-label="Choose your own link"
                                                        aria-expanded={slugFor === t.id}
                                                    >
                                                        <Pencil size={14} />
                                                    </button>
                                                </Tooltip>
                                                <Tooltip content="Revoke link">
                                                    <button type="button" onClick={() => handleRevoke(t)} aria-label="Revoke link" className="btn btn-ghost btn-icon btn-sm">
                                                        <Trash2 size={14} className="text-(--error-light)" />
                                                    </button>
                                                </Tooltip>
                                            </>
                                        ) : (
                                            <>
                                                <button type="button" onClick={() => handleRotate(t)} className="btn btn-secondary btn-sm">
                                                    <Link2 size={14} /> Generate share link
                                                </button>
                                                <button
                                                    type="button"
                                                    onClick={() => { setSlugFor(slugFor === t.id ? null : t.id); setSlugDraft(''); }}
                                                    className="btn btn-ghost btn-sm"
                                                    aria-expanded={slugFor === t.id}
                                                >
                                                    <Pencil size={14} /> Choose my own
                                                </button>
                                            </>
                                        )}
                                    </div>
                                    {/* An expiry that nothing displayed was as good as no expiry:
                                        the owner had no way to see when a link they handed out
                                        stops working, or that it already has. */}
                                    {slugFor === t.id && (
                                        <form
                                            className="flex flex-wrap items-center gap-2"
                                            onSubmit={e => { e.preventDefault(); handleRotate(t, slugDraft); }}
                                        >
                                            <span className="srv-num text-xs text-(--base-06)">/c/</span>
                                            <Input
                                                autoFocus
                                                value={slugDraft}
                                                onChange={e => setSlugDraft(e.target.value.toLowerCase())}
                                                placeholder="max-survival-map"
                                                minLength={4}
                                                maxLength={40}
                                                pattern="[a-z0-9]+(-[a-z0-9]+)*"
                                                className="input-field input-mono w-56 text-xs"
                                                aria-label="Custom share link"
                                            />
                                            <button type="submit" className="btn btn-primary btn-sm" disabled={slugDraft.trim().length < 4}>
                                                Set
                                            </button>
                                            <button type="button" onClick={() => { setSlugFor(null); setSlugDraft(''); }} className="btn btn-secondary btn-sm">
                                                Cancel
                                            </button>
                                            <span className="text-xs text-(--base-06)">
                                                Lowercase letters, digits and hyphens. A name anyone could
                                                guess is fine for a public link and changes nothing for a
                                                private one, which is gated by your sign-in.
                                            </span>
                                        </form>
                                    )}
                                    {/* A private tab's link opens only for signed-in members with
                                        access; to anyone else it says "This page is private". Said
                                        here, because a link reads as something to hand out. */}
                                    {t.shareToken && t.visibility !== 'public' && (
                                        <span className="inline-flex flex-wrap items-center gap-2 text-xs text-(--base-06)">
                                            <Lock size={14} className="shrink-0" aria-hidden="true" />
                                            Private: this link opens only for signed-in members with access to the server.
                                            <button
                                                type="button"
                                                onClick={async () => {
                                                    const res = await updateServerTab(serverId, t.id, { visibility: 'public' });
                                                    if (res.success) { showToast('The tab is public; anyone with the link can open it.', true); refresh(); }
                                                    else showToast(res.message || 'Could not make the tab public', false);
                                                }}
                                                className="btn btn-secondary btn-sm"
                                            >
                                                Make public
                                            </button>
                                        </span>
                                    )}
                                    {t.shareToken && t.shareExpiresAt && (
                                        <span className={`inline-flex items-center gap-1.5 text-xs ${
                                            shareLinkExpired(t.shareExpiresAt) ? 'text-(--warning-light)' : 'text-(--base-06)'
                                        }`}>
                                            <Clock size={14} className="shrink-0" />
                                            {shareLinkExpired(t.shareExpiresAt)
                                                ? `Expired ${fmtDateTime(t.shareExpiresAt)} - rotate to hand out a working one`
                                                : `Expires ${fmtDateTime(t.shareExpiresAt)}`}
                                        </span>
                                    )}
                                    {t.shareToken && !coreInfo?.tabProxyAvailable && (
                                        <span className="inline-flex items-start gap-1.5 text-xs text-(--warning-light)">
                                            <AlertTriangle size={14} className="mt-0.5 shrink-0" />
                                            Share links need a proxy host on Core (TAB_PROXY_HOST_SUFFIX).
                                            Until an admin sets that up this link answers &quot;not valid&quot;.
                                        </span>
                                    )}
                                </div>
                            )}
                        </li>
                    ))}
                </ul>
            )}

            {editing && (
                <Dialog open onOpenChange={open => { if (!open) setEditing(null); }}>
                    <DialogBackdrop className="modal-overlay animate-fade-in" />
                    <DialogContent className="modal-panel fixed inset-0 z-50 m-auto flex h-fit max-h-[calc(100dvh-2rem)] w-[calc(100%-2rem)] max-w-lg flex-col">
                        <div className="modal-header flex items-start justify-between gap-4">
                            <div className="min-w-0">
                                <DialogTitle className="modal-title">{editing.isNew ? 'New tab' : 'Edit tab'}</DialogTitle>
                                <DialogDescription className="mt-1 text-xs text-(--base-06)">A page shown in the server navigation.</DialogDescription>
                            </div>
                            <DialogClose aria-label="Close" className="btn btn-ghost btn-icon btn-sm shrink-0">
                                <X size={16} />
                            </DialogClose>
                        </div>
                        <div className="modal-body min-h-0 flex-1 space-y-5 overflow-y-auto">
                            <div>
                                <label htmlFor="tab-name" className="srv-label mb-1.5 block">Name</label>
                                <Input id="tab-name" type="text" value={editing.name || ''}
                                    onChange={e => setEditing({ ...editing, name: e.target.value })}
                                    className="input-field w-full" placeholder="Map" maxLength={64} />
                            </div>

                            <div>
                                <span className="srv-label mb-1.5 block">Mode</span>
                                <Segmented
                                    ariaLabel="Mode"
                                    value={editingMode}
                                    onChange={m => setEditing({ ...editing, mode: m as 'direct' | 'proxied' })}
                                    options={[{ id: 'direct', label: 'Direct URL' }, { id: 'proxied', label: 'Proxied (via Dylaris)' }]}
                                />
                                <p className="mt-1.5 text-xs leading-snug text-(--base-06)">
                                    {editingMode === 'proxied'
                                        ? 'Dylaris fetches the page from the server container and streams it to the browser. Nothing has to be exposed, and it works on every routing mode.'
                                        : 'The browser fetches the URL itself, so it has to be reachable from wherever the player is. Dylaris does not serve it.'}
                                </p>
                            </div>

                            {editingMode === 'direct' ? (
                                <div>
                                    <label htmlFor="tab-url" className="srv-label mb-1.5 block">URL</label>
                                    <Input id="tab-url" type="url" value={editing.url || ''}
                                        onChange={e => setEditing({ ...editing, url: e.target.value })}
                                        className="input-field input-mono w-full" placeholder="https://map.example.com" />
                                    <p className="mt-1.5 text-xs text-(--base-06)">Must be reachable from the user&apos;s browser.</p>
                                    {gatewayEnabled && (
                                        <p className="mt-1.5 text-xs leading-snug text-(--warning-light)">
                                            The Dylaris edge carries Minecraft traffic only — it serves no web
                                            pages. This has to be an address you publish yourself. For something
                                            running inside the server container, use Proxied instead.
                                        </p>
                                    )}
                                </div>
                            ) : (
                                <>
                                    {/* The maps' own default ports. They only fill the form; the
                                        map itself is installed from the Content tab, and a changed
                                        port in its config has to be changed here too. */}
                                    <div className="flex flex-wrap items-center gap-1.5">
                                        <span className="text-xs text-(--base-06)">Web map preset:</span>
                                        {MAP_PRESETS.map(p => (
                                            <Tooltip key={p.name} content={`${p.name} serves its map on port ${p.port}`}>
                                                <button
                                                    type="button"
                                                    onClick={() => setEditing({ ...editing, targetPort: p.port, targetPath: '/', name: editing.name || p.name, icon: 'map' })}
                                                    className="btn btn-secondary btn-sm"
                                                >
                                                    {p.name} <span className="srv-num text-(--base-06)">:{p.port}</span>
                                                </button>
                                            </Tooltip>
                                        ))}
                                    </div>
                                    <div className="grid grid-cols-2 gap-3">
                                        <NumberField
                                            label="Container port"
                                            value={editing.targetPort ?? 8100}
                                            onChange={v => setEditing({ ...editing, targetPort: v })}
                                            min={1}
                                            max={65535}
                                            placeholder="8100"
                                        />
                                        <div>
                                            <label htmlFor="tab-path" className="srv-label mb-1.5 block">Base path</label>
                                            <Input id="tab-path" type="text" value={editing.targetPath || '/'}
                                                onChange={e => setEditing({ ...editing, targetPath: e.target.value })}
                                                className="input-field input-mono w-full" placeholder="/" />
                                        </div>
                                    </div>
                                    <div>
                                        <label className="srv-label mb-1.5 block" htmlFor="tab-subserver">Sub-server</label>
                                        <Select
                                            id="tab-subserver"
                                            value={editing.subServerName || ''}
                                            onChange={v => setEditing({ ...editing, subServerName: v })}
                                            options={[
                                                { value: '', label: 'Every sub-server' },
                                                ...subServers.map(n => ({ value: n, label: n })),
                                                ...(editing.subServerName && !subServers.includes(editing.subServerName)
                                                    ? [{ value: editing.subServerName, label: `${editing.subServerName} (not found)` }]
                                                    : []),
                                            ]}
                                        />
                                        <p className="mt-1.5 text-xs text-(--base-06)">
                                            The tab points at a port inside the container, and the container runs
                                            whichever sub-server is started. Pin it if only one of them serves
                                            that port - the tab then stays hidden while another is running,
                                            instead of showing a different world under the same name.
                                        </p>
                                    </div>
                                    <div>
                                        <span className="srv-label mb-1.5 block">Surface</span>
                                        <Segmented
                                            ariaLabel="Surface"
                                            value={editing.surface || 'tab'}
                                            onChange={s => setEditing({ ...editing, surface: s as 'tab' | 'page' | 'both' })}
                                            options={[{ id: 'tab', label: 'tab' }, { id: 'page', label: 'page' }, { id: 'both', label: 'both' }]}
                                        />
                                        <p className="mt-1.5 text-xs text-(--base-06)">tab = in-dashboard, page = standalone share link, both = either.</p>
                                    </div>
                                    {editingSurfaceHasPage && (
                                        <div>
                                            <span className="srv-label mb-1.5 block">Visibility</span>
                                            <Segmented
                                                ariaLabel="Visibility"
                                                value={editing.visibility || 'private'}
                                                onChange={v => setEditing({ ...editing, visibility: v as 'private' | 'public' })}
                                                options={[{ id: 'private', label: 'private' }, { id: 'public', label: 'public' }]}
                                            />
                                            {(editing.visibility || 'private') === 'public' && (
                                                <p className="mt-2 flex items-start gap-1.5 text-xs text-(--warning-light)">
                                                    <AlertTriangle size={14} className="mt-0.5 shrink-0" />
                                                    <span>Anyone with the share link can view this page - no login required.</span>
                                                </p>
                                            )}
                                        </div>
                                    )}
                                    {editingSurfaceHasPage && (
                                        <div>
                                            <div className="mb-1.5 flex items-baseline justify-between gap-2">
                                                <label className="srv-label" htmlFor="tab-share-expiry">Link expires (optional)</label>
                                                {editing.shareExpiresAt && (
                                                    <button type="button"
                                                        onClick={() => setEditing({ ...editing, shareExpiresAt: null })}
                                                        className="rounded text-xs text-(--base-06) transition-colors hover:text-(--base-08) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">
                                                        Clear
                                                    </button>
                                                )}
                                            </div>
                                            <Input
                                                id="tab-share-expiry"
                                                type="datetime-local"
                                                value={editing.shareExpiresAt ? toLocalInput(editing.shareExpiresAt) : ''}
                                                onChange={e => setEditing({
                                                    ...editing,
                                                    shareExpiresAt: e.target.value ? fromLocalInput(e.target.value) : null,
                                                })}
                                                style={{ colorScheme: 'dark' }}
                                                className="input-field datetime-field w-full"
                                            />
                                            <p className="mt-1.5 text-xs text-(--base-06)">
                                                {editing.shareExpiresAt
                                                    ? 'After this the link stops working. The tab itself stays.'
                                                    : 'Empty means the link never expires.'}
                                            </p>
                                        </div>
                                    )}
                                    {editingSurfaceHasPage && !coreInfo?.tabProxyAvailable && (
                                        <p className="flex items-start gap-1.5 text-xs text-(--warning-light)">
                                            <AlertTriangle size={14} className="mt-0.5 shrink-0" />
                                            <span>
                                                Share links are switched off on this platform: they need their own
                                                origin (TAB_PROXY_PORT + TAB_PROXY_ORIGIN on Core) so a container
                                                cannot reach the panel&apos;s session. Until an admin sets that up, the
                                                standalone page answers &quot;not valid&quot; - the in-dashboard tab works either way.
                                            </span>
                                        </p>
                                    )}
                                </>
                            )}

                            <div>
                                <div className="mb-1.5 flex items-baseline justify-between gap-2">
                                    <label className="srv-label" htmlFor="tab-icon-search">Icon</label>
                                    <span className="srv-num text-xs text-(--base-06)">
                                        {availableIcons.length} available
                                    </span>
                                </div>
                                <Input
                                    id="tab-icon-search"
                                    type="search"
                                    value={iconQuery}
                                    onChange={e => setIconQuery(e.target.value)}
                                    placeholder="Search icons…"
                                    className="input-field w-full"
                                />
                                <div className="mt-2 grid max-h-44 grid-cols-8 gap-1 overflow-y-auto rounded-lg bg-(--base-01) p-1.5">
                                    {availableIcons.map(icon => (
                                        // Native title, not Tooltip: the grid holds every icon name, and a
                                        // tooltip instance per cell would be hundreds of popup roots.
                                        <button key={icon} type="button" title={icon} aria-label={icon} aria-pressed={editing.icon === icon}
                                            onClick={() => setEditing({ ...editing, icon })}
                                            className={`flex h-9 w-9 items-center justify-center rounded-md transition-colors focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] ${
                                                editing.icon === icon
                                                    ? 'bg-(--accent-ghost) text-(--accent-light)'
                                                    : 'text-(--base-07) hover:bg-(--base-03) hover:text-(--base-09)'
                                            }`}>
                                            <DynamicIcon name={icon} size={16} />
                                        </button>
                                    ))}
                                    {availableIcons.length === 0 && (
                                        <p className="col-span-8 px-2 py-3 text-xs text-(--base-06)">
                                            {iconQuery
                                                ? `Nothing matches "${iconQuery}".`
                                                : 'Every icon is already in use by another tab.'}
                                        </p>
                                    )}
                                </div>
                            </div>

                            {editingMode === 'direct' && (
                                <div className="flex items-center justify-between gap-4 rounded-lg bg-(--base-03) px-4 py-3">
                                    <div>
                                        <div className="text-sm font-medium text-(--base-09)">Open in panel</div>
                                        <p className="text-xs text-(--base-06)">Off: clicking the tab opens the URL in a new browser window.</p>
                                    </div>
                                    <Switch checked={editing.openInPanel ?? true} onChange={v => setEditing({ ...editing, openInPanel: v })} ariaLabel="Open in panel" />
                                </div>
                            )}
                            <div className="flex items-center justify-between gap-4 rounded-lg bg-(--base-03) px-4 py-3">
                                <div>
                                    <div className="text-sm font-medium text-(--base-09)">Enabled</div>
                                    <p className="text-xs text-(--base-06)">Disabled tabs disappear from the nav strip.</p>
                                </div>
                                <Switch checked={editing.enabled ?? true} onChange={v => setEditing({ ...editing, enabled: v })} ariaLabel="Enabled" />
                            </div>
                        </div>
                        <div className="modal-footer">
                            <DialogClose className="btn btn-secondary">Cancel</DialogClose>
                            <button type="button" onClick={() => runSave(handleSave)} disabled={savingTab} className="btn btn-primary disabled:opacity-40">
                                {editing.isNew ? 'Create' : 'Save'}
                            </button>
                        </div>
                    </DialogContent>
                </Dialog>
            )}
        </div>
    );
}
