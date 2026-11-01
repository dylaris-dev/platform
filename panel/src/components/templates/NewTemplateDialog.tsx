"use client";

import React, { useEffect, useState } from 'react';
import { FileArchive, FilePlus, Loader2, Server as ServerIcon, X, LayoutTemplate } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';
import Tabs from '@/components/ui/Tabs';
import { useAppData } from '@/lib/AppDataContext';
import { MAX_CAPTURE_PATHS, softwareVersionLabel } from '@/lib/templates';
import { overPathLimit, TOO_MANY_PATHS_MESSAGE } from '@dylaris/ui-filebrowser';
import ServerFilesPicker from './ServerFilesPicker';
import {
    captureTemplate, createTemplate, uploadTemplate,
    type SkippedFile, type Template, type TemplateKind, type TemplateMeta, type TemplateVisibility,
} from '@/lib/api/templates';

type Mode = 'upload' | 'server' | 'empty';

// Creates a template three ways: from a zip, from files of one of the caller's
// servers, or empty. Errors are Core's message verbatim, inside the dialog.
// With fromServerId it is "Save as template" for that one server only.
export default function NewTemplateDialog({
    onClose,
    onCreated,
    fromServerId,
}: {
    onClose: () => void;
    /** skipped is what Core left out; the caller shows it as an alert. */
    onCreated: (template: Template, skipped: SkippedFile[]) => void;
    fromServerId?: number;
}) {
    const { user, servers } = useAppData();
    const isAdmin = !!user?.isAdmin;
    const usable = servers.filter((s) => s.role !== 'demo');

    const [mode, setMode] = useState<Mode>(fromServerId ? 'server' : 'upload');
    const [name, setName] = useState('');
    const [kind, setKind] = useState<TemplateKind>('server');
    const [visibility, setVisibility] = useState<TemplateVisibility>('personal');
    const [software, setSoftware] = useState('');
    const [mcVersion, setMcVersion] = useState('');
    const [softwareVersion, setSoftwareVersion] = useState('');
    const [file, setFile] = useState<File | null>(null);

    const [serverId, setServerId] = useState<number | null>(fromServerId ?? usable[0]?.id ?? null);
    const [subServer, setSubServer] = useState('');
    const [selected, setSelected] = useState<string[]>([]);

    const [busy, setBusy] = useState(false);
    const [error, setError] = useState<string | null>(null);

    const server = usable.find((s) => s.id === serverId) ?? null;
    // Primitives, so a refreshed server list does not reset the selection.
    const uuid = server?.uuid;
    const activeSub = server?.activeSubServer || '';
    const isProxy = server?.serverType === 'proxy';
    const onPick = (sub: string, paths: string[]) => { setSubServer(sub); setSelected(paths); };
    // Core refuses more with a message about choosing a path, which misleads here.
    const tooMany = overPathLimit(selected, MAX_CAPTURE_PATHS);

    // A proxy server makes a proxy template; the field stays editable.
    useEffect(() => {
        if (mode === 'server' && uuid) setKind(isProxy ? 'proxy' : 'server');
    }, [mode, uuid, isProxy]);

    const submit = async (e: React.FormEvent) => {
        e.preventDefault();
        setError(null);
        if (!name.trim()) { setError('Enter a name.'); return; }
        const meta: TemplateMeta = { name: name.trim(), kind, visibility, software: software.trim(), mcVersion: mcVersion.trim(), softwareVersion: softwareVersion.trim() };
        setBusy(true);
        try {
            if (mode === 'upload') {
                if (!file) { setError('Choose a zip file.'); return; }
                const res = await uploadTemplate(meta, file);
                if (!res.success || !res.template) { setError(res.message || 'Upload failed.'); return; }
                onCreated(res.template, res.skipped ?? []);
            } else if (mode === 'server') {
                if (!server || !subServer) { setError('Choose a server.'); return; }
                if (selected.length === 0) { setError('Choose at least one file or folder.'); return; }
                if (tooMany) { setError(TOO_MANY_PATHS_MESSAGE); return; }
                const res = await captureTemplate({ ...meta, serverId: server.id, subServer, selected });
                if (!res.success || !res.template) { setError(res.message || 'Could not capture the files.'); return; }
                onCreated(res.template, res.skipped ?? []);
            } else {
                const res = await createTemplate(meta);
                if (!res.success || !res.id) { setError(res.message || 'Could not create the template.'); return; }
                onCreated(res as Template, []);
            }
        } finally {
            setBusy(false);
        }
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={busy ? undefined : onClose}>
            <ModalPanel as="form" onSubmit={submit} onClose={onClose} dismissible={!busy} className="modal-panel max-w-lg" onClick={(e) => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between gap-3">
                    <h3 className="modal-title flex items-center gap-2"><LayoutTemplate size={16} /> {fromServerId ? 'Save as template' : 'New template'}</h3>
                    <button type="button" onClick={onClose} disabled={busy} aria-label="Close" className="text-(--base-06) hover:text-(--base-09) disabled:opacity-40"><X size={16} /></button>
                </div>
                <div className="modal-body space-y-4">
                    {!fromServerId && <Tabs<Mode>
                        ariaLabel="Start from"
                        active={mode}
                        onChange={(m) => { setMode(m); setError(null); }}
                        items={[
                            { id: 'upload', label: 'Upload zip', icon: FileArchive },
                            { id: 'server', label: 'From a server', icon: ServerIcon },
                            { id: 'empty', label: 'Empty', icon: FilePlus },
                        ]}
                    />}

                    <div>
                        <label htmlFor="tpl-name" className="input-label">Name</label>
                        <input id="tpl-name" autoFocus className="input-field w-full" value={name} onChange={(e) => setName(e.target.value)} maxLength={64} placeholder="Survival base" />
                    </div>

                    {mode === 'upload' && (
                        <div>
                            <label htmlFor="tpl-file" className="input-label">Zip file</label>
                            <input id="tpl-file" type="file" accept=".zip,application/zip" className="input-field w-full" onChange={(e) => setFile(e.target.files?.[0] ?? null)} />
                            <p className="text-xs text-(--base-06) mt-1">The zip holds the server folder: server.properties, config, world and so on.</p>
                        </div>
                    )}

                    {mode === 'server' && (
                        usable.length === 0 || !server ? (
                            <p className="text-xs text-(--base-06)">You have no servers to capture from.</p>
                        ) : (
                            <div className="space-y-3">
                                {!fromServerId && (
                                    <div>
                                        <label htmlFor="tpl-server" className="input-label">Server</label>
                                        <select id="tpl-server" className="input-field w-full sm:w-1/2" value={serverId ?? ''} disabled={busy} onChange={(e) => setServerId(Number(e.target.value))}>
                                            {usable.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
                                        </select>
                                    </div>
                                )}
                                <ServerFilesPicker uuid={server.uuid} preferredSub={activeSub} disabled={busy} onChange={onPick} />
                            </div>
                        )
                    )}

                    <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                        <div>
                            <label htmlFor="tpl-kind" className="input-label">Kind</label>
                            <select id="tpl-kind" className="input-field w-full" value={kind} onChange={(e) => setKind(e.target.value as TemplateKind)}>
                                <option value="server">Server</option>
                                <option value="proxy">Proxy</option>
                            </select>
                        </div>
                        {isAdmin && (
                            <div>
                                <label htmlFor="tpl-vis" className="input-label">Visibility</label>
                                <select id="tpl-vis" className="input-field w-full" value={visibility} onChange={(e) => setVisibility(e.target.value as TemplateVisibility)}>
                                    <option value="personal">Personal</option>
                                    <option value="admin">Admin</option>
                                    <option value="platform">Platform</option>
                                </select>
                            </div>
                        )}
                        <div>
                            <label htmlFor="tpl-software" className="input-label">Software (optional)</label>
                            <input id="tpl-software" className="input-field w-full" value={software} onChange={(e) => setSoftware(e.target.value)} maxLength={64} placeholder="paper" />
                        </div>
                        <div>
                            <label htmlFor="tpl-mc" className="input-label">Minecraft version (optional)</label>
                            <input id="tpl-mc" className="input-field w-full" value={mcVersion} onChange={(e) => setMcVersion(e.target.value)} maxLength={64} placeholder="1.21.4" />
                        </div>
                        <div>
                            <label htmlFor="tpl-swv" className="input-label">{softwareVersionLabel(software.trim())} (optional)</label>
                            <input id="tpl-swv" className="input-field w-full" value={softwareVersion} onChange={(e) => setSoftwareVersion(e.target.value)} maxLength={64} />
                        </div>
                    </div>
                    {mode === 'server' && <p className="text-xs text-(--base-06)">Left empty, software and versions are taken from how the sub-server was installed.</p>}

                    {error && <div className="card p-3 border border-(--error) bg-(--error)/10 text-xs text-(--base-09)" role="alert">{error}</div>}
                </div>
                <div className="modal-footer">
                    <button type="button" onClick={onClose} disabled={busy} className="btn btn-secondary disabled:opacity-40">Cancel</button>
                    <button type="submit" disabled={busy || (mode === 'server' && (!server || selected.length === 0 || tooMany))} className="btn btn-primary disabled:opacity-40 disabled:cursor-not-allowed">
                        {busy ? <><Loader2 size={14} className="animate-spin" /> {mode === 'empty' ? 'Creating' : mode === 'upload' ? 'Uploading' : 'Capturing'}...</> : 'Create'}
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
