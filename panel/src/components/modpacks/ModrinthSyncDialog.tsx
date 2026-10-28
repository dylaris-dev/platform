'use client';

import { useEffect, useRef, useState } from 'react';
import { CheckCircle2, AlertTriangle, RefreshCw } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';
import { listModrinthPackVersions, importModrinthPackVersion, type ModrinthImportVersion } from '@/lib/api/packs';

const MANY = 20;
const NEWEST = 10;

type Row = { version: ModrinthImportVersion; state: 'waiting' | 'running' | 'done' | 'failed'; note?: string };

/**
 * Imports every Modrinth version of a pack that it does not have yet, one at a
 * time and oldest first, so builds appear in release order and one bad version
 * does not stop the rest. Used right after importing a project and by
 * "Sync from Modrinth".
 */
export default function ModrinthSyncDialog({ packId, title, onClose }: { packId: number; title: string; onClose: (imported: number) => void }) {
    const [rows, setRows] = useState<Row[] | null>(null);
    const [error, setError] = useState('');
    const [running, setRunning] = useState(true);
    const [warnings, setWarnings] = useState<string[]>([]);
    // Large projects (hundreds of versions) ask first instead of importing all.
    const [choice, setChoice] = useState<{ all: ModrinthImportVersion[]; pick: (list: ModrinthImportVersion[]) => void } | null>(null);
    const cancelled = useRef(false);
    const imported = useRef(0);

    useEffect(() => {
        // Per run, not shared: React may run this effect twice, and the first
        // run must stop rather than import the same versions in parallel.
        let stop = false;
        (async () => {
            const res = await listModrinthPackVersions(packId);
            if (stop) return;
            if (!res.success || !res.versions) { setError(res.message || 'Could not read the Modrinth versions.'); setRunning(false); return; }
            let missing = res.versions.filter(v => !v.buildId).sort((a, b) => a.datePublished.localeCompare(b.datePublished));
            if (missing.length > MANY) {
                const all = missing;
                missing = await new Promise<ModrinthImportVersion[]>(pick => setChoice({ all, pick }));
                setChoice(null);
                if (stop) return;
            }
            const list: Row[] = missing.map(version => ({ version, state: 'waiting' }));
            setRows(list);
            for (let i = 0; i < list.length && !stop && !cancelled.current; i++) {
                setRows(prev => prev && prev.map((row, j) => j === i ? { ...row, state: 'running' } : row));
                const result = await importModrinthPackVersion(packId, list[i].version.id);
                if (result.success) imported.current++;
                if (result.warnings?.length) setWarnings(prev => [...prev, ...result.warnings!.map(w => `${list[i].version.versionNumber}: ${w}`)]);
                setRows(prev => prev && prev.map((row, j) => j === i ? { ...row, state: result.success ? 'done' : 'failed', note: result.success ? `${result.linked ?? 0} from Modrinth, ${result.uploaded ?? 0} files` : result.message } : row));
            }
            if (!stop) setRunning(false);
        })();
        return () => { stop = true; };
    }, [packId]);

    const close = () => { cancelled.current = true; onClose(imported.current); };
    const done = rows?.filter(r => r.state === 'done').length ?? 0;
    return <div className="modal-overlay" onClick={() => { if (!running) close(); }}>
        <ModalPanel className="modal-panel max-w-xl" onClose={() => { if (!running) close(); }} onClick={event => event.stopPropagation()}>
            <div className="modal-header"><h2 className="modal-title">Import from Modrinth</h2><p className="text-xs text-(--base-06) mt-2">{title}</p></div>
            <div className="modal-body space-y-3">
                {error && <p role="alert" className="text-sm text-(--error-light)">{error}</p>}
                {!rows && !error && !choice && <p role="status" className="text-sm text-(--base-07) flex items-center gap-2"><RefreshCw size={13} className="animate-spin" />Reading the project&apos;s versions...</p>}
                {choice && <div className="space-y-3"><p className="text-sm text-(--base-07)">This project has {choice.all.length} versions this pack does not have yet. Importing all of them takes a while; you can start with the newest and sync the rest later.</p><div className="flex gap-2"><button className="btn btn-primary btn-sm" onClick={() => choice.pick(choice.all.slice(-NEWEST))}>Newest {NEWEST}</button><button className="btn btn-secondary btn-sm" onClick={() => choice.pick(choice.all)}>All {choice.all.length}</button></div></div>}
                {rows && rows.length === 0 && <p className="text-sm text-(--base-07) flex items-center gap-2"><CheckCircle2 size={14} className="text-(--success-light)" />Every Modrinth version is already here.</p>}
                {rows && rows.length > 0 && <>
                    <p className="text-sm text-(--base-07)">{running ? `Importing ${Math.min(done + 1, rows.length)} of ${rows.length}. Each version becomes a read-only build.` : `Imported ${done} of ${rows.length} versions.`}</p>
                    <ul className="max-h-72 overflow-y-auto space-y-1.5 text-xs">{rows.map(row => <li key={row.version.id} className="flex items-start gap-2">
                        <span className="w-4 shrink-0 mt-0.5">{row.state === 'running' ? <RefreshCw size={12} className="animate-spin" /> : row.state === 'done' ? <CheckCircle2 size={12} className="text-(--success-light)" /> : row.state === 'failed' ? <AlertTriangle size={12} className="text-(--error-light)" /> : null}</span>
                        <span className="font-mono text-(--base-09)">{row.version.versionNumber}</span>
                        <span className="text-(--base-06)">MC {row.version.gameVersions.join(', ')} · {row.version.loaders.join(', ')}</span>
                        {row.note && <span className={row.state === 'failed' ? 'text-(--error-light)' : 'text-(--base-06)'}>· {row.note}</span>}
                    </li>)}</ul>
                </>}
                {warnings.length > 0 && <details className="text-xs text-(--base-07)"><summary className="cursor-pointer text-(--warning-light)">{warnings.length} file(s) need a look</summary><ul className="mt-2 space-y-1 max-h-40 overflow-y-auto">{warnings.map((w, i) => <li key={i}>{w}</li>)}</ul></details>}
            </div>
            <div className="modal-footer"><button className="btn btn-primary" disabled={running} onClick={close}>{running ? 'Importing...' : 'Done'}</button></div>
        </ModalPanel>
    </div>;
}
