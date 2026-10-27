'use client';

import { useEffect, useRef, useState } from 'react';
import Link from 'next/link';
import { Download, ExternalLink, Loader2, X } from 'lucide-react';
import type { Pack, PackBuild } from '@/lib/api/packs';
import { mrpackDownloadUrl } from '@/lib/api/packsPublish';
import { getAuthHeader } from '@/lib/api/core';
import { packServerGuide } from '@/lib/packDownloadGuide';
import ModalPanel from '@/components/ui/ModalPanel';

export default function PackDownloadDialog({ pack, build, kind, onClose }: { pack: Pack; build: PackBuild; kind: 'client' | 'server'; onClose: () => void }) {
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');
    const [complete, setComplete] = useState(false);
    const [os, setOS] = useState<'windows' | 'linux'>('windows');
    const request = useRef<AbortController | null>(null);
    useEffect(() => () => { request.current?.abort(); }, []);
    const close = () => { request.current?.abort(); onClose(); };
    const guide = packServerGuide(build, os);
    const download = async () => {
        if (busy) return;
        const controller = new AbortController();
        request.current = controller;
        setBusy(true); setError(''); setComplete(false);
        try {
            const response = await fetch(`${mrpackDownloadUrl(pack.id, build.id)}?kind=${kind}`, { headers: getAuthHeader(), signal: controller.signal });
            if (!response.ok) {
                if (response.status === 503) throw new Error('The server archive is being prepared. Try again in a minute. Large packs can take longer on their first download.');
                throw new Error(`Download failed (${response.status}). Check your access and try again.`);
            }
            const blob = await response.blob();
            if (controller.signal.aborted) return;
            const url = URL.createObjectURL(blob);
            const anchor = document.createElement('a');
            anchor.href = url;
            anchor.download = `${pack.internalSlug}-${build.versionString}${kind === 'server' ? '-server.zip' : '.mrpack'}`;
            document.body.appendChild(anchor); anchor.click(); anchor.remove();
            // Give the browser time to start reading the object URL.
            setTimeout(() => URL.revokeObjectURL(url), 30_000);
            setComplete(true);
        } catch (failure) {
            if (!controller.signal.aborted) setError(failure instanceof Error ? failure.message : 'Download failed. Try again.');
        } finally { if (!controller.signal.aborted) setBusy(false); }
    };
    return <div className="modal-overlay" onClick={close}><ModalPanel onClose={close} className="modal-panel max-w-2xl" onClick={event => event.stopPropagation()}>
        <div className="modal-header flex items-start justify-between gap-3"><div><h2 className="modal-title">{kind === 'client' ? 'Download client modpack' : 'Download server pack'}</h2><p className="text-xs text-(--base-06) mt-1">{pack.internalName} · {build.versionString} · Minecraft {build.minecraft} · {build.loader} {build.loaderVersion}</p></div><button onClick={close} className="btn btn-ghost btn-sm" aria-label="Close download guide"><X size={16} /></button></div>
        <div className="modal-body space-y-5 text-sm text-(--base-07)">
            {kind === 'client' ? <>
                <p>The .mrpack contains the build manifest, uploaded content and configs. Your launcher downloads the pinned Modrinth files and installs the matching Minecraft and loader versions.</p>
                <ol className="list-decimal pl-5 space-y-2"><li>Download this build below.</li><li>Open the Modrinth App and import the downloaded .mrpack as a new instance using its file import option.</li><li>Wait for installation to finish, choose the memory allocation your pack needs, then launch the instance.</li></ol>
                <a href="https://modrinth.com/app" target="_blank" rel="noopener noreferrer" className="btn btn-secondary btn-sm">Get the Modrinth App <ExternalLink size={12} /></a>
            </> : <>
                <section className="card p-4 space-y-2"><h3 className="font-medium text-(--base-09)">Install through DYLARIS</h3><p>Create a server, choose <strong>My modpacks</strong> as the installation method, choose <strong>{pack.internalName}</strong> and build <strong>{build.versionString}</strong>, then complete the setup. DYLARIS installs the matching loader and server content. You do not need to upload this ZIP.</p><Link href="/servers" className="btn btn-secondary btn-sm">Open servers</Link></section>
                <section className="space-y-3"><div className="flex items-center justify-between"><h3 className="font-medium text-(--base-09)">Manual installation</h3><div role="group" aria-label="Server operating system" className="flex gap-1">{(['windows', 'linux'] as const).map(value => <button key={value} aria-pressed={os === value} onClick={() => setOS(value)} className={`btn btn-sm ${os === value ? 'btn-primary' : 'btn-secondary'}`}>{value === 'windows' ? 'Windows' : 'Linux'}</button>)}</div></div>
                    <p>The ZIP includes server-compatible mods and configs. Client-only entries are excluded. It does not include Minecraft, Java or the loader installer.</p>
                    <ol className="list-decimal pl-5 space-y-3"><li>Install <strong>{guide.java}</strong>. Create an empty server directory and extract this ZIP there, keeping its folders.</li><li>Download the official <strong>{build.loader || 'Minecraft'}</strong> installer for Minecraft <strong>{build.minecraft}</strong>{build.loaderVersion && <> and loader <strong>{build.loaderVersion}</strong></>}.{guide.url && <> <a href={guide.url} target="_blank" rel="noopener noreferrer" className="text-(--accent-light) underline">Official download <ExternalLink size={11} className="inline" /></a></>}{guide.install && <><p className="mt-2">Rename the installer to {build.loader}-installer.jar and run this from the server directory:</p><pre className="bg-(--base-02) p-3 rounded-md overflow-x-auto text-xs mt-2"><code>{guide.install}</code></pre></>}</li><li>{guide.memory || 'Follow the loader documentation for this exact version.'}{guide.start && <pre className="bg-(--base-02) p-3 rounded-md overflow-x-auto text-xs mt-2"><code>{guide.start}</code></pre>}{!guide.supported && <p>Automatic commands are unavailable for this loader or version. Follow its official server installation guide.</p>}</li><li>On first start, review Minecraft&apos;s EULA in eula.txt, accept it if you agree, then run the start command again. Check the console for missing dependencies and adjust memory before inviting players.</li></ol>
                </section>
            </>}
            {error && <p role="alert" className="text-(--error-light)">{error}</p>}
            {complete && <p role="status" className="text-(--success-light)">Download started. Follow the steps above to install this build.</p>}
        </div>
        <div className="modal-footer"><button onClick={close} className="btn btn-secondary">Close</button><button onClick={download} disabled={busy} className="btn btn-primary">{busy ? <Loader2 size={14} className="animate-spin" /> : <Download size={14} />}{busy ? 'Preparing download...' : kind === 'client' ? 'Download .mrpack' : 'Download server ZIP'}</button></div>
    </ModalPanel></div>;
}
