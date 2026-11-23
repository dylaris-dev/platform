"use client";

import React, { useState, useEffect, useCallback } from 'react';

import { Plus, Play, Trash2, Pencil, X, Download, Clock, HardDrive, CircleAlert, Save, Undo2, AlertTriangle, MoreHorizontal } from 'lucide-react';
import { Meter, MeterIndicator, MeterTrack } from '@ui-sdk/react/meter';
import '@/styles/server-backups.css';
import { useAppData } from '@/lib/AppDataContext';
import { restoresNeedPolling } from '@/lib/backupPolling';
import {
    BackupJob, BackupRun, BackupStorage, BackupRestore, BackupConfig, BackupUsage, getBackupConfig, getBackupUsage,
    listBackupJobs, createBackupJob, updateBackupJob, deleteBackupJob, triggerBackupJob,
    listBackupRuns, deleteBackupRun, restoreBackupRun, backupDownloadUrl,
    listBackupStorages, listBackupRestores,
} from '@/lib/api';
import Spinner from '@/components/Spinner';
import { Skeleton, SkeletonText } from '@/components/Skeleton';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';
import ModalPanel from '@/components/ui/ModalPanel';
import Select from '@/components/ui/Select';
import Switch from '@/components/ui/Switch';
import Menu from '@/components/ui/Menu';
import NumberField from '@/components/ui/NumberField';
import Callout from '@/components/server/Callout';
import { formatBytes } from '@/lib/format';
import { currentSkip, relativeTime } from '@/lib/backupJobStatus';
import { fmtDateTime } from '@/lib/time';
import { listOwnBackupStorages, jobStorageOptions, type OwnBackupStorage } from '@/lib/api/ownBackupStorage';
import BackupScheduleEditor from '@/components/BackupScheduleEditor';
import { describeSchedule, editorZone, parseSchedule, usesTimeZone } from '@/lib/backupSchedule';

// A run that archived nothing yet shows a dash rather than "0 B".
function runSize(b: number): string {
    return b ? formatBytes(b) : '—';
}

function localTime(iso: string): string {
    return fmtDateTime(iso);
}

// Runs shown before "Show all": enough for the usual retention of a few.
const RUNS_COLLAPSED = 5;

interface JobFormProps {
    initial: Partial<BackupJob>;
    storages: { id: number; label: string }[];
    subServers: string[];
    onClose: () => void;
    onSave: (job: Partial<BackupJob>) => Promise<void>;
}

function JobForm({ initial, storages, subServers, onClose, onSave }: JobFormProps) {
    const [job, setJob] = useState<Partial<BackupJob>>(initial);
    const [saving, setSaving] = useState(false);
    const [zone, setZone] = useState(() => editorZone(initial.schedule ?? 'manual', initial.timezone));
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
            // Omitted for a relative schedule: it never reads the zone, and a
            // changed zone re-arms the job, which would push "every 6h" back.
            timezone: usesTimeZone(parseSchedule(job.schedule ?? 'manual').kind) ? zone : undefined,
        };
        await onSave(finalJob);
        setSaving(false);
    };

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel w-full max-w-xl" onClick={e => e.stopPropagation()}>
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
                            <Select
                                ariaLabel="Sub-Server"
                                value={job.subServer ?? ''}
                                onChange={v => updateField('subServer', v)}
                                placeholder={subServers.length === 0 ? '(no sub-servers found)' : undefined}
                                options={subServers.map(s => ({ value: s, label: s }))}
                            />
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

                    <div className="srv-backups-card">
                        <BackupScheduleEditor
                            idPrefix="job-schedule"
                            schedule={job.schedule ?? 'manual'}
                            timezone={zone}
                            onChange={(schedule, tz) => { updateField('schedule', schedule); setZone(tz); }}
                        />
                    </div>

                    <NumberField
                        label="Keep last N runs"
                        min={1}
                        max={100}
                        value={job.retentionCount ?? 3}
                        onChange={v => updateField('retentionCount', Math.max(1, v))}
                        className="max-w-32"
                    />

                    <div className="form-group">
                        <label className="input-label">Storage</label>
                        <Select
                            ariaLabel="Storage"
                            value={job.storageId == null ? '' : String(job.storageId)}
                            onChange={v => updateField('storageId', v ? Number(v) : null)}
                            options={[{ value: '', label: 'Default storage' }, ...storages.map(s => ({ value: String(s.id), label: s.label }))]}
                        />
                    </div>

                    <div className="flex items-center justify-between pt-2 border-t border-(--base-03)">
                        <span className="input-label">Enabled</span>
                        <Switch checked={job.enabled !== false} onChange={v => updateField('enabled', v)} ariaLabel="Enabled" />
                    </div>
                </div>
                <div className="modal-footer">
                    <button onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button onClick={submit} disabled={saving} className="btn btn-primary disabled:opacity-40">
                        <Save size={14} /> {saving ? 'Saving…' : 'Save Job'}
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}

function runTone(status: string): 'success' | 'error' | 'warning' {
    if (status === 'success') return 'success';
    if (status === 'failed') return 'error';
    return 'warning';
}

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

function StatusPill({ tone, children, pulse }: { tone: 'success' | 'error' | 'warning' | 'accent' | 'neutral'; children: React.ReactNode; pulse?: boolean }) {
    return (
        <span className="srv-backups-pill" data-tone={tone} data-pulse={pulse ? 'true' : undefined}>
            <i aria-hidden="true" />{children}
        </span>
    );
}

function Tile({ label, value, sub, meter }: { label: string; value: React.ReactNode; sub?: React.ReactNode; meter?: React.ReactNode }) {
    return (
        <div className="srv-surface flex min-w-0 flex-col gap-3 p-5">
            <span className="srv-label">{label}</span>
            <span className="srv-num truncate text-[28px] font-medium leading-none text-(--base-09)">{value}</span>
            {meter}
            <div className="min-h-4 truncate text-xs text-(--base-06)">{sub}</div>
        </div>
    );
}

const MENU_BTN = 'btn btn-ghost btn-icon btn-sm';

export default function ServerBackupsView() {
    const paramId = useRouteId('servers');
    const { servers, user, featureFlags } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);
    // Own targets are offered only on the caller's own server: Core refuses a
    // job on someone else's server that names the caller's bucket.
    const isOwner = !!server && !!user && server.ownerId === user.id;

    const [jobs, setJobs] = useState<BackupJob[]>([]);
    const [storages, setStorages] = useState<BackupStorage[]>([]);
    const [ownStorages, setOwnStorages] = useState<OwnBackupStorage[]>([]);
    // An older Core omits ownStorage; own storage was always open then.
    const ownAllowed = featureFlags.ownStorage !== false;
    const storageOptions = jobStorageOptions(storages, ownStorages, ownAllowed);
    const [runs, setRuns] = useState<Record<number, BackupRun[]>>({});
    const [loading, setLoading] = useState(true);
    const [showForm, setShowForm] = useState(false);
    const [editingJob, setEditingJob] = useState<BackupJob | null>(null);
    const [busyJob, setBusyJob] = useState<number | null>(null);
    const [restoreTarget, setRestoreTarget] = useState<BackupRun | null>(null);
    const [restoreCountdown, setRestoreCountdown] = useState(5);
    const [restoring, setRestoring] = useState(false);
    const [restoreHistory, setRestoreHistory] = useState<BackupRestore[]>([]);
    // The per-server quota, only meaningful for node-local storage with its own budget.
    const [backupConfig, setBackupConfig] = useState<BackupConfig | null>(null);
    const [backupUsage, setBackupUsage] = useState<BackupUsage | null>(null);
    const [expandedJobs, setExpandedJobs] = useState<Set<number>>(new Set());
    const toggleRuns = (id: number) => setExpandedJobs(prev => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id); else next.add(id);
        return next;
    });

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
        const [jobsRes, storagesRes, restoresRes, ownRes] = await Promise.all([
            listBackupJobs(serverId),
            listBackupStorages(),
            listBackupRestores(serverId),
            isOwner ? listOwnBackupStorages() : Promise.resolve({ success: true, storages: [] as OwnBackupStorage[] }),
        ]);
        if (jobsRes.success && jobsRes.jobs) setJobs(jobsRes.jobs);
        if (storagesRes.success && storagesRes.storages) setStorages(storagesRes.storages);
        if (ownRes.success) setOwnStorages(ownRes.storages ?? []);
        if (restoresRes.success && restoresRes.restores) setRestoreHistory(restoresRes.restores);
        setLoading(false);

        // Load runs per job in parallel.
        const allRuns: Record<number, BackupRun[]> = {};
        await Promise.all((jobsRes.jobs ?? []).map(async j => {
            const r = await listBackupRuns(j.id);
            if (r.success && r.runs) allRuns[j.id] = r.runs;
        }));
        setRuns(allRuns);
    }, [serverId, isOwner]);

    useEffect(() => { reload(); }, [reload]);

    // The same two reads the Overview's backup card makes.
    useEffect(() => {
        getBackupConfig().then(res => {
            if (res.success && res.settings) setBackupConfig(res.settings);
        }).catch(() => {});
    }, []);
    // A string, not `jobs`: the poll replaces that array every tick while a run is going.
    const finishedKey = jobs.map(j => `${j.id}:${j.lastRunAt ?? ''}`).join(',');
    useEffect(() => {
        if (!serverId || backupConfig?.mode !== 'node-local') { setBackupUsage(null); return; }
        getBackupUsage(serverId).then(u => { if (u && u.success) setBackupUsage(u); }).catch(() => {});
    }, [serverId, backupConfig, finishedKey]);

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
            showToast(res.message || 'Failed to create job.', false);
        }
    };
    const handleUpdate = async (job: Partial<BackupJob>) => {
        if (!editingJob) return;
        // "Default storage" is null in the form, and the API reads a null
        // storageId as "unchanged": 0 is what clears it.
        const res = await updateBackupJob(editingJob.id, { ...job, storageId: job.storageId ?? 0 });
        if (res.success) {
            showToast('Job saved.');
            setEditingJob(null);
            reload();
        } else {
            showToast(res.message || 'Failed to save job.', false);
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

    // Summary across the jobs of this server.
    const totalCount = jobs.reduce((n, j) => n + (j.keptRuns ?? 0), 0);
    const totalBytes = jobs.reduce((n, j) => n + (j.keptBytes ?? 0), 0);
    const nextJob = jobs
        .filter(j => j.enabled && j.schedule !== 'manual' && j.nextRunAt)
        .sort((a, b) => new Date(a.nextRunAt!).getTime() - new Date(b.nextRunAt!).getTime())[0];
    // A cap of 0 is a real ceiling ("none allowed"), null is no cap; only a positive one draws a meter.
    const quotaGb = backupConfig?.mode === 'node-local' && !backupConfig.shareQuotaWithServer ? backupConfig.quotaPerServerGb : null;
    const quotaBytes = quotaGb !== null && quotaGb > 0 ? quotaGb * 1024 ** 3 : 0;
    const quotaUsed = backupUsage && !backupUsage.degraded ? backupUsage.usedBytes : null;
    const showQuota = quotaBytes > 0 && quotaUsed !== null;
    const quotaPct = showQuota ? quotaUsed / quotaBytes : 0;

    return (
        <div className="flex flex-col gap-6">
            <div className="flex flex-wrap items-start gap-x-4 gap-y-3">
                <div className="min-w-0">
                    <h1 className="h-page">Backups</h1>
                    <p className="mt-1 text-sm text-(--base-06)">
                        {server.activeSubServer
                            ? <>Scheduled and manual archives for <span className="srv-num text-(--base-07)">{server.activeSubServer}</span>.</>
                            : 'Scheduled and manual archives. No sub-server selected.'}
                    </p>
                </div>
                {/* The empty state carries its own "New job" button. */}
                {(loading || jobs.length > 0) && (
                    <button onClick={() => setShowForm(true)} className="btn btn-primary ml-auto">
                        <Plus size={16} /> New job
                    </button>
                )}
            </div>

            {storageOptions.length === 0 && (
                <Callout tone="warning" icon={<CircleAlert size={16} />} title="No storage configured">
                    Admins: head over to Settings → Backups and add at least one storage (Local or S3-compatible) before creating jobs. You can also connect your own bucket under Account &gt; Storage.
                </Callout>
            )}

            {!loading && jobs.length > 0 && (
                <div className="grid grid-cols-1 gap-4 sm:grid-cols-3">
                    <Tile
                        label="Backups kept"
                        value={totalCount}
                        sub={`${jobs.length} ${jobs.length === 1 ? 'job' : 'jobs'}`}
                    />
                    <Tile
                        label="Total size"
                        value={formatBytes(showQuota ? quotaUsed! : totalBytes)}
                        meter={showQuota ? (
                            <Meter value={Math.min(quotaUsed!, quotaBytes)} min={0} max={quotaBytes} aria-label="Backup storage used">
                                <MeterTrack className="srv-meter-track">
                                    <MeterIndicator className="srv-meter-fill" data-tone={quotaPct >= 0.9 ? 'error' : quotaPct >= 0.75 ? 'warning' : undefined} />
                                </MeterTrack>
                            </Meter>
                        ) : undefined}
                        sub={showQuota ? `of ${formatBytes(quotaBytes)} quota` : undefined}
                    />
                    <Tile
                        label="Next scheduled run"
                        value={<span className="font-body">{nextJob ? relativeTime(nextJob.nextRunAt) : 'None'}</span>}
                        sub={nextJob ? `${nextJob.name}, ${localTime(nextJob.nextRunAt!)}` : 'No job is scheduled'}
                    />
                </div>
            )}

            {loading ? (
                <div className="flex flex-col gap-4">
                    {Array.from({ length: 2 }).map((_, i) => (
                        <div key={i} className="srv-surface space-y-4 p-5">
                            <div className="flex items-start justify-between gap-3">
                                <div className="space-y-2">
                                    <SkeletonText width="w-40" className="h-4" />
                                    <SkeletonText width="w-56" className="h-3" />
                                </div>
                                <Skeleton className="h-8 w-24 rounded-md" />
                            </div>
                            {Array.from({ length: 3 }).map((_, j) => (
                                <div key={j} className="flex items-center gap-3 px-3">
                                    <SkeletonText width="w-28" className="flex-1" />
                                    <SkeletonText width="w-16" />
                                    <Skeleton className="h-5 w-16 rounded-full" />
                                    <Skeleton className="h-7 w-7 rounded-md" />
                                </div>
                            ))}
                        </div>
                    ))}
                </div>
            ) : jobs.length === 0 ? (
                <div className="srv-surface flex flex-col items-center justify-center gap-3 px-6 py-16 text-center">
                    <HardDrive size={28} className="text-(--base-05)" />
                    <div>
                        <p className="text-sm font-medium text-(--base-09)">No backup jobs yet</p>
                        <p className="mt-1 text-sm text-(--base-06)">A job decides what is archived, when, and how many copies are kept.</p>
                    </div>
                    <button onClick={() => setShowForm(true)} className="btn btn-primary"><Plus size={16} /> New job</button>
                </div>
            ) : (
                <div className="flex flex-col gap-4">
                    {jobs.map(job => {
                        const skip = currentSkip(job);
                        const scheduled = job.enabled && job.schedule !== 'manual' && job.nextRunAt;
                        const all = runs[job.id] ?? [];
                        const open = expandedJobs.has(job.id);
                        const shown = open ? all : all.slice(0, RUNS_COLLAPSED);
                        return (
                            <section key={job.id} className="srv-surface p-5" aria-label={job.name}>
                                <div className="flex flex-wrap items-start gap-x-4 gap-y-3">
                                    <div className="min-w-0 flex-1">
                                        <div className="flex flex-wrap items-center gap-2">
                                            <h2 className="text-base font-medium text-(--base-09)">{job.name}</h2>
                                            {!job.enabled && <StatusPill tone="neutral">Disabled</StatusPill>}
                                            {job.subServer ? <StatusPill tone="neutral">{job.subServer}</StatusPill> : <StatusPill tone="accent">Full container</StatusPill>}
                                            {(job.includePatterns?.length || job.excludePatterns?.length) ? <StatusPill tone="accent">Selective</StatusPill> : null}
                                        </div>
                                        <div className="mt-1.5 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-(--base-06)">
                                            <span className="inline-flex items-center gap-1.5"><Clock size={14} />{describeSchedule(job.schedule, job.timezone)}</span>
                                            <span>Keep last <span className="srv-num">{job.retentionCount}</span></span>
                                            <span title={job.lastRunAt ? localTime(job.lastRunAt) : undefined}>Last run {job.lastRunAt ? `${relativeTime(job.lastRunAt)} (${localTime(job.lastRunAt)})` : 'never'}</span>
                                            <span title={scheduled ? localTime(job.nextRunAt!) : undefined}>
                                                Next run {scheduled ? `${relativeTime(job.nextRunAt)} (${localTime(job.nextRunAt!)})` : job.enabled ? 'manual only' : 'disabled'}
                                            </span>
                                            <span className="srv-num">{job.keptRuns ?? 0} {(job.keptRuns ?? 0) === 1 ? 'backup' : 'backups'}, {formatBytes(job.keptBytes ?? 0)}</span>
                                        </div>
                                    </div>
                                    <div className="flex items-center gap-1.5">
                                        <button onClick={() => handleTrigger(job.id)} disabled={busyJob === job.id} className="btn btn-secondary btn-sm">
                                            {busyJob === job.id ? <Spinner size="xs" /> : <Play size={14} />}
                                            Run now
                                        </button>
                                        <Menu
                                            trigger={<button className={MENU_BTN} aria-label={`Actions for ${job.name}`}><MoreHorizontal size={16} /></button>}
                                            items={[
                                                { label: 'Edit job', icon: <Pencil size={14} />, onSelect: () => setEditingJob(job) },
                                                { type: 'separator' },
                                                { label: 'Delete job', icon: <Trash2 size={14} />, danger: true, onSelect: () => handleDeleteJob(job.id) },
                                            ]}
                                        />
                                    </div>
                                </div>

                                {skip && (
                                    <div className="mt-4">
                                        <Callout tone="warning" icon={<AlertTriangle size={16} />} role="status" title={`Skipped at ${localTime(skip.at)}`}>
                                            <span className="break-words">{skip.reason}</span>
                                        </Callout>
                                    </div>
                                )}

                                {all.length > 0 ? (
                                    <div className="mt-4">
                                        <div className="flex items-center justify-between gap-2 px-3 pb-1">
                                            <span className="srv-label">Backups</span>
                                            <span className="srv-num text-xs text-(--base-06)">{shown.length} of {all.length}</span>
                                        </div>
                                        <div className="flex flex-col">
                                            {shown.map(run => (
                                                <div key={run.id}>
                                                    <div className="srv-backups-row">
                                                        <span className="min-w-0 flex-1 truncate text-sm text-(--base-08)">
                                                            {localTime(run.startedAt)}
                                                            <span className="ml-2 text-xs text-(--base-06)">{relativeTime(run.startedAt)}</span>
                                                        </span>
                                                        <span className="srv-num w-20 text-right text-xs text-(--base-07)" title={run.status === 'running' ? 'Archived so far' : 'Archive size'}>{runSize(run.sizeBytes)}</span>
                                                        <span className="w-24"><StatusPill tone={runTone(run.status)} pulse={run.status === 'running'}>{cap(run.status)}</StatusPill></span>
                                                        <Menu
                                                            trigger={<button className={MENU_BTN} aria-label={`Actions for backup from ${localTime(run.startedAt)}`}><MoreHorizontal size={16} /></button>}
                                                            items={[
                                                                ...(run.status === 'success' ? [
                                                                    { label: 'Restore', icon: <Undo2 size={14} />, onSelect: () => setRestoreTarget(run) },
                                                                    { label: 'Download', icon: <Download size={14} />, href: backupDownloadUrl(run.id) },
                                                                    { type: 'separator' as const },
                                                                ] : []),
                                                                { label: 'Delete', icon: <Trash2 size={14} />, danger: true, onSelect: () => handleDeleteRun(run.id) },
                                                            ]}
                                                        />
                                                    </div>
                                                    {/* Its own line, full width: the one thing a failed
                                                        backup has to tell you must be readable. */}
                                                    {run.status === 'failed' && run.errorMessage && (
                                                        <p className="break-words px-3 pb-2 text-xs text-(--error-light)">{run.errorMessage}</p>
                                                    )}
                                                </div>
                                            ))}
                                        </div>
                                        {all.length > RUNS_COLLAPSED && (
                                            <button
                                                type="button"
                                                onClick={() => toggleRuns(job.id)}
                                                aria-expanded={open}
                                                className="btn btn-ghost btn-sm mt-1 w-full justify-center"
                                            >
                                                {open ? 'Show fewer' : `Show all ${all.length} runs`}
                                            </button>
                                        )}
                                        {/* Core lists the newest 50 runs; a longer retention keeps more. */}
                                        {open && (job.keptRuns ?? 0) > all.filter(r => r.status === 'success').length && (
                                            <p className="mt-2 px-3 text-xs text-(--base-06)">
                                                Showing the newest {all.length} runs. This job keeps {job.keptRuns} backups in total.
                                            </p>
                                        )}
                                    </div>
                                ) : (
                                    <p className="mt-4 px-3 text-sm text-(--base-06)">No backups yet. Use Run now to create the first one.</p>
                                )}
                            </section>
                        );
                    })}
                </div>
            )}

            {restoreHistory.length > 0 && (
                <section className="srv-surface p-5" aria-label="Restore history">
                    <div className="mb-2 flex items-baseline gap-2 px-3">
                        <h2 className="text-sm font-medium text-(--base-09)">Restore history</h2>
                        <span className="srv-num text-xs text-(--base-06)">{restoreHistory.length}</span>
                    </div>
                    <div className="flex flex-col">
                        {restoreHistory.slice(0, 8).map(restore => (
                            <div key={restore.id} className="srv-backups-row flex-wrap">
                                <span className="min-w-0 flex-1 text-sm text-(--base-08)">
                                    Run <span className="srv-num">#{restore.runId}</span>
                                    <span className="ml-2 text-xs text-(--base-06)">{relativeTime(restore.requestedAt)}</span>
                                </span>
                                {restore.stalled && (
                                    <span className="max-w-xs truncate text-xs text-(--warning-light)" title={restore.stallReason}>{restore.stallReason}</span>
                                )}
                                {restore.errorMessage && (
                                    <span className="max-w-xs truncate text-xs text-(--error-light)" title={restore.errorMessage}>{restore.errorMessage}</span>
                                )}
                                {/* No pulse on a stalled one: the animation is the
                                    claim that something is happening, and for
                                    these nothing is until the node returns. */}
                                <StatusPill
                                    tone={restore.status === 'success' ? 'success' : restore.status === 'failed' ? 'error' : 'warning'}
                                    pulse={restore.status !== 'success' && restore.status !== 'failed' && !restore.stalled}
                                >
                                    {restore.stalled ? 'Paused' : cap(restore.status)}
                                </StatusPill>
                            </div>
                        ))}
                    </div>
                </section>
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
                    storages={storageOptions}
                    subServers={subServers}
                    onClose={() => setShowForm(false)}
                    onSave={handleCreate}
                />
            )}
            {editingJob && (
                <JobForm
                    initial={editingJob}
                    storages={jobStorageOptions(storages, ownStorages, ownAllowed, editingJob.storageId ?? null)}
                    subServers={subServers}
                    onClose={() => setEditingJob(null)}
                    onSave={handleUpdate}
                />
            )}

            {restoreTarget && (
                <div className="modal-overlay animate-fade-in" onClick={() => !restoring && setRestoreTarget(null)}>
                    <ModalPanel onClose={() => !restoring && setRestoreTarget(null)} className="modal-panel max-w-md" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--warning-light)">
                                <AlertTriangle size={18} /> Restore backup
                            </h3>
                        </div>
                        <div className="modal-body space-y-3">
                            <p className="text-sm text-(--base-08)">
                                Replace the current world data with the backup from {' '}
                                <span className="srv-num text-(--base-09)">{fmtDateTime(restoreTarget.startedAt)}</span>?
                            </p>
                            <Callout tone="warning" icon={<AlertTriangle size={16} />}>
                                Your server will be stopped, the archive extracted into the sub-server directory, and the server restarted automatically. The previous contents are moved aside (kept until next disk cleanup) — but anything written since this backup will be gone.
                            </Callout>
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
                                        : <><Undo2 size={14} /> Restore now</>}
                            </button>
                        </div>
                    </ModalPanel>
                </div>
            )}
        </div>
    );
}
