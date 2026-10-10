"use client";

import React, { useEffect, useState } from 'react';
import { AlertTriangle, Copy, Loader2, Search, ShieldCheck, Trash2 } from 'lucide-react';
import {
    scanStorageOrphans, deleteStorageOrphans, getStorageLifecycle, applyStorageLifecycle,
    type OrphanScan, type OrphanCandidate, type LifecycleRule,
} from '@/lib/api/storageMaintenance';
import { formatBytes } from '@/lib/api/usage';
import {
    selectionTotals, toggleAll, orphanAge, truncateKey, summarizeDelete,
    describeLifecycleRule, lifecycleUpToDate, dropDeleted,
} from '@/lib/storageOrphans';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';

// Core refuses a larger request (services.MaxOrphanDeleteKeys).
const MAX_DELETE = 1000;

const KIND_LABEL: Record<string, string> = {
    'server-backup': 'Server backup',
    'platform-backup': 'Platform backup',
    'migration-transfer': 'Migration transfer',
    probe: 'Connection test',
};

function ExistsBadge({ label, exists }: { label: string; exists?: boolean }) {
    if (exists === undefined) return null;
    return (
        <span className={`badge ${exists ? 'badge-neutral' : 'badge-warning'}`} title={exists ? `The ${label} still exists` : `The ${label} no longer exists`}>
            {label} {exists ? 'exists' : 'gone'}
        </span>
    );
}

function OrphanTable({ rows, selected, onSelect, onCopy }: {
    rows: OrphanCandidate[];
    selected: Set<string>;
    onSelect: (next: Set<string>) => void;
    onCopy: (key: string) => void;
}) {
    const allSelected = rows.length > 0 && selectionTotals(rows, selected).count === rows.length;
    return (
        <div className="table-wrapper max-h-[420px] overflow-y-auto">
            <table className="w-full">
                <thead>
                    <tr>
                        <th className="table-th w-8">
                            <input
                                type="checkbox"
                                className="checkbox"
                                aria-label="Select all"
                                checked={allSelected}
                                onChange={() => onSelect(toggleAll(rows, selected))}
                            />
                        </th>
                        <th className="table-th text-left">File</th>
                        <th className="table-th text-left">Kind</th>
                        <th className="table-th text-right">Size</th>
                        <th className="table-th text-right">Age</th>
                    </tr>
                </thead>
                <tbody>
                    {rows.map(c => (
                        <tr key={c.key} className="table-tr table-tr-hover">
                            <td className="table-td">
                                <input
                                    type="checkbox"
                                    className="checkbox"
                                    aria-label={`Select ${c.key}`}
                                    checked={selected.has(c.key)}
                                    onChange={() => {
                                        const next = new Set(selected);
                                        if (next.has(c.key)) next.delete(c.key); else next.add(c.key);
                                        onSelect(next);
                                    }}
                                />
                            </td>
                            <td className="table-td">
                                <div className="flex items-center gap-1.5 min-w-0">
                                    <span className="font-mono text-xs text-(--base-08) truncate" title={c.key}>{truncateKey(c.key)}</span>
                                    <button onClick={() => onCopy(c.key)} className="btn btn-ghost btn-icon btn-sm shrink-0" aria-label="Copy key" title="Copy key">
                                        <Copy size={12} />
                                    </button>
                                </div>
                            </td>
                            <td className="table-td">
                                <div className="flex flex-wrap items-center gap-1">
                                    <span className="text-xs text-(--base-07)">{KIND_LABEL[c.kind] ?? c.kind}</span>
                                    <ExistsBadge label="server" exists={c.serverExists} />
                                    <ExistsBadge label="job" exists={c.jobExists} />
                                </div>
                            </td>
                            <td className="table-td text-right text-xs tabular-nums">{formatBytes(c.size)}</td>
                            <td className="table-td text-right text-xs tabular-nums" title={c.lastModified}>{orphanAge(c.lastModified)}</td>
                        </tr>
                    ))}
                </tbody>
            </table>
        </div>
    );
}

/**
 * Orphaned files and bucket lifecycle rules for ONE platform storage. Nothing
 * here runs on its own: the scan is a dry run, and deletes and rule changes
 * happen only on an explicit, confirmed click.
 */
export default function StorageMaintenance({ storageId }: { storageId: number }) {
    const [rules, setRules] = useState<LifecycleRule[] | null>(null);
    const [planned, setPlanned] = useState<LifecycleRule[]>([]);
    const [rulesError, setRulesError] = useState('');
    const [applying, setApplying] = useState(false);

    const [scan, setScan] = useState<OrphanScan | null>(null);
    const [scanError, setScanError] = useState('');
    const [scanning, setScanning] = useState(false);
    const [selected, setSelected] = useState<Set<string>>(new Set());
    // Files whose server and job still exist have their own selection and an
    // explicit acknowledgement: they may be real backups this database lost.
    const [liveSelected, setLiveSelected] = useState<Set<string>>(new Set());
    const [liveAck, setLiveAck] = useState(false);
    const [deleting, setDeleting] = useState(false);

    const loadRules = async () => {
        setRulesError('');
        const res = await getStorageLifecycle(storageId);
        if (res.success) {
            setRules(res.rules ?? []);
            setPlanned(res.planned ?? []);
        } else {
            setRules(null);
            setRulesError(res.message || 'Could not read the lifecycle rules.');
        }
    };

    useEffect(() => { loadRules(); }, [storageId]); // eslint-disable-line react-hooks/exhaustive-deps

    const handleApply = async () => {
        if (!(await confirmDialog({
            title: 'Apply lifecycle rules',
            message: 'Add the DYLARIS rules to this storage: unfinished uploads are aborted after 3 days, and on the storage that receives migration transfers those expire a day after their download links (at least 2 days). Rules are rewritten as a whole; rules managed elsewhere are kept but unusual options on them may be lost.',
            confirmLabel: 'Apply',
            destructive: false,
        }))) return;
        setApplying(true);
        try {
            const res = await applyStorageLifecycle(storageId);
            if (res.success) {
                setRules(res.rules ?? []);
                toast('Lifecycle rules applied.');
            } else {
                toast(res.message || 'Applying the lifecycle rules failed.', false);
            }
        } finally {
            setApplying(false);
        }
    };

    const handleScan = async () => {
        setScanning(true);
        setScanError('');
        try {
            const res = await scanStorageOrphans(storageId);
            if (res.success && res.scan) {
                setScan({ ...res.scan, live: res.scan.live ?? [] });
                setSelected(new Set());
                setLiveSelected(new Set());
                setLiveAck(false);
            } else {
                setScanError(res.message || 'The scan failed.');
            }
        } finally {
            setScanning(false);
        }
    };

    const candidates = scan?.candidates ?? [];
    const live = scan?.live ?? [];
    const totals = selectionTotals(candidates, selected);
    const liveTotals = selectionTotals(live, liveSelected);

    const runDelete = async (keys: string[], includeLive: boolean, bytes: number) => {
        if (keys.length === 0) return;
        if (!(await confirmDialog({
            title: includeLive ? 'Delete files that may be real backups' : 'Delete orphaned files',
            message: includeLive
                ? `Delete ${keys.length} file${keys.length === 1 ? '' : 's'} (${formatBytes(bytes)}) whose server and backup job still exist? If this database lost their rows, these are the only copies. This cannot be undone.`
                : `Delete ${keys.length} file${keys.length === 1 ? '' : 's'} (${formatBytes(bytes)}) from this storage? Each file is checked again first and kept if a backup refers to it by now. This cannot be undone.`,
        }))) return;
        setDeleting(true);
        try {
            const res = await deleteStorageOrphans(storageId, keys, includeLive);
            const sum = summarizeDelete(res.results ?? []);
            if (res.results) {
                setScan(s => s && dropDeleted(s, res.results ?? []));
                if (includeLive) setLiveSelected(new Set()); else setSelected(new Set());
            }
            if (!res.success) {
                toast(res.message || 'Delete failed.', false);
            } else if (sum.refused > 0) {
                toast(`Deleted ${sum.deleted} file(s), ${formatBytes(sum.bytes)}. ${sum.refused} kept: ${sum.firstReason ?? 'see the scan'}.`, false);
            } else {
                toast(`Deleted ${sum.deleted} file(s), ${formatBytes(sum.bytes)}.`);
            }
        } finally {
            setDeleting(false);
        }
    };

    const copyKey = (key: string) => {
        navigator.clipboard.writeText(key).then(() => toast('Key copied.'), () => toast('Copy failed.', false));
    };

    return (
        <div className="bg-(--base-02) border border-(--base-04) rounded-md p-3 space-y-4">
            <section className="space-y-2">
                <div className="flex items-center justify-between gap-3">
                    <h4 className="text-sm font-medium text-(--base-09)">Lifecycle rules</h4>
                    <button
                        onClick={handleApply}
                        className="btn btn-secondary btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                        disabled={applying || rules === null || lifecycleUpToDate(rules, planned)}
                        title={rules !== null && lifecycleUpToDate(rules, planned) ? 'The DYLARIS rules are already in place' : undefined}
                    >
                        {applying ? <Loader2 size={12} className="animate-spin" /> : <ShieldCheck size={12} />}
                        Apply
                    </button>
                </div>
                {rulesError && <p className="alert alert-warning text-xs">{rulesError}</p>}
                {rules !== null && (rules.length === 0 ? (
                    <p className="text-xs text-(--base-06)">This bucket has no lifecycle rules.</p>
                ) : (
                    <ul className="space-y-1">
                        {rules.map(r => (
                            <li key={r.id} className="flex items-center gap-2 text-xs text-(--base-07)">
                                <span className={`badge ${r.ours ? 'badge-accent' : 'badge-neutral'}`}>{r.id || 'unnamed'}</span>
                                <span>{describeLifecycleRule(r)}</span>
                                {r.status !== 'Enabled' && <span className="badge badge-warning">{r.status.toLowerCase()}</span>}
                            </li>
                        ))}
                    </ul>
                ))}
            </section>

            <section className="space-y-2">
                <div className="flex items-center justify-between gap-3">
                    <div>
                        <h4 className="text-sm font-medium text-(--base-09)">Orphaned files</h4>
                        <p className="text-xs text-(--base-06)">Backup files no backup refers to any more, older than 24 hours. The scan deletes nothing.</p>
                    </div>
                    <button onClick={handleScan} className="btn btn-secondary btn-sm disabled:opacity-40 disabled:cursor-not-allowed" disabled={scanning || deleting}>
                        {scanning ? <Loader2 size={12} className="animate-spin" /> : <Search size={12} />}
                        {scanning ? 'Scanning...' : 'Scan'}
                    </button>
                </div>
                {scanError && <p className="alert alert-error text-xs">{scanError}</p>}
                {scan && (
                    <>
                        <p className="text-xs text-(--base-06)">
                            {candidates.length} orphaned ({formatBytes(scan.candidateBytes)}).
                            {' '}Left alone: {scan.referencedCount} in use ({formatBytes(scan.referencedBytes)}),
                            {' '}{scan.recentCount} newer than 24 h ({formatBytes(scan.recentBytes)}),
                            {' '}{scan.unclassifiedCount} other files ({formatBytes(scan.unclassifiedBytes)}).
                        </p>
                        {candidates.length === 0 ? (
                            <p className="alert alert-success text-xs">No orphaned files.</p>
                        ) : (
                            <>
                                <OrphanTable rows={candidates} selected={selected} onSelect={setSelected} onCopy={copyKey} />
                                <div className="flex items-center justify-between gap-3">
                                    <span className="text-xs text-(--base-06)">
                                        {totals.count} selected ({formatBytes(totals.bytes)})
                                        {totals.count > MAX_DELETE && ` - at most ${MAX_DELETE} per delete`}
                                    </span>
                                    <button
                                        onClick={() => runDelete([...selected], false, totals.bytes)}
                                        className="btn btn-danger btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                                        disabled={totals.count === 0 || totals.count > MAX_DELETE || deleting}
                                    >
                                        {deleting ? <Loader2 size={12} className="animate-spin" /> : <Trash2 size={12} />}
                                        Delete selected
                                    </button>
                                </div>
                            </>
                        )}

                        {live.length > 0 && (
                            <div className="space-y-2 pt-2">
                                <h5 className="text-sm font-medium text-(--base-09)">
                                    Unreferenced, but server and job still exist ({live.length}, {formatBytes(scan.liveBytes)})
                                </h5>
                                <div className="alert alert-warning text-xs flex items-start gap-2">
                                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                                    <span>These belong to a server and backup job that still exist. They may be real backups this database lost, e.g. after a database restore. Delete only if you are sure.</span>
                                </div>
                                <OrphanTable rows={live} selected={liveSelected} onSelect={setLiveSelected} onCopy={copyKey} />
                                <label className="checkbox-row flex items-center gap-2 text-xs text-(--base-07)">
                                    <input type="checkbox" className="checkbox" checked={liveAck} onChange={e => setLiveAck(e.target.checked)} />
                                    I checked these are not backups this database still needs
                                </label>
                                <div className="flex items-center justify-between gap-3">
                                    <span className="text-xs text-(--base-06)">
                                        {liveTotals.count} selected ({formatBytes(liveTotals.bytes)})
                                        {liveTotals.count > MAX_DELETE && ` - at most ${MAX_DELETE} per delete`}
                                    </span>
                                    <button
                                        onClick={() => runDelete([...liveSelected], true, liveTotals.bytes)}
                                        className="btn btn-danger btn-sm disabled:opacity-40 disabled:cursor-not-allowed"
                                        disabled={!liveAck || liveTotals.count === 0 || liveTotals.count > MAX_DELETE || deleting}
                                    >
                                        {deleting ? <Loader2 size={12} className="animate-spin" /> : <Trash2 size={12} />}
                                        Delete selected anyway
                                    </button>
                                </div>
                            </div>
                        )}

                        {(scan.unclassified?.length ?? 0) > 0 && (
                            <details className="text-xs">
                                <summary className="cursor-pointer text-(--base-06) mono-label">
                                    Other files, never deleted here ({scan.unclassifiedCount}
                                    {scan.unclassifiedCount > (scan.unclassified?.length ?? 0) && `, first ${scan.unclassified?.length} shown`})
                                </summary>
                                <ul className="mt-2 max-h-64 overflow-auto space-y-1">
                                    {scan.unclassified?.map(o => (
                                        <li key={o.key} className="flex items-center gap-3 text-(--base-07)">
                                            <span className="font-mono flex-1 min-w-0 truncate" title={o.key}>{truncateKey(o.key)}</span>
                                            <span className="shrink-0 text-(--base-06)">{formatBytes(o.size)}</span>
                                            <span className="shrink-0 w-12 text-right text-(--base-06)">{o.lastModified ? orphanAge(o.lastModified) : '-'}</span>
                                        </li>
                                    ))}
                                </ul>
                            </details>
                        )}
                    </>
                )}
            </section>
        </div>
    );
}
