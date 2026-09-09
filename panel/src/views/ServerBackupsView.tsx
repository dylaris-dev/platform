"use client";

import React, { useState, useEffect, useCallback } from 'react';

import { Plus, Play, Trash2, Pencil, X, Download, Clock, HardDrive, CircleAlert, Save, Undo2, AlertTriangle } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { restoresNeedPolling } from '@/lib/backupPolling';
import {
    BackupJob, BackupRun, BackupStorage, BackupRestore,
    listBackupJobs, createBackupJob, updateBackupJob, deleteBackupJob, triggerBackupJob,
    listBackupRuns, deleteBackupRun, restoreBackupRun, backupDownloadUrl,
    listBackupStorages, listBackupRestores,
} from '@/lib/api';
import Spinner from '@/components/Spinner';
import { Skeleton, SkeletonText } from '@/components/Skeleton';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';

function formatBytes(b: number): string {
    if (!b) return '—';
    if (b >= 1024 ** 3) return `${(b / 1024 ** 3).toFixed(2)} GB`;
    if (b >= 1024 ** 2) return `${(b / 1024 ** 2).toFixed(1)} MB`;
    if (b >= 1024) return `${(b / 1024).toFixed(0)} KB`;
    return `${b} B`;
}

function formatRel(iso: string | null | undefined): string {
    if (!iso) return '—';
    const diff = Date.now() - new Date(iso).getTime();
    const m = Math.floor(diff / 60000);
    if (m < 1) return 'just now';
    if (m < 60) return `${m}m ago`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h}h ago`;
    return `${Math.floor(h / 24)}d ago`;
}

const SCHEDULE_OPTIONS = [
    { value: 'manual', label: 'Manual only' },
    { value: 'every 6h', label: 'Every 6 hours' },
    { value: 'every 12h', label: 'Every 12 hours' },
    { value: 'every 1d', label: 'Daily' },
    { value: 'every 7d', label: 'Weekly' },
];

interface JobFormProps {
    initial: Partial<BackupJob>;
    storages: BackupStorage[];
    subServers: string[];
    onClose: () => void;
    onSave: (job: Partial<BackupJob>) => Promise<void>;
}

function JobForm({ initial, storages, subServers, onClose, onSave }: JobFormProps) {
    const [job, setJob] = useState<Partial<BackupJob>>(initial);
    const [saving, setSaving] = useState(false);
    const isSelective = (job.includePatterns?.length || 0) + (job.excludePatterns?.length || 0) > 0;
    const [mode, setMode] = useState<'full-container' | 'sub-server' | 'selective'>(
        job.subServer ? (isSelective ? 'selective' : 'sub-server') : 'full-container',
    );

    const updateField = <K extends keyof BackupJob>(k: K, v: BackupJob[K]) => setJob(j => ({ ...j, [k]: v }));

    const submit = async () => {
        setSaving(true);
        const finalJob: Partial<BackupJob> = {
            ...job,
            subServer: mode === 'full-container' ? null : (job.subServer || subServers[0] || ''),
            includePatterns: mode === 'selective' ? (job.includePatterns ?? []) : [],
            excludePatterns: mode === 'selective' ? (job.excludePatterns ?? []) : [],
        };
        await onSave(finalJob);
        setSaving(false);
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <div className="modal-panel w-full max-w-xl" onClick={e => e.stopPropagation()}>
                <div className="modal-header flex items-center justify-between">
                    <h3 className="modal-title">{job.id ? 'Edit Backup Job' : 'New Backup Job'}</h3>
                    <button onClick={onClose} className="p-1 text-(--base-06) hover:text-(--base-09)">
                        <X size={16} />
                    </button>
                </div>
                <div className="modal-body space-y-4">
                    <div className="form-group">
                        <label className="input-label">Job Name</label>
                        <input type="text" value={job.name ?? ''} onChange={e => updateField('name', e.target.value)} className="input-field" placeholder="World snapshot" />
                    </div>

                    <div className="form-group">
                        <label className="input-label">Scope</label>
                        <div className="grid grid-cols-3 gap-2">
                            {([
                                { id: 'full-container', label: 'Full Container', desc: 'All sub-servers' },
                                { id: 'sub-server', label: 'Sub-Server', desc: 'One server instance' },
                                { id: 'selective', label: 'Selective', desc: 'Include/exclude paths' },
                            ] as const).map(opt => (
                                <button
                                    key={opt.id}
                                    type="button"
                                    onClick={() => setMode(opt.id)}
                                    className={`p-3 rounded-md border text-left transition-colors ${mode === opt.id ? 'border-(--accent) bg-(--accent-ghost)' : 'border-(--base-04) bg-(--base-03) hover:border-(--base-05)'}`}
                                >
                                    <div className="text-sm font-medium text-(--base-09)">{opt.label}</div>
                                    <div className="text-xs text-(--base-06) mt-0.5">{opt.desc}</div>
                                </button>
                            ))}
                        </div>
                    </div>

                    {mode !== 'full-container' && (
                        <div className="form-group">
                            <label className="input-label">Sub-Server</label>
                            <select value={job.subServer ?? ''} onChange={e => updateField('subServer', e.target.value)} className="input-field">
                                {subServers.length === 0 && <option value="">(no sub-servers found)</option>}
                                {subServers.map(s => <option key={s} value={s}>{s}</option>)}
                            </select>
                        </div>
                    )}

                    {mode === 'selective' && (
                        <>
                            <div className="form-group">
                                <label className="input-label">Include Patterns (one per line)</label>
                                <textarea
                                    value={(job.includePatterns ?? []).join('\n')}
                                    onChange={e => updateField('includePatterns', e.target.value.split('\n').map(s => s.trim()).filter(Boolean))}
                                    className="input-field font-mono text-xs h-24"
                                    placeholder={'world/**\nworld_nether/**\nplugins/*.yml'}
                                />
                                <p className="text-xs text-(--base-06)">Empty = include everything not excluded.</p>
                            </div>
                            <div className="form-group">
                                <label className="input-label">Exclude Patterns (one per line)</label>
                                <textarea
                                    value={(job.excludePatterns ?? []).join('\n')}
                                    onChange={e => updateField('excludePatterns', e.target.value.split('\n').map(s => s.trim()).filter(Boolean))}
                                    className="input-field font-mono text-xs h-24"
                                    placeholder={'logs/**\n*.log\ncache/**'}
                                />
                            </div>
                        </>
                    )}

                    <div className="grid grid-cols-2 gap-3">
                        <div className="form-group">
                            <label className="input-label">Schedule</label>
                            <select value={job.schedule ?? 'manual'} onChange={e => updateField('schedule', e.target.value)} className="input-field">
                                {SCHEDULE_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
                            </select>
                        </div>
                        <div className="form-group">
                            <label className="input-label">Keep last N runs</label>
                            <input type="number" min={1} max={100} value={job.retentionCount ?? 3} onChange={e => updateField('retentionCount', Math.max(1, Number(e.target.value)))} className="input-field text-right tabular-nums" />
                        </div>
                    </div>

                    <div className="form-group">
                        <label className="input-label">Storage</label>
                        <select value={job.storageId ?? ''} onChange={e => updateField('storageId', e.target.value ? Number(e.target.value) : null)} className="input-field">
                            <option value="">Default storage</option>
                            {storages.map(s => <option key={s.id} value={s.id}>{s.name} ({s.provider})</option>)}
                        </select>
                    </div>

                    <div className="flex items-center justify-between pt-2 border-t border-(--base-03)">
                        <label className="input-label">Enabled</label>
                        <button
                            type="button"
                            onClick={() => updateField('enabled', !job.enabled)}
                            className={`toggle-track ${(job.enabled !== false) ? 'toggle-track-on' : 'toggle-track-off'}`}
                            role="switch"
                            aria-checked={job.enabled !== false}
                        >
                            <span className={`toggle-knob ${(job.enabled !== false) ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                        </button>
                    </div>
                </div>
                <div className="modal-footer">
                    <button onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button onClick={submit} disabled={saving} className="btn btn-primary disabled:opacity-40">
                        <Save size={13} /> {saving ? 'Saving…' : 'Save Job'}
                    </button>
                </div>
            </div>
        </div>
    );
}

// The dot was already coloured; the word beside it was not, so a failure read
// as neutral grey. badge-success / badge-error / badge-warning all exist in
// globals.css and were simply never used here.
function runBadgeClass(status: string): string {
    if (status === 'success') return 'badge badge-success';
    if (status === 'failed') return 'badge badge-error';
    return 'badge badge-warning';
}

export default function ServerBackupsView() {
    const paramId = useRouteId('servers');
    const { servers } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);

    const [jobs, setJobs] = useState<BackupJob[]>([]);
    const [storages, setStorages] = useState<BackupStorage[]>([]);
    const [runs, setRuns] = useState<Record<number, BackupRun[]>>({});
    const [loading, setLoading] = useState(true);
    const [showForm, setShowForm] = useState(false);
    const [editingJob, setEditingJob] = useState<BackupJob | null>(null);
    const [busyJob, setBusyJob] = useState<number | null>(null);
    const [restoreTarget, setRestoreTarget] = useState<BackupRun | null>(null);
    const [restoreCountdown, setRestoreCountdown] = useState(5);
    const [restoring, setRestoring] = useState(false);
    const [restoreHistory, setRestoreHistory] = useState<BackupRestore[]>([]);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    // Deliberately no setLoading(true) beyond the initial value: this runs every
    // 5 seconds while a backup is going, and raising the flag swapped the job
    // list for skeletons for the length of three round trips each time. That was
    // the flicker. NodesTab polls on the same interval and has never touched a
    // loading flag.
    //
    // Keyed on serverId, not the server OBJECT: AppDataContext replaces the
    // servers array on every servers.changed event, so depending on the object
    // gave reload a new identity for unrelated reasons - which re-ran the mount
    // effect and rebuilt the poll timer.
    const reload = useCallback(async () => {
        if (!serverId) return;
        const [jobsRes, storagesRes, restoresRes] = await Promise.all([
            listBackupJobs(serverId),
            listBackupStorages(),
            listBackupRestores(serverId),
        ]);
        if (jobsRes.success && jobsRes.jobs) setJobs(jobsRes.jobs);
        if (storagesRes.success && storagesRes.storages) setStorages(storagesRes.storages);
        if (restoresRes.success && restoresRes.restores) setRestoreHistory(restoresRes.restores);
        setLoading(false);

        // Load runs per job in parallel.
        const allRuns: Record<number, BackupRun[]> = {};
        await Promise.all((jobsRes.jobs ?? []).map(async j => {
            const r = await listBackupRuns(j.id);
            if (r.success && r.runs) allRuns[j.id] = r.runs;
        }));
        setRuns(allRuns);
    }, [serverId]);

    useEffect(() => { reload(); }, [reload]);

    // Refresh while any run or restore is in progress. The two conditions are
    // computed outside the effect so it depends on BOOLEANS: runs and
    // restoreHistory are replaced wholesale by every reload, so depending on
    // them tore the timer down and rebuilt it on every tick.
    const hasRunningBackup = Object.values(runs).some(list => list.some(r => r.status === 'running'));
    const hasPendingRestore = restoresNeedPolling(restoreHistory);
    useEffect(() => {
        if (!hasRunningBackup && !hasPendingRestore) return;
        const interval = setInterval(reload, 5000);
        return () => clearInterval(interval);
    }, [hasRunningBackup, hasPendingRestore, reload]);

    const subServers = server?.activeSubServer ? [server.activeSubServer] : [];

    const handleCreate = async (job: Partial<BackupJob>) => {
        if (!server) return;
        const res = await createBackupJob(server.id, job);
        if (res.success) {
            showToast('Job created.');
            setShowForm(false);
            reload();
        } else {
            showToast('Failed to create job.', false);
        }
    };
    const handleUpdate = async (job: Partial<BackupJob>) => {
        if (!editingJob) return;
        const res = await updateBackupJob(editingJob.id, job);
        if (res.success) {
            showToast('Job saved.');
            setEditingJob(null);
            reload();
        } else {
            showToast('Failed to save job.', false);
        }
    };
    const handleDeleteJob = async (id: number) => {
        if (!(await confirmDialog({ title: 'Delete backup job', message: 'Delete this job? Existing runs stay in storage but become unmanaged.' }))) return;
        const res = await deleteBackupJob(id);
        if (res.success) reload();
    };
    const handleTrigger = async (id: number) => {
        setBusyJob(id);
        const res = await triggerBackupJob(id);
        if (res.success) showToast('Run started.');
        // Core's reason, not a generic string: the refusals here are ones the
        // operator can act on and cannot guess. Measured with the per-server
        // backup quota - the run was correctly refused and the button looked
        // like it had simply done nothing.
        else showToast(res.message || 'Failed to start run.', false);
        setBusyJob(null);
        reload();
    };
    const handleDeleteRun = async (runId: number) => {
        if (!(await confirmDialog({ title: 'Delete backup', message: 'Delete this backup? The archive will be removed from storage.' }))) return;
        // The trigger handler right above toasts both outcomes; this one dropped
        // the answer, so a delete Core refused (storage unreachable, so the
        // archive is still billed for and still there) looked identical to one
        // that worked.
        const res = await deleteBackupRun(runId);
        if (!res.success) {
            showToast('Failed to delete the backup.', false);
            return;
        }
        showToast('Backup deleted.');
        reload();
    };

    // Restore confirmation: 5s countdown gives a clear chance to bail.
    useEffect(() => {
        if (!restoreTarget) return;
        setRestoreCountdown(5);
        const interval = setInterval(() => {
            setRestoreCountdown(c => Math.max(0, c - 1));
        }, 1000);
        return () => clearInterval(interval);
    }, [restoreTarget]);

    const handleConfirmRestore = async () => {
        if (!restoreTarget) return;
        setRestoring(true);
        const res = await restoreBackupRun(restoreTarget.id);
        setRestoring(false);
        if (res.success) {
            showToast('Restore queued — your server will restart shortly.');
            setRestoreTarget(null);
            reload();
        } else {
            showToast(res.message || 'Restore failed.', false);
        }
    };

    if (!server) return null;

    return (
        <div className="flex flex-col gap-4 h-full">
            <div className="flex items-center gap-3 flex-wrap">
                <h1 className="h-page">Backups</h1>
                <span className="mono-label">{server.activeSubServer || 'no sub-server selected'}</span>
                <button onClick={() => setShowForm(true)} className="btn btn-primary ml-auto">
                    <Plus size={14} /> New Job
                </button>
            </div>

            {storages.length === 0 && (
                <div className="alert alert-warning">
                    <CircleAlert size={14} className="shrink-0 mt-0.5" />
                    <div>
                        <p className="font-medium">No storage configured</p>
                        <p className="text-xs">Admins: head over to Settings → Backups and add at least one storage (Local or S3-compatible) before creating jobs.</p>
                    </div>
                </div>
            )}

            {loading ? (
                <div className="flex-1 overflow-auto space-y-3">
                    {Array.from({ length: 2 }).map((_, i) => (
                        <div key={i} className="card card-pad">
                            <div className="flex items-start justify-between gap-3 flex-wrap">
                                <div className="min-w-0 space-y-2">
                                    <div className="flex items-center gap-2 flex-wrap">
                                        <SkeletonText width="w-32" className="h-4" />
                                        <Skeleton className="w-16 h-4 rounded-sm" />
                                        <Skeleton className="w-20 h-4 rounded-sm" />
                                    </div>
                                    <div className="flex items-center gap-3 mt-1">
                                        <SkeletonText width="w-20" className="h-2.5" />
                                        <SkeletonText width="w-14" className="h-2.5" />
                                        <SkeletonText width="w-24" className="h-2.5" />
                                    </div>
                                </div>
                                <div className="flex items-center gap-1.5">
                                    <Skeleton className="w-20 h-7 rounded-md" />
                                    <Skeleton className="w-7 h-7 rounded-md" />
                                    <Skeleton className="w-7 h-7 rounded-md" />
                                </div>
                            </div>
                            <div className="mt-4 border-t border-(--base-03) pt-3 space-y-2">
                                <SkeletonText width="w-24" className="h-2.5" />
                                {Array.from({ length: 3 }).map((_, j) => (
                                    <div key={j} className="flex items-center gap-3 py-1.5 px-2">
                                        <Skeleton className="w-2 h-2 rounded-full" />
                                        <SkeletonText width="w-24" className="flex-1" />
                                        <SkeletonText width="w-16" />
                                        <Skeleton className="w-16 h-4 rounded-sm" />
                                        <Skeleton className="w-7 h-7 rounded-md" />
                                        <Skeleton className="w-7 h-7 rounded-md" />
                                        <Skeleton className="w-7 h-7 rounded-md" />
                                    </div>
                                ))}
                            </div>
                        </div>
                    ))}
                </div>
            ) : jobs.length === 0 ? (
                <div className="flex-1 flex flex-col items-center justify-center text-(--base-06) gap-2">
                    <HardDrive size={40} className="opacity-40" />
                    <p className="text-sm">No backup jobs configured.</p>
                </div>
            ) : (
                <div className="flex-1 overflow-auto space-y-3">
                    {jobs.map(job => (
                        <div key={job.id} className="card card-pad">
                            <div className="flex items-start justify-between gap-3 flex-wrap">
                                <div className="min-w-0">
                                    <div className="flex items-center gap-2 flex-wrap">
                                        <span className="h-section">{job.name}</span>
                                        {!job.enabled && <span className="badge badge-neutral">disabled</span>}
                                        {job.subServer ? <span className="badge badge-neutral">{job.subServer}</span> : <span className="badge badge-accent">full container</span>}
                                        {(job.includePatterns?.length || job.excludePatterns?.length) ? <span className="badge badge-accent">selective</span> : null}
                                    </div>
                                    <div className="flex items-center gap-3 mt-1 text-xs text-(--base-06)">
                                        <span className="inline-flex items-center gap-1"><Clock size={11} />{job.schedule}</span>
                                        <span>keep {job.retentionCount}</span>
                                        <span>last: {formatRel(job.lastRunAt)}</span>
                                    </div>
                                </div>
                                <div className="flex items-center gap-1.5">
                                    <button onClick={() => handleTrigger(job.id)} disabled={busyJob === job.id} className="btn btn-primary btn-sm disabled:opacity-40">
                                        {busyJob === job.id ? <Spinner size="xs" /> : <Play size={12} />}
                                        Run Now
                                    </button>
                                    <button onClick={() => setEditingJob(job)} className="btn btn-secondary btn-sm">
                                        <Pencil size={12} />
                                    </button>
                                    <button onClick={() => handleDeleteJob(job.id)} className="btn btn-danger btn-sm">
                                        <Trash2 size={12} />
                                    </button>
                                </div>
                            </div>

                            {(runs[job.id] ?? []).length > 0 && (
                                <div className="mt-4 border-t border-(--base-03) pt-3">
                                    <div className="mono-label mb-2">Recent Runs</div>
                                    <div className="space-y-1">
                                        {runs[job.id].slice(0, 5).map(run => (
                                            <div key={run.id} className="rounded hover:bg-(--base-03)/40">
                                            <div className="flex items-center gap-3 py-1.5 px-2">
                                                <span className={`badge-dot ${run.status === 'success' ? 'bg-(--success-light)' : run.status === 'failed' ? 'bg-(--error)' : 'bg-(--warning) animate-pulse'}`} />
                                                <span className="text-sm text-(--base-08) flex-1">{formatRel(run.startedAt)}</span>
                                                <span className="text-xs text-(--base-06) tabular-nums w-20 text-right" title={run.status === 'running' ? 'Archived so far' : 'Archive size'}>{formatBytes(run.sizeBytes)}</span>
                                                <span className={runBadgeClass(run.status) + ' capitalize'}>{run.status}</span>
                                                {run.status === 'success' && (
                                                    <>
                                                        <button
                                                            onClick={() => setRestoreTarget(run)}
                                                            className="btn btn-secondary btn-sm"
                                                            title="Restore this backup"
                                                        >
                                                            <Undo2 size={11} />
                                                        </button>
                                                        <a href={backupDownloadUrl(run.id)} className="btn btn-secondary btn-sm" download title="Download archive">
                                                            <Download size={11} />
                                                        </a>
                                                    </>
                                                )}
                                                <button onClick={() => handleDeleteRun(run.id)} className="btn btn-danger btn-sm" title="Delete">
                                                    <Trash2 size={11} />
                                                </button>
                                            </div>
                                            {/* Its own line, full width. It used to sit AFTER the delete
                                                button, truncated with the text only in a tooltip - so the
                                                one thing a failed backup has to tell you was the least
                                                readable thing in the row. */}
                                            {run.status === 'failed' && run.errorMessage && (
                                                <p className="px-2 pb-2 -mt-0.5 text-xs text-(--error-light) break-words">
                                                    {run.errorMessage}
                                                </p>
                                            )}
                                            </div>
                                        ))}
                                    </div>
                                </div>
                            )}
                        </div>
                    ))}
                </div>
            )}

            {restoreHistory.length > 0 && (
                <div className="card card-pad">
                    <div className="flex items-center gap-2 mb-3">
                        <Undo2 size={14} className="text-(--accent-light)" />
                        <span className="h-section">Restore History</span>
                        <span className="mono-label">{restoreHistory.length}</span>
                    </div>
                    <div className="space-y-1">
                        {restoreHistory.slice(0, 8).map(restore => (
                            <div key={restore.id} className="flex items-center gap-3 py-1.5 px-2 rounded hover:bg-(--base-03)/40">
                                {/* No pulse on a stalled one: the animation is the
                                    claim that something is happening, and for
                                    these nothing is until the node returns. */}
                                <span className={`badge-dot ${restore.status === 'success' ? 'bg-(--success-light)' : restore.status === 'failed' ? 'bg-(--error)' : restore.stalled ? 'bg-(--warning)' : 'bg-(--warning) animate-pulse'}`} />
                                <span className="text-sm text-(--base-08) flex-1">
                                    Run #{restore.runId}
                                    <span className="text-xs text-(--base-06) ml-2">{formatRel(restore.requestedAt)}</span>
                                </span>
                                <span className="badge badge-neutral capitalize">
                                    {restore.stalled ? 'paused' : restore.status}
                                </span>
                                {restore.stalled && (
                                    <span className="text-[10px] text-(--warning-light) max-w-xs truncate" title={restore.stallReason}>
                                        {restore.stallReason}
                                    </span>
                                )}
                                {restore.errorMessage && (
                                    <span className="text-[10px] text-(--error-light) max-w-xs truncate" title={restore.errorMessage}>
                                        {restore.errorMessage}
                                    </span>
                                )}
                            </div>
                        ))}
                    </div>
                </div>
            )}

            {showForm && (
                <JobForm
                    initial={{
                        name: 'Backup',
                        schedule: 'manual',
                        retentionCount: 3,
                        enabled: true,
                        includePatterns: [],
                        excludePatterns: [],
                        subServer: subServers[0] ?? '',
                    }}
                    storages={storages}
                    subServers={subServers}
                    onClose={() => setShowForm(false)}
                    onSave={handleCreate}
                />
            )}
            {editingJob && (
                <JobForm
                    initial={editingJob}
                    storages={storages}
                    subServers={subServers}
                    onClose={() => setEditingJob(null)}
                    onSave={handleUpdate}
                />
            )}

            {restoreTarget && (
                <div className="modal-overlay animate-fade-in" onClick={() => !restoring && setRestoreTarget(null)}>
                    <div className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--warning-light)">
                                <AlertTriangle size={18} /> Restore Backup
                            </h3>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-08)">
                                Replace the current world data with the backup from {' '}
                                <span className="font-mono text-(--base-09)">{new Date(restoreTarget.startedAt).toLocaleString()}</span>?
                            </p>
                            <p className="alert alert-warning text-xs">
                                <AlertTriangle size={13} className="shrink-0 mt-0.5" />
                                <span>
                                    Your server will be stopped, the archive extracted into the sub-server directory, and the server restarted automatically. The previous contents are moved aside (kept until next disk cleanup) — but anything written since this backup will be gone.
                                </span>
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setRestoreTarget(null)} disabled={restoring} className="btn btn-secondary">
                                Cancel
                            </button>
                            <button
                                onClick={handleConfirmRestore}
                                disabled={restoreCountdown > 0 || restoring}
                                className="btn btn-danger disabled:opacity-40"
                            >
                                {restoring
                                    ? 'Queueing…'
                                    : restoreCountdown > 0
                                        ? `Restore (${restoreCountdown}s)`
                                        : <><Undo2 size={13} /> Restore now</>}
                            </button>
                        </div>
                    </div>
                </div>
            )}

        </div>
    );
}
