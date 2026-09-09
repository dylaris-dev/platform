"use client";

import { useState } from 'react';

import { Terminal, Globe, FolderOpen, Copy, Loader2 } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { API_URL } from '@/lib/api';
import FileBrowserView from '@/views/FileBrowserView';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';
import { downloadBeamApp } from '@/lib/beamDownload';
import { isWails } from '@/lib/adapters';

export default function ServerFilesPage() {
    const paramId = useRouteId('servers');
    const { servers, user, fileAccessMode, beamSettings } = useAppData();
    const server = servers.find(s => s.id === Number(paramId));

    // Beam-download UI state: lives at page scope so the toast/spinner stay
    // mounted even if the file browser below re-renders.
    const [downloading, setDownloading] = useState(false);

    if (!server) return null;

    // Beam's own half of the bar is hidden INSIDE Beam. The app proxies this
    // page, so it was offering the user a download of the program they were
    // reading it in - and taking the width to do it. BeamDownloadButton in the
    // navbar was fixed for exactly this and documents it; this page was the one
    // Beam surface that never asked.
    //
    // Read at render, not in an effect: window.go is injected before the first
    // render in Wails, which is the assumption FileBrowserView already relies on.
    const insideBeam = isWails();
    const beamEnabled = (fileAccessMode === 'beam' || fileAccessMode === 'both')
        && beamSettings?.enabled !== false
        && !insideBeam;

    // SFTP stays: it is a way to reach the files that has nothing to do with
    // which program is showing this page.
    const showSftp = (fileAccessMode === 'sftp' || fileAccessMode === 'both') && server.nodeAddress;
    const hasInfoBar = showSftp || beamEnabled;

    // Errors stay up longer than successes: the failures here are the long
    // relay/platform explanations, and 3.5s is not enough to read one.
    const showToast = (msg: string, ok: boolean) => toast(msg, ok, { durationMs: ok ? 3500 : 6000 });

    // Fetch the binary through Core (which proxies to the relay) instead of
    // opening the URL in a new tab. The old <a href> approach made an error
    // response — JSON {success:false, message:...} — render as a raw JSON
    // page in a fresh tab, which is awful. With fetch we can parse the
    // structured error and surface it as a toast.
    const handleBeamDownload = async () => {
        if (downloading) return;
        setDownloading(true);
        const err = await downloadBeamApp();
        showToast(err ?? 'Beam app downloaded.', err === null);
        setDownloading(false);
    };

    return (
        <div className="flex flex-col gap-3 h-full">
            {hasInfoBar && (
                <div className="card px-4 py-3 flex flex-wrap items-center gap-4 shrink-0">
                    {showSftp && (
                        <div className="flex items-center gap-3 min-w-0">
                            <Terminal size={14} className="text-(--base-06) shrink-0" />
                            <div className="min-w-0">
                                <div className="input-label mb-0.5">SFTP</div>
                                <div className="flex items-center gap-2 font-mono text-xs text-(--base-08)">
                                    <span>{server.nodeAddress}</span>
                                    <span className="text-(--base-05)">:</span>
                                    <span>25520</span>
                                    <span className="text-(--base-05)">·</span>
                                    <span>{user?.username}</span>
                                </div>
                            </div>
                            <button
                                onClick={() => navigator.clipboard.writeText(`sftp ${user?.username}@${server.nodeAddress} -p 25520`)}
                                className="text-(--base-05) hover:text-(--base-09) transition-colors shrink-0 p-1 rounded"
                                title="Copy SFTP command"
                            >
                                <Copy size={13} />
                            </button>
                        </div>
                    )}
                    {fileAccessMode === 'both' && beamEnabled && (
                        <div className="w-px h-8 bg-(--base-03) hidden sm:block" />
                    )}
                    {beamEnabled && (
                        <div className="flex items-center gap-3 min-w-0">
                            <Globe size={14} className="text-(--accent-light) shrink-0" />
                            <div className="min-w-0">
                                <div className="input-label mb-0.5">Beam Desktop</div>
                                <div className="text-xs text-(--base-08)">High-speed transfers via the Beam app</div>
                            </div>
                        </div>
                    )}
                    {beamEnabled && (
                        <button
                            type="button"
                            onClick={handleBeamDownload}
                            disabled={downloading}
                            className="btn btn-secondary btn-sm ml-auto shrink-0 disabled:opacity-50"
                            title="Download the Beam Desktop app — connects directly to the relay so transfers don't hit Core"
                        >
                            {downloading
                                ? <Loader2 size={12} className="animate-spin" />
                                : <FolderOpen size={12} />}
                            {downloading ? 'Downloading…' : 'Download Beam'}
                        </button>
                    )}
                </div>
            )}
            <FileBrowserView serverUuid={server.uuid} currentServerPath={server.activeSubServer || ''} readOnly={server.role === 'demo'} />

        </div>
    );
}
