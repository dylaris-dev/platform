"use client";

import { useState } from 'react';
import Link from 'next/link';

import { Terminal, Globe, FolderOpen, Copy, Loader2, LayoutTemplate } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { API_URL } from '@/lib/api';
import FileBrowserView from '@/views/FileBrowserView';
import { toast } from '@/components/ui/Toast';
import Tooltip from '@/components/ui/Tooltip';
import { useRouteId } from '@/lib/routeParams';
import { downloadBeamApp } from '@/lib/beamDownload';
import { isWails } from '@/lib/adapters';
import { templatesVisible } from '@/lib/templates';
import CaptureTemplateDialog from '@/components/templates/CaptureTemplateDialog';

export default function ServerFilesPage() {
    const paramId = useRouteId('servers');
    const { servers, user, fileAccessMode, beamSettings, featureFlags } = useAppData();
    const server = servers.find(s => s.id === Number(paramId));

    // Beam-download UI state: lives at page scope so the toast/spinner stay
    // mounted even if the file browser below re-renders.
    const [downloading, setDownloading] = useState(false);
    const [capturing, setCapturing] = useState(false);

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
        <div className="flex flex-col gap-4 h-full">
            {hasInfoBar && (
                <div className="srv-surface px-5 py-4 flex flex-wrap items-center gap-x-6 gap-y-3 shrink-0">
                    {showSftp && (
                        <div className="flex items-center gap-3 min-w-0">
                            <Terminal size={14} className="text-(--base-06) shrink-0" />
                            <div className="min-w-0">
                                <div className="srv-label mb-1">SFTP</div>
                                <div className="srv-num flex items-center gap-2 text-xs text-(--base-08)">
                                    <span>{server.nodeAddress}</span>
                                    <span className="text-(--base-05)">:</span>
                                    <span>25520</span>
                                    <span className="text-(--base-05)">·</span>
                                    <span>{user?.username}</span>
                                </div>
                                <div className="text-xs text-(--base-06) mt-0.5">
                                    Using two-factor authentication? Sign in with an SSH key from{' '}
                                    <Link href="/account/ssh-keys" className="text-(--accent-light) hover:underline">Account &gt; SSH keys</Link>.
                                </div>
                            </div>
                            <Tooltip content="Copy SFTP command">
                                <button
                                    type="button"
                                    onClick={() => navigator.clipboard.writeText(`sftp ${user?.username}@${server.nodeAddress} -p 25520`)}
                                    aria-label="Copy SFTP command"
                                    className="text-(--base-06) hover:text-(--base-09) hover:bg-(--base-03) active:bg-(--base-04) focus-visible:outline-none focus-visible:shadow-(--focus-ring) transition-colors shrink-0 p-1.5 rounded-md"
                                >
                                    <Copy size={14} />
                                </button>
                            </Tooltip>
                        </div>
                    )}
                    {fileAccessMode === 'both' && beamEnabled && (
                        <div className="w-px h-8 bg-(--base-04) hidden sm:block" />
                    )}
                    {beamEnabled && (
                        <div className="flex items-center gap-3 min-w-0">
                            <Globe size={14} className="text-(--accent-light) shrink-0" />
                            <div className="min-w-0">
                                <div className="srv-label mb-1">Beam Desktop</div>
                                <div className="text-xs text-(--base-08)">High-speed transfers via the Beam app</div>
                            </div>
                        </div>
                    )}
                    {beamEnabled && (
                        <Tooltip content="Connects directly to the relay, so transfers do not hit Core">
                            <button
                                type="button"
                                onClick={handleBeamDownload}
                                disabled={downloading}
                                className="btn btn-secondary btn-sm ml-auto shrink-0 disabled:opacity-50"
                            >
                                {downloading
                                    ? <Loader2 size={14} className="animate-spin" />
                                    : <FolderOpen size={14} />}
                                {downloading ? 'Downloading…' : 'Download Beam'}
                            </button>
                        </Tooltip>
                    )}
                </div>
            )}
            <FileBrowserView
                serverUuid={server.uuid}
                currentServerPath={server.activeSubServer || ''}
                readOnly={server.role === 'demo'}
                toolbarActions={server.role !== 'demo' && templatesVisible(featureFlags.templates, !!user?.isAdmin) && (
                    <Tooltip content="Save files of this server as a template">
                        <button type="button" onClick={() => setCapturing(true)} className="btn btn-secondary px-3 h-[37px] text-sm" aria-label="Save as template">
                            <LayoutTemplate size={16} /> <span className="hidden md:inline">Save as template</span>
                        </button>
                    </Tooltip>
                )}
            />
            {capturing && <CaptureTemplateDialog serverId={server.id} onClose={() => setCapturing(false)} />}

        </div>
    );
}
