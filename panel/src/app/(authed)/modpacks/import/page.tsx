'use client';

import { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { useRouter } from 'next/navigation';
import { Download, Eye, EyeOff, ExternalLink, RefreshCw } from 'lucide-react';
import { listModrinthImportProjects, setModrinthProjectHidden, importModrinthProject, type ModrinthImportProject } from '@/lib/api/packs';
import { MODRINTH_STATUS_LABEL } from '@/lib/modrinthChecklist';
import ModrinthSyncDialog from '@/components/modpacks/ModrinthSyncDialog';
import { toast } from '@/components/ui/Toast';
import { SkeletonList } from '@/components/Skeleton';
import styles from '@/components/modpacks/workspace.module.css';

export default function ModrinthImportPage() {
    const router = useRouter();
    const [projects, setProjects] = useState<ModrinthImportProject[] | null>(null);
    const [error, setError] = useState('');
    const [needsToken, setNeedsToken] = useState(false);
    const [showHidden, setShowHidden] = useState(false);
    const [busy, setBusy] = useState('');
    const [syncing, setSyncing] = useState<{ packId: number; title: string } | null>(null);

    const load = useCallback(async () => {
        setError(''); setProjects(null);
        const res = await listModrinthImportProjects();
        if (res.success && res.projects) { setProjects(res.projects); return; }
        setNeedsToken(/token/i.test(res.message || ''));
        setError(res.message || 'Could not read your Modrinth projects.');
    }, []);
    useEffect(() => { void load(); }, [load]);

    const toggleHidden = async (p: ModrinthImportProject) => {
        const res = await setModrinthProjectHidden(p.id, !p.hidden);
        if (!res.success) { toast(res.message || 'Could not save.', false); return; }
        setProjects(prev => prev && prev.map(x => x.id === p.id ? { ...x, hidden: !p.hidden } : x));
    };
    const startImport = async (p: ModrinthImportProject) => {
        setBusy(p.id);
        try {
            const res = await importModrinthProject(p.id);
            if (!res.success || !res.pack) { toast(res.message || 'Import failed.', false); return; }
            setSyncing({ packId: res.pack.id, title: p.title });
        } finally { setBusy(''); }
    };

    const hiddenCount = projects?.filter(p => p.hidden).length ?? 0;
    const visible = projects?.filter(p => showHidden || !p.hidden) ?? [];
    return <div className={styles.scroll}>
        <div className="max-w-4xl mx-auto space-y-4">
            <header className="flex items-start justify-between gap-3 flex-wrap">
                <div>
                    <h1 className="text-lg font-medium text-(--base-09)">Import from Modrinth</h1>
                    <p className="text-sm text-(--base-06) mt-1">Modpacks you are a member of on Modrinth. Importing creates a pack here; every Modrinth version becomes a read-only build you can copy into new builds.</p>
                </div>
                <div className="flex gap-2">
                    {hiddenCount > 0 && <button className="btn btn-secondary btn-sm" onClick={() => setShowHidden(v => !v)}>{showHidden ? <EyeOff size={12} /> : <Eye size={12} />}{showHidden ? 'Hide hidden' : `Show hidden (${hiddenCount})`}</button>}
                    <button className="btn btn-ghost btn-sm" onClick={() => void load()}><RefreshCw size={12} />Refresh</button>
                </div>
            </header>

            {error && <div role="alert" className="card p-5 text-sm space-y-3">
                <p>{error}</p>
                {needsToken ? <Link href="/account/modrinth" className="btn btn-primary btn-sm">Connect your Modrinth token</Link> : <button className="btn btn-secondary btn-sm" onClick={() => void load()}>Retry</button>}
            </div>}
            {!projects && !error && <SkeletonList rows={4} />}
            {projects && visible.length === 0 && <div className="card p-6 text-sm text-(--base-06)">{projects.length === 0 ? 'Your Modrinth account has no modpack projects yet.' : 'All your projects are hidden.'}</div>}

            <ul className="space-y-2">{visible.map(p => <li key={p.id} className={`card px-4 py-3 flex items-center gap-3 ${p.hidden ? 'opacity-60' : ''}`}>
                {p.icon_url ? <img src={p.icon_url} alt="" className="w-11 h-11 rounded-md object-cover shrink-0" /> : <div className="w-11 h-11 rounded-md bg-(--base-03) shrink-0" aria-hidden="true" />}
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2 flex-wrap"><span className="font-medium text-(--base-09) truncate">{p.title}</span><span className="text-[11px] text-(--base-06)">{MODRINTH_STATUS_LABEL[p.status]?.split(' - ')[0] || p.status} · {p.versions.length} version{p.versions.length === 1 ? '' : 's'}</span></div>
                    <p className="text-xs text-(--base-06) truncate mt-0.5" title={p.description}>{p.description}</p>
                </div>
                <div className="flex items-center gap-1.5 shrink-0">
                    <a href={`https://modrinth.com/modpack/${encodeURIComponent(p.slug || p.id)}`} target="_blank" rel="noopener noreferrer" className="btn btn-ghost btn-sm" aria-label={`Open ${p.title} on Modrinth`}><ExternalLink size={13} /></a>
                    <button className="btn btn-ghost btn-sm" onClick={() => void toggleHidden(p)} aria-label={p.hidden ? `Show ${p.title}` : `Hide ${p.title}`} title={p.hidden ? 'Show in this list' : 'Hide from this list'}>{p.hidden ? <Eye size={13} /> : <EyeOff size={13} />}</button>
                    {p.packId
                        ? <><button className="btn btn-secondary btn-sm" onClick={() => setSyncing({ packId: p.packId!, title: p.title })}><RefreshCw size={12} />Sync</button><Link href={`/modpacks/${p.packId}`} className="btn btn-primary btn-sm">Open</Link></>
                        : <button className="btn btn-primary btn-sm" disabled={!!busy} onClick={() => void startImport(p)}><Download size={12} />{busy === p.id ? 'Starting...' : 'Import'}</button>}
                </div>
            </li>)}</ul>
        </div>
        {syncing && <ModrinthSyncDialog packId={syncing.packId} title={syncing.title} onClose={() => { const id = syncing.packId; setSyncing(null); router.push(`/modpacks/${id}`); }} />}
    </div>;
}
