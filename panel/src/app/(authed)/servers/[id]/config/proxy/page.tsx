"use client";

import { toast as notifyToast } from '@/components/ui/Toast';

import { useCallback, useEffect, useState, lazy, Suspense } from 'react';

import { AlertTriangle, FileQuestion, Check, Copy, Info } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { getFileContent, saveFile } from '@/lib/api';
import { proxyConfigFilename, backendAddress, proxyPrereqHint } from '@/lib/proxyConfig';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import { SettingsCardSave } from '@/components/settings/SettingsCard';
import { useRouteId } from '@/lib/routeParams';
import Tooltip from '@/components/ui/Tooltip';
import Callout from '@/components/server/Callout';
import ConfigSection from '../ConfigSection';

// Proxy Config tab: a raw text editor for the proxy's own config file
// (config.yml for BungeeCord/Waterfall, velocity.toml for Velocity) plus a
// right-hand column of the linked backend servers with their stable in-network
// address to paste into that file. There is no simple/form mode - proxy config
// is edited as raw text on purpose.

const CodeMirrorEditor = lazy(() => import('@dylaris/ui-filebrowser').then(m => ({ default: m.CodeMirrorEditor })));

export default function ServerConfigProxyPage() {
    const paramId = useRouteId('servers');
    const { servers } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);

    const serverUuid = server?.uuid ?? '';
    const activeSubServer = server?.activeSubServer ?? '';
    const installerType = server?.installerType ?? '';
    const filename = proxyConfigFilename(installerType);
    // The file lives in the active sub-server dir; the node resolves paths
    // relative to the server root, so we prepend the sub-server ourselves.
    const filePath = activeSubServer ? `${activeSubServer}/${filename}` : '';

    const [text, setText] = useState('');
    const [dirty, setDirty] = useState(false);
    const [loading, setLoading] = useState(true);
    const [notFound, setNotFound] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const [saving, setSaving] = useState(false);

    const [copiedKey, setCopiedKey] = useState<string | null>(null);

    const showToast = useCallback((msg: string, ok = true) => {
        notifyToast(msg, ok);

    }, []);

    const reload = useCallback(async () => {
        if (!serverUuid) return;
        setNotFound(false);
        setError(null);
        // No active sub-server -> nothing to load yet. Soft empty state.
        if (!filePath) { setLoading(false); setNotFound(true); return; }
        setLoading(true);
        try {
            const res = await getFileContent(filePath, serverUuid);
            if (res?.success === false) {
                // A proxy writes its config on first boot, so file-not-found is
                // the expected pre-launch state, not an error.
                const msg = (res.message || '').toLowerCase();
                if (msg.includes('not found') || msg.includes('no such file') || msg.includes('does not exist') || msg.includes('enoent')) {
                    setNotFound(true);
                } else {
                    setError(res.message || `Could not load ${filename}.`);
                }
            } else {
                const content: string = res.content ?? res.text ?? '';
                setText(content);
                setDirty(false);
            }
        } catch {
            setError(`Network error while loading ${filename}.`);
        }
        setLoading(false);
    }, [serverUuid, filePath, filename]);

    useEffect(() => { reload(); }, [reload]);

    const save = useCallback(async (): Promise<boolean> => {
        if (!serverUuid || !filePath) return false;
        setSaving(true);
        try {
            const res = await saveFile(filePath, text, serverUuid);
            if (res?.success === false) {
                showToast(res.message || 'Save failed.', false);
                return false;
            }
            setDirty(false);
            setNotFound(false);
            showToast(`Saved ${filename}`);
            return true;
        } catch {
            showToast('Network error', false);
            return false;
        } finally {
            setSaving(false);
        }
    }, [serverUuid, filePath, text, filename, showToast]);

    // Editing a raw config file is the same kind of change as everything else
    // in the panel now: the Save above the editor commits it, and leaving the page
    // with an unsaved edit prompts rather than dropping it.
    useUnsavedChanges({
        dirty: dirty && !notFound,
        saving,
        save,
        discard: () => { void reload(); },
    });

    const copy = (key: string, val: string) => {
        navigator.clipboard.writeText(val);
        setCopiedKey(key);
        setTimeout(() => setCopiedKey(null), 1500);
    };

    const backends = server ? servers.filter(s => s.proxyId === server.id && s.serverType !== 'proxy') : [];

    if (!server) return null;

    return (
        <div className="flex min-h-full flex-col gap-6">
            <div className="srv-config-head">
                <div className="min-w-0">
                    <h1 className="srv-config-title">Proxy config</h1>
                    <p className="srv-config-desc max-w-2xl">
                        Raw <code className="srv-num text-(--base-08)">{filename}</code>. Changes are not applied until you save and restart the proxy.
                    </p>
                </div>
                {/* Refused while the file does not exist: there is nothing to
                    write it into. */}
                <div className="flex items-center gap-2">
                    <SettingsCardSave
                        form={{
                            dirty: dirty && !notFound,
                            saving,
                            save,
                            discard: () => { void reload(); },
                        }}
                    />
                </div>
            </div>

            <div className="flex min-h-0 flex-1 flex-col gap-6 lg:flex-row">
                <div className="flex min-h-[28rem] min-w-0 flex-1 flex-col">
                    {loading ? (
                        <div className="srv-surface flex flex-1 items-center justify-center text-sm text-(--base-07)" aria-busy="true">Loading…</div>
                    ) : error ? (
                        <Callout
                            tone="error"
                            role="alert"
                            icon={<AlertTriangle size={16} />}
                            title={`Could not load ${filename}`}
                            actions={<button type="button" onClick={reload} className="btn btn-secondary btn-sm">Retry</button>}
                        >
                            {error}
                        </Callout>
                    ) : notFound ? (
                        <div className="srv-surface flex flex-col items-center gap-3 px-6 py-14 text-center">
                            <FileQuestion size={32} className="text-(--base-05)" />
                            <p className="text-sm font-medium text-(--base-09)">
                                <code className="srv-num text-(--base-08)">{filename}</code> doesn&apos;t exist yet
                            </p>
                            <p className="max-w-md text-xs text-(--base-06)">
                                A proxy writes its config on first launch. Start the proxy once to generate it, then reload
                                this tab. You can also create it from the Files tab.
                            </p>
                            <button type="button" onClick={reload} className="btn btn-secondary btn-sm">Reload</button>
                        </div>
                    ) : (
                        <div className="srv-surface min-h-0 flex-1 overflow-hidden bg-(--base-01)">
                            <Suspense fallback={<div className="flex h-full items-center justify-center text-sm text-(--base-07)">Loading editor…</div>}>
                                <CodeMirrorEditor
                                    value={text}
                                    onChange={next => { setText(next); setDirty(true); }}
                                    filename={filename}
                                    className="h-full"
                                />
                            </Suspense>
                        </div>
                    )}
                </div>

                <aside className="flex shrink-0 flex-col gap-4 lg:w-80">
                    <ConfigSection
                        title="Backend servers"
                        description={<>Servers linked to this proxy. Paste each stable address into your server list in <code className="srv-num text-(--base-08)">{filename}</code>.</>}
                    >
                        {backends.length === 0 ? (
                            <p className="text-xs text-(--base-06)">No servers assigned yet. Link them in the Network tab.</p>
                        ) : (
                            <ul className="-mx-5 -mb-5">
                                {backends.map(b => {
                                    const addr = backendAddress(b.uuid, b.containerPort);
                                    return (
                                        <li key={b.id} className="srv-config-item flex-col gap-2">
                                            <div className="flex min-w-0 items-center gap-2">
                                                <span className="truncate text-sm font-medium text-(--base-09)">{b.name}</span>
                                                {b.activeSubServer && <span className="srv-config-pill shrink-0">{b.activeSubServer}</span>}
                                            </div>
                                            <div className="flex w-full items-center gap-2">
                                                <code className="srv-config-key min-w-0 flex-1 truncate !text-xs">{addr}</code>
                                                <Tooltip content={copiedKey === `b-${b.id}` ? 'Copied' : 'Copy address'}>
                                                    <button
                                                        type="button"
                                                        onClick={() => copy(`b-${b.id}`, addr)}
                                                        aria-label={`Copy address of ${b.name}`}
                                                        className="btn btn-ghost btn-icon btn-sm shrink-0"
                                                    >
                                                        {copiedKey === `b-${b.id}` ? <Check size={14} className="text-(--success-light)" /> : <Copy size={14} />}
                                                    </button>
                                                </Tooltip>
                                            </div>
                                        </li>
                                    );
                                })}
                            </ul>
                        )}
                    </ConfigSection>

                    <Callout tone="info" icon={<Info size={16} />}>{proxyPrereqHint(installerType)}</Callout>
                </aside>
            </div>
        </div>
    );
}
