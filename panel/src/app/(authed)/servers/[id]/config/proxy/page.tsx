"use client";

import { toast as notifyToast } from '@/components/ui/Toast';

import { useCallback, useEffect, useState, lazy, Suspense } from 'react';

import { AlertTriangle, Network, Copy, Info } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { getFileContent, saveFile } from '@/lib/api';
import { proxyConfigFilename, backendAddress, proxyPrereqHint } from '@/lib/proxyConfig';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import { SettingsCardSave } from '@/components/settings/SettingsCard';
import { useRouteId } from '@/lib/routeParams';

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
        <div className="flex flex-col lg:flex-row gap-4 h-full min-h-0">
            {/* Config file editor */}
            <div className="flex-1 flex flex-col min-h-0 min-w-0">
                <div className="flex items-center justify-between gap-3 mb-3">
                    <div className="flex items-center gap-2 text-xs text-(--base-06) min-w-0">
                        <span className="truncate">
                            Raw <code className="font-mono text-(--base-08)">{filename}</code>. Changes are not applied until you save and restart the proxy.
                        </span>
                    </div>
                    {/* A file editor rather than a card, so the pair sits in
                        this column's own header. Refused while the file does
                        not exist: there is nothing to write it into. */}
                    <div className="flex items-center gap-2 shrink-0">
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

                {loading ? (
                    <div className="flex-1 flex items-center justify-center text-sm text-(--base-07)">Loading…</div>
                ) : error ? (
                    <div className="card card-pad flex items-start gap-2 text-sm text-(--error-light)">
                        <AlertTriangle size={16} className="mt-0.5 shrink-0" />
                        <span>{error}</span>
                    </div>
                ) : notFound ? (
                    <div className="card card-pad text-sm text-(--base-07) space-y-2">
                        <p>The <code className="font-mono text-(--base-08)">{filename}</code> file doesn&apos;t exist yet.</p>
                        <p className="text-(--base-06)">
                            A proxy writes its config on first launch. Start the proxy once to generate it, then reload
                            this tab. You can also create it from the Files tab.
                        </p>
                        <button onClick={reload} className="btn btn-secondary btn-sm">Reload</button>
                    </div>
                ) : (
                    <div className="flex-1 min-h-0 rounded-md bg-(--base-01) border border-(--base-03) overflow-hidden">
                        <Suspense fallback={<div className="h-full flex items-center justify-center text-sm text-(--base-07)">Loading editor…</div>}>
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

            {/* Backend servers column */}
            <div className="lg:w-80 shrink-0 flex flex-col gap-3">
                <div className="card card-pad">
                    <h3 className="modal-title text-sm mb-1 flex items-center gap-2">
                        <Network size={16} className="text-(--accent-light)" />
                        Backend Servers
                    </h3>
                    <p className="text-xs text-(--base-06) mb-3">
                        Servers linked to this proxy. Paste each stable address into your server list in <code className="font-mono text-(--base-08)">{filename}</code>.
                    </p>
                    {backends.length === 0 ? (
                        <p className="text-xs text-(--base-06) italic">No servers assigned yet. Link them in the Network tab.</p>
                    ) : (
                        <div className="space-y-2">
                            {backends.map(b => {
                                const addr = backendAddress(b.uuid, b.containerPort);
                                return (
                                    <div key={b.id} className="bg-(--base-02) rounded-md px-3 py-2 border border-(--base-03)">
                                        <div className="flex items-center gap-2 min-w-0">
                                            <span className="text-sm font-medium text-(--base-09) truncate">{b.name}</span>
                                            {b.activeSubServer && (
                                                <span className="mono-label bg-(--base-03) px-1.5 py-0.5 rounded-sm text-(--base-06) shrink-0">{b.activeSubServer}</span>
                                            )}
                                        </div>
                                        <div className="flex items-center gap-1.5 mt-1">
                                            <code className="font-mono text-xs text-(--base-08) bg-(--base-03) px-1.5 py-0.5 rounded truncate">{addr}</code>
                                            <button
                                                onClick={() => copy(`b-${b.id}`, addr)}
                                                className="text-(--base-06) hover:text-(--base-09) transition-colors shrink-0"
                                                title="Copy address"
                                            >
                                                <Copy size={11} />
                                            </button>
                                            {copiedKey === `b-${b.id}` && <span className="mono-label text-(--success-light) shrink-0">copied</span>}
                                        </div>
                                    </div>
                                );
                            })}
                        </div>
                    )}
                </div>

                <div className="flex items-start gap-2.5 bg-(--base-03) border border-(--base-04) rounded-xl px-3 py-2.5">
                    <Info size={14} className="text-(--base-06) mt-0.5 shrink-0" />
                    <p className="text-xs text-(--base-07)">{proxyPrereqHint(installerType)}</p>
                </div>
            </div>


        </div>
    );
}
