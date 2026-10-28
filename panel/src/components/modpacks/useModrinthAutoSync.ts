'use client';

import { useEffect, useState } from 'react';
import { listModrinthPackVersions, importModrinthPackVersion, type Pack } from '@/lib/api/packs';

// Opening a pack bound to Modrinth imports versions published since its newest
// one, at most every 10 minutes per pack and 5 versions per visit. Core does
// the same every few hours in the background; this makes a fresh release show
// up when someone looks. Older missing versions are left to "Sync from Modrinth".
const EVERY_MS = 10 * 60 * 1000;
const PER_VISIT = 5;

export type AutoSyncState = { phase: 'idle' } | { phase: 'running'; done: number; total: number } | { phase: 'done'; imported: number } | { phase: 'failed'; message: string };

function due(packId: number): boolean {
    try {
        const last = Number(window.localStorage.getItem(`dylaris.modrinthSync.${packId}`) || 0);
        if (Date.now() - last < EVERY_MS) return false;
        window.localStorage.setItem(`dylaris.modrinthSync.${packId}`, String(Date.now()));
    } catch { /* storage unavailable: check anyway */ }
    return true;
}

export function useModrinthAutoSync(pack: Pack | null, enabled: boolean, onImported: () => void): AutoSyncState {
    const [state, setState] = useState<AutoSyncState>({ phase: 'idle' });
    const packId = pack?.id ?? 0;
    const bound = !!pack?.modrinthProjectId;
    useEffect(() => {
        setState({ phase: 'idle' });
        if (!enabled || !bound || !packId || !due(packId)) return;
        let stop = false;
        (async () => {
            const res = await listModrinthPackVersions(packId);
            if (stop || !res.success || !res.versions) return; // quiet: the manual sync reports errors
            const fresh = res.versions.filter(v => v.new && !v.buildId).sort((a, b) => a.datePublished.localeCompare(b.datePublished)).slice(0, PER_VISIT);
            if (!fresh.length) return;
            let imported = 0;
            for (const v of fresh) {
                if (stop) return;
                setState({ phase: 'running', done: imported, total: fresh.length });
                const r = await importModrinthPackVersion(packId, v.id);
                if (!r.success) { if (!stop) setState({ phase: 'failed', message: `${v.versionNumber}: ${r.message || 'import failed'}` }); break; }
                imported++;
            }
            if (stop) return;
            if (imported) { onImported(); setState(prev => prev.phase === 'failed' ? prev : { phase: 'done', imported }); }
        })();
        return () => { stop = true; };
    // onImported is a refresh callback; a new identity must not restart the sync.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [packId, bound, enabled]);
    return state;
}
