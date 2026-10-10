"use client";

import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { Image as ImageIcon, Upload, Trash2, MessageSquare, RotateCcw, Globe } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { getFileContent, saveFile, getEdgeMotd, setEdgeMotd, type EdgeMotdMode } from '@/lib/api';
import { uploadFiles } from '@/lib/api/files';
import { API_URL, getAuthHeader } from '@/lib/api/core';
import { parseProperties, serializeProperties } from '@/lib/properties-codec';
import { toast } from '@/components/ui/Toast';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import SettingsCard from '@/components/settings/SettingsCard';
import {
    parseMotd, motdVisibleLengths, insertMotdCode,
    MOTD_COLORS, MOTD_STYLES, MOTD_MAX_LINES, MOTD_SOFT_LINE_LIMIT,
} from '@/lib/motd';
import { useRouteId } from '@/lib/routeParams';

// Display sub-tab. Surgically edits motd= in server.properties so
// the user gets a friendly multi-line editor without touching the rest of the
// file, and uploads server-icon.png to the active sub-server dir.
//
// Why not a dedicated /display backend endpoint: server.properties is owned by
// the FileBrowser API and ServerPropertiesView; reusing those endpoints means
// the Display tab automatically benefits from existing auth + audit + permission
// gates without parallel routes to maintain.

const ICON_PATH_NAME = 'server-icon.png';

export default function ServerConfigDisplayPage() {
    const paramId = useRouteId('servers');
    const { servers, gatewayEnabled } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);
    const activeSubServer = server?.activeSubServer || '';
    const serverUuid = server?.uuid || '';
    const propertiesPath = activeSubServer ? `${activeSubServer}/server.properties` : '';

    const [propertiesLoaded, setPropertiesLoaded] = useState(false);
    const [propertiesText, setPropertiesText] = useState('');
    const [motd, setMotd] = useState('');
    const [motdInitial, setMotdInitial] = useState('');
    const [savingMotd, setSavingMotd] = useState(false);
    const [iconBust, setIconBust] = useState(0);
    const [uploadingIcon, setUploadingIcon] = useState(false);
    const fileInputRef = useRef<HTMLInputElement>(null);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    useEffect(() => {
        if (!propertiesPath || !serverUuid) return;
        let cancelled = false;
        (async () => {
            const res = await getFileContent(propertiesPath, serverUuid);
            if (cancelled) return;
            if (res.success) {
                setPropertiesText(res.content || '');
                const parsed = parseProperties(res.content || '');
                // parseProperties already decodes \\n → \n on read; serializeProperties
                // re-escapes on write. We just pass the value through unchanged.
                const decoded = parsed.values['motd'] || '';
                setMotd(decoded);
                setMotdInitial(decoded);
            } else {
                // empty/missing file is fine — first boot
                setMotd('');
                setMotdInitial('');
            }
            setPropertiesLoaded(true);
        })();
        return () => { cancelled = true; };
    }, [propertiesPath, serverUuid]);

    const motdDirty = motd !== motdInitial;
    const previewLines = useMemo(() => parseMotd(motd || 'A Minecraft Server'), [motd]);
    const lineLengths = useMemo(() => motdVisibleLengths(motd), [motd]);
    const tooLong = lineLengths.some(n => n > MOTD_SOFT_LINE_LIMIT);
    const tooManyLines = motd.split('\n').length > MOTD_MAX_LINES;

    // Inserting at the caret rather than appending: someone colouring the
    // middle of a line should not have the code land at the end.
    const motdRef = useRef<HTMLTextAreaElement>(null);
    const applyCode = useCallback((code: string) => {
        const el = motdRef.current;
        const at = el ? el.selectionStart : motd.length;
        const next = insertMotdCode(motd, at, code);
        setMotd(next.value);
        // After React re-renders the controlled value the caret would otherwise
        // jump to the end, undoing the insert position we just computed.
        requestAnimationFrame(() => {
            if (!motdRef.current) return;
            motdRef.current.focus();
            motdRef.current.setSelectionRange(next.caret, next.caret);
        });
    }, [motd]);

    // Edge transitional MOTD (auto/custom/off) - what the gateway edge shows while
    // the server is down/starting/migrating. Server-level (not sub-server), loaded
    // via a dedicated endpoint and published to Redis for the edge on save.
    const [edgeMotdMode, setEdgeMotdMode] = useState<EdgeMotdMode>('auto');
    const [edgeMotdText, setEdgeMotdText] = useState('');
    const [edgeMotdInitial, setEdgeMotdInitial] = useState<{ mode: EdgeMotdMode; text: string }>({ mode: 'auto', text: '' });
    const [edgeMotdLoaded, setEdgeMotdLoaded] = useState(false);
    const [savingEdgeMotd, setSavingEdgeMotd] = useState(false);

    useEffect(() => {
        if (!serverId) return;
        let cancelled = false;
        (async () => {
            const res = await getEdgeMotd(serverId);
            if (cancelled) return;
            if (res.success) {
                const mode = (res.mode || 'auto') as EdgeMotdMode;
                const text = res.customText || '';
                setEdgeMotdMode(mode);
                setEdgeMotdText(text);
                setEdgeMotdInitial({ mode, text });
            }
            setEdgeMotdLoaded(true);
        })();
        return () => { cancelled = true; };
    }, [serverId]);

    const edgeMotdDirty = edgeMotdMode !== edgeMotdInitial.mode || edgeMotdText !== edgeMotdInitial.text;

    const handleSaveEdgeMotd = useCallback(async (): Promise<boolean> => {
        setSavingEdgeMotd(true);
        // Only persist custom text in custom mode; auto/off ignore it.
        const text = edgeMotdMode === 'custom' ? edgeMotdText : '';
        try {
            const res: any = await setEdgeMotd(serverId, edgeMotdMode, text);
            if (res && res.success !== false && !res.error) {
                setEdgeMotdInitial({ mode: edgeMotdMode, text });
                setEdgeMotdText(text);
                showToast('Edge MOTD saved.', true);
                return true;
            }
            showToast(res?.error || res?.message || 'Failed to save edge MOTD', false);
            return false;
        } finally {
            setSavingEdgeMotd(false);
        }
    }, [serverId, edgeMotdMode, edgeMotdText, showToast]);

    const handleSaveMotd = useCallback(async (): Promise<boolean> => {
        if (!propertiesPath || !serverUuid) return false;
        setSavingMotd(true);
        // Surgically rewrite only the motd line — preserves blank lines,
        // comments, key order. The codec re-escapes \n in the value.
        const parsed = parseProperties(propertiesText || '');
        const next = serializeProperties(parsed, { motd });
        try {
            const res = await saveFile(propertiesPath, next, serverUuid);
            if (res.success) {
                setPropertiesText(next);
                setMotdInitial(motd);
                showToast('MOTD saved. Restart the server to apply.', true);
                return true;
            }
            showToast(res.message || 'Failed to save MOTD', false);
            return false;
        } finally {
            setSavingMotd(false);
        }
    }, [propertiesPath, serverUuid, propertiesText, motd, showToast]);

    // Two Revert/Save pairs used to sit on this one page. They are separate
    // registrations rather than one, because they write different things by
    // different routes - the MOTD is a line in server.properties, the edge MOTD
    // is a gateway setting - and the bar saves whatever is dirty, in order.
    useUnsavedChanges({
        dirty: motdDirty,
        saving: savingMotd,
        save: handleSaveMotd,
        discard: () => setMotd(motdInitial),
    });
    useUnsavedChanges({
        dirty: edgeMotdDirty,
        saving: savingEdgeMotd,
        save: handleSaveEdgeMotd,
        discard: () => { setEdgeMotdMode(edgeMotdInitial.mode); setEdgeMotdText(edgeMotdInitial.text); },
    });

    const handleIconUpload = useCallback(async (file: File) => {
        if (!activeSubServer || !serverUuid) return;
        if (file.type !== 'image/png') { showToast('server-icon must be a PNG file', false); return; }
        if (file.size > 1 * 1024 * 1024) { showToast('server-icon must be < 1MB', false); return; }
        // Validate dimensions client-side. Minecraft accepts 64x64 only;
        // anything else is rendered as a blank slot in the multiplayer list.
        const bitmap = await createImageBitmap(file).catch(() => null);
        if (!bitmap) { showToast('Could not decode PNG', false); return; }
        if (bitmap.width !== 64 || bitmap.height !== 64) {
            showToast(`Icon must be exactly 64x64 px (got ${bitmap.width}x${bitmap.height})`, false);
            return;
        }

        setUploadingIcon(true);
        // uploadFiles takes a FileList — wrap our single file in a DataTransfer.
        const dt = new DataTransfer();
        // Rename to server-icon.png so the in-game client picks it up by
        // convention, regardless of what the user named the source file.
        dt.items.add(new File([file], ICON_PATH_NAME, { type: 'image/png' }));
        const res = await uploadFiles(activeSubServer, dt.files, () => { /* progress unused */ }, 'overwrite');
        setUploadingIcon(false);
        if (res.success) {
            setIconBust(b => b + 1);
            showToast('Icon uploaded. Restart the server to apply.', true);
        } else {
            showToast(res.message || 'Upload failed', false);
        }
    }, [activeSubServer, serverUuid, showToast]);

    const handleDeleteIcon = useCallback(async () => {
        if (!activeSubServer || !serverUuid) return;
        try {
            const res = await fetch(`${API_URL}/files/delete`, {
                method: 'POST',
                headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
                body: JSON.stringify({ path: `${activeSubServer}/${ICON_PATH_NAME}`, server_uuid: serverUuid }),
            });
            const data = await res.json();
            if (res.ok && data.success !== false) {
                setIconBust(b => b + 1);
                showToast('Icon removed.', true);
            } else {
                showToast(data.message || 'Delete failed', false);
            }
        } catch {
            showToast('Network error', false);
        }
    }, [activeSubServer, serverUuid, showToast]);

    const iconUrl = useMemo(() => {
        if (!activeSubServer || !serverUuid) return '';
        return `${API_URL}/files/download?path=${encodeURIComponent(`${activeSubServer}/${ICON_PATH_NAME}`)}&server_uuid=${encodeURIComponent(serverUuid)}&_cb=${iconBust}`;
    }, [activeSubServer, serverUuid, iconBust]);

    if (!server) return null;
    if (!activeSubServer) {
        return (
            <div className="max-w-2xl">
                <div className="card p-5 text-sm text-(--base-07)">
                    Install or switch to a sub-server in Setup before editing display options.
                </div>
            </div>
        );
    }

    return (
        <div className="max-w-3xl space-y-6">
            {/* MOTD */}
            <SettingsCard
                title="MOTD"
                icon={MessageSquare}
                form={{ dirty: motdDirty, saving: savingMotd, save: handleSaveMotd, discard: () => setMotd(motdInitial) }}
                description="Two lines, shown in the multiplayer server list. Click a colour or a style to insert its code at the cursor; a colour also clears the styles before it, exactly as the game does. Requires a server restart to take effect."
            >
                {/* Toolbar. The codes are typeable by hand and always were -
                    this is so nobody has to remember that &o is italic. */}
                <div className="space-y-2">
                    <div className="flex flex-wrap gap-1">
                        {MOTD_COLORS.map(c => (
                            <button
                                key={c.code}
                                type="button"
                                onClick={() => applyCode(c.code)}
                                disabled={!propertiesLoaded}
                                title={`${c.name} (&${c.code})`}
                                aria-label={c.name}
                                className="w-7 h-7 rounded-sm border border-(--base-04) hover:scale-110 focus-visible:scale-110 transition-transform disabled:opacity-40 disabled:hover:scale-100"
                                style={{ backgroundColor: c.hex }}
                            />
                        ))}
                    </div>
                    <div className="flex flex-wrap gap-1">
                        {MOTD_STYLES.map(st => (
                            <button
                                key={st.code}
                                type="button"
                                onClick={() => applyCode(st.code)}
                                disabled={!propertiesLoaded}
                                title={`${st.name} (&${st.code}) — ${st.hint}`}
                                className="px-2.5 py-1 rounded-md text-xs bg-(--base-02) border border-(--base-04) text-(--base-07) hover:bg-(--base-03) hover:text-(--base-09) transition-colors disabled:opacity-40"
                            >
                                <span
                                    style={{
                                        fontWeight: st.code === 'l' ? 700 : undefined,
                                        fontStyle: st.code === 'o' ? 'italic' : undefined,
                                        textDecoration: st.code === 'n' ? 'underline'
                                            : st.code === 'm' ? 'line-through' : undefined,
                                    }}
                                >
                                    {st.name}
                                </span>
                            </button>
                        ))}
                    </div>
                </div>

                <textarea
                    ref={motdRef}
                    value={motd}
                    onChange={e => setMotd(e.target.value)}
                    placeholder="A Minecraft Server"
                    rows={2}
                    maxLength={240}
                    disabled={!propertiesLoaded}
                    className="input-field font-mono text-sm resize-none w-full"
                />

                {/* Counted WITHOUT the codes: they are not drawn, so counting
                    raw characters tells the owner a line is too long when it
                    is not. */}
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs">
                    {lineLengths.map((n, i) => (
                        <span key={i} className={n > MOTD_SOFT_LINE_LIMIT ? 'text-(--warning-light)' : 'text-(--base-06)'}>
                            Line {i + 1}: {n}/{MOTD_SOFT_LINE_LIMIT}
                        </span>
                    ))}
                    {tooLong && (
                        <span className="text-(--warning-light)">
                            The client measures pixels, so this is a guide — wide characters run out sooner.
                        </span>
                    )}
                    {tooManyLines && (
                        <span className="text-(--warning-light)">
                            Only the first {MOTD_MAX_LINES} lines are shown in the server list.
                        </span>
                    )}
                </div>

                {/* Preview pane — emulates the multiplayer server-list MOTD slot */}
                <div>
                    <div className="mono-label mb-1.5">Preview</div>
                    <div className="bg-[#1a1a1a] border border-(--base-04) rounded-md p-3 font-mono text-sm leading-tight space-y-0.5">
                        {previewLines.map((segments, i) => (
                            <div key={i}>
                                {segments.map((seg, j) => (
                                    <span
                                        key={j}
                                        style={{
                                            color: seg.color,
                                            fontWeight: seg.bold ? 700 : 400,
                                            fontStyle: seg.italic ? 'italic' : 'normal',
                                            textDecoration: [seg.underline && 'underline', seg.strike && 'line-through'].filter(Boolean).join(' ') || 'none',
                                        }}
                                    >
                                        {seg.text || ' '}
                                    </span>
                                ))}
                            </div>
                        ))}
                    </div>
                </div>

            </SettingsCard>

            {/* Server Icon. Uploads on pick, so it owns no form and shows no
                save: there is nothing pending for a button to commit. */}
            <SettingsCard
                title="Server icon"
                icon={ImageIcon}
                description={<>
                    64x64 PNG, max 1 MB. Stored as{' '}
                    <code className="font-mono">{activeSubServer}/server-icon.png</code>.
                    Requires a server restart to take effect.
                </>}
            >

                <div className="flex items-center gap-4">
                    <div className="w-16 h-16 rounded-md border border-(--base-04) bg-(--base-02) flex items-center justify-center overflow-hidden shrink-0">
                        {/* Cache-bust on every upload so the new icon shows immediately */}
                        {/* eslint-disable-next-line @next/next/no-img-element */}
                        <img
                            src={iconUrl}
                            alt="server icon"
                            className="w-full h-full object-contain"
                            onError={(e) => { (e.target as HTMLImageElement).style.display = 'none'; }}
                            style={{ imageRendering: 'pixelated' }}
                        />
                    </div>
                    <div className="flex items-center gap-2">
                        <input
                            ref={fileInputRef}
                            type="file"
                            accept="image/png"
                            className="hidden"
                            onChange={e => {
                                const f = e.target.files?.[0];
                                if (f) handleIconUpload(f);
                                e.target.value = '';
                            }}
                        />
                        <button
                            type="button"
                            onClick={() => fileInputRef.current?.click()}
                            className="btn btn-secondary btn-sm"
                            disabled={uploadingIcon}
                        >
                            <Upload size={13} />
                            {uploadingIcon ? 'Uploading…' : 'Upload PNG'}
                        </button>
                        <button
                            type="button"
                            onClick={handleDeleteIcon}
                            className="btn btn-secondary btn-sm"
                            disabled={uploadingIcon}
                        >
                            <Trash2 size={13} className="text-(--error-light)" />
                            Remove
                        </button>
                    </div>
                </div>
            </SettingsCard>

            {/* Edge MOTD (transitional). Only the gateway edge can answer a ping
                for a server whose container is not running - it is the thing
                standing in front of the port. With routing on ip_port there is no
                listener at all while the server is down, so the whole card would
                configure something nothing reads. */}
            {gatewayEnabled && (
            <SettingsCard
                title="Edge MOTD (transitional)"
                icon={Globe}
                form={{
                    dirty: edgeMotdDirty,
                    saving: savingEdgeMotd,
                    save: handleSaveEdgeMotd,
                    discard: () => { setEdgeMotdMode(edgeMotdInitial.mode); setEdgeMotdText(edgeMotdInitial.text); },
                }}
                description="What players see via the gateway edge while the server is down, starting, or migrating. Applies when gateway routing is enabled. Takes effect within a few seconds, no restart needed."
            >
                <div className="flex flex-col gap-[5px] max-w-xs">
                    <label className="mono-label">Mode</label>
                    <select
                        value={edgeMotdMode}
                        onChange={e => setEdgeMotdMode(e.target.value as EdgeMotdMode)}
                        disabled={!edgeMotdLoaded}
                        className="input-field text-sm"
                    >
                        <option value="auto">Auto (built-in status notice)</option>
                        <option value="custom">Custom (your own message)</option>
                        <option value="off">Off (no message, vanilla)</option>
                    </select>
                </div>

                {edgeMotdMode === 'custom' && (
                    <div className="flex flex-col gap-[5px]">
                        <label className="mono-label">Custom message</label>
                        <input
                            type="text"
                            value={edgeMotdText}
                            onChange={e => setEdgeMotdText(e.target.value)}
                            placeholder="Be right back, the server is moving to a new home."
                            maxLength={256}
                            disabled={!edgeMotdLoaded}
                            className="input-field font-mono text-sm w-full"
                        />
                        <p className="text-xs text-(--base-06)">
                            Shown while the server is down or migrating. Supports Minecraft colour codes (<code className="font-mono">&amp;a</code>, <code className="font-mono">&amp;l</code>).
                        </p>
                    </div>
                )}
            </SettingsCard>
            )}

        </div>
    );
}
