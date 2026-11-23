'use client';

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import {
    AlertTriangle, Archive, Check, ChevronDown, ChevronRight, Download,
    KeyRound, MoreHorizontal, Pencil, Play, Plus, Trash2,
} from 'lucide-react';
import { listBackupStorages, type BackupStorage } from '@/lib/api';
import {
    EMPTY_SELECTION,
    createPlatformBackupJob, deletePlatformBackupJob, getPlatformBackupPassphraseStatus,
    listPlatformBackupJobs, listPlatformBackupRuns, listPlatformBackupTargets,
    platformBackupDownloadUrl, runPlatformBackupJob, setPlatformBackupPassphrase,
    updatePlatformBackupJob,
    type BackupTargetServer, type PlatformBackupJob, type PlatformBackupRun,
    type PlatformBackupSelection, type PlatformBackupServerMode,
} from '@/lib/api/platformBackups';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard from '@/components/settings/SettingsCard';
import PlatformRestoreCard from '@/components/settings/PlatformRestoreCard';
import { SkeletonList } from '@/components/Skeleton';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { useBusy } from '@/lib/useBusy';
import { scheduleWarning } from '@/lib/platformBackupSchedule';
import BackupScheduleEditor from '@/components/BackupScheduleEditor';
import { describeSchedule, editorZone, parseSchedule, usesTimeZone } from '@/lib/backupSchedule';
import { fmtDateTime } from '@/lib/time';
import Select from '@/components/ui/Select';
import Checkbox from '@/components/ui/Checkbox';
import Segmented from '@/components/ui/Segmented';
import Menu from '@/components/ui/Menu';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';
import Callout from '@/components/server/Callout';
import '@/styles/settings-storage.css';

const MIN_PASSPHRASE = 12;

function formatSize(bytes: number): string {
    if (!bytes) return '0 B';
    if (bytes >= 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
    if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
    return `${Math.round(bytes / 1024)} KB`;
}

function formatWhen(iso?: string): string {
    if (!iso) return 'never';
    return fmtDateTime(iso);
}

const COMPONENTS: { key: keyof Omit<PlatformBackupSelection, 'servers'>; label: string; hint: string }[] = [
    { key: 'database', label: 'Platform database', hint: 'Users, servers, jobs, settings, and the encrypted credentials that make the rest usable.' },
    { key: 'modpacks', label: 'Modpacks', hint: 'Published packs and their builds in Core storage.' },
    { key: 'metricsDb', label: 'Statistics database', hint: 'The TimescaleDB statistics, dumped into the same bundle. If it is configured but cannot be dumped, the run fails. Restoring it needs a superuser on a new database with the same TimescaleDB version.' },
];

interface JobDraft {
    id: number;
    name: string;
    schedule: string;
    timezone: string;
    retentionCount: number;
    storageId: number | null;
    enabled: boolean;
    selection: PlatformBackupSelection;
}

function draftFrom(job: PlatformBackupJob): JobDraft {
    return {
        id: job.id,
        name: job.name,
        schedule: job.schedule,
        timezone: editorZone(job.schedule, job.timezone),
        retentionCount: job.retentionCount,
        storageId: job.storageId ?? null,
        enabled: job.enabled,
        selection: { ...EMPTY_SELECTION, ...job.selection, servers: { ...job.selection?.servers } },
    };
}

function emptyDraft(): JobDraft {
    return {
        id: 0,
        name: 'Nightly platform backup',
        schedule: 'manual',
        timezone: editorZone('manual'),
        retentionCount: 3,
        storageId: null,
        enabled: true,
        selection: { ...EMPTY_SELECTION, database: true, servers: { mode: 'none' } },
    };
}

/** Mirrors the server's rule, so the button is inert for the same reasons. */
function selectionProblem(sel: PlatformBackupSelection): string {
    if (sel.servers.mode === 'owner' && !sel.servers.ownerId) {
        return 'Pick the user whose servers should be included.';
    }
    if (sel.servers.mode === 'list' && !(sel.servers.serverIds || []).length) {
        return 'Pick at least one server.';
    }
    const nothing = !sel.database && !sel.metricsDb && !sel.modpacks && sel.servers.mode === 'none';
    if (nothing) return 'Choose at least one thing to back up.';
    return '';
}

export default function PlatformBackupsTab() {
    const [loading, setLoading] = useState(true);
    const [jobs, setJobs] = useState<PlatformBackupJob[]>([]);
    const [storages, setStorages] = useState<BackupStorage[]>([]);
    const [targets, setTargets] = useState<BackupTargetServer[]>([]);
    const [byonOffered, setByonOffered] = useState(false);
    const [passphraseSet, setPassphraseSet] = useState(false);

    const [draft, setDraft] = useState<JobDraft | null>(null);
    const [savingJob, runSaveJob] = useBusy();
    const [expanded, setExpanded] = useState<number | null>(null);
    const [runs, setRuns] = useState<Record<number, PlatformBackupRun[]>>({});
    const [running, runRunning] = useBusy();

    const reload = useCallback(async () => {
        setLoading(true);
        const [jobRes, stRes, tgRes, ppRes] = await Promise.all([
            listPlatformBackupJobs(),
            listBackupStorages(),
            listPlatformBackupTargets(),
            getPlatformBackupPassphraseStatus(),
        ]);
        if (jobRes.success && jobRes.jobs) setJobs(jobRes.jobs);
        if (stRes.success && stRes.storages) setStorages(stRes.storages);
        if (tgRes.success) {
            setTargets(tgRes.targets || []);
            setByonOffered(!!tgRes.byonOffered);
        }
        if (ppRes.success) setPassphraseSet(!!ppRes.isSet);
        setLoading(false);
    }, []);

    useEffect(() => { reload(); }, [reload]);

    const loadRuns = useCallback(async (jobId: number) => {
        const res = await listPlatformBackupRuns(jobId);
        if (res.success && res.runs) setRuns(prev => ({ ...prev, [jobId]: res.runs! }));
    }, []);

    const toggleExpanded = (jobId: number) => {
        if (expanded === jobId) { setExpanded(null); return; }
        setExpanded(jobId);
        loadRuns(jobId);
    };

    const saveJob = async () => {
        if (!draft) return;
        const problem = selectionProblem(draft.selection);
        if (problem) { toast(problem, false); return; }
        await runSaveJob(async () => {
            const input = {
                name: draft.name,
                schedule: draft.schedule,
                // Omitted for a relative schedule, which never reads it: a
                // changed zone re-arms the job and would push "every 1d" back.
                timezone: usesTimeZone(parseSchedule(draft.schedule).kind) ? draft.timezone : undefined,
                selection: draft.selection,
                storageId: draft.storageId,
                retentionCount: draft.retentionCount,
                enabled: draft.enabled,
            };
            const res = draft.id === 0
                ? await createPlatformBackupJob(input)
                : await updatePlatformBackupJob(draft.id, input);
            if (res.success) {
                toast('Saved.');
                setDraft(null);
                reload();
            } else {
                toast(res.message || 'Save failed.', false);
            }
        });
    };

    const removeJob = async (job: PlatformBackupJob) => {
        const ok = await confirmDialog({
            title: `Delete "${job.name}"?`,
            message: 'The bundles it already wrote stay where they are. Only the schedule and its history go.',
            confirmLabel: 'Delete',
            destructive: true,
        });
        if (!ok) return;
        const res = await deletePlatformBackupJob(job.id);
        if (res.success) { toast('Deleted.'); reload(); } else { toast(res.message || 'Delete failed.', false); }
    };

    const runNow = async (job: PlatformBackupJob) => {
        await runRunning(async () => {
            const res = await runPlatformBackupJob(job.id);
            if (res.success) {
                toast('Backup finished.');
            } else {
                toast(res.message || 'The backup failed.', false);
            }
            setExpanded(job.id);
            loadRuns(job.id);
            reload();
        });
    };

    return (
        <SettingsPage
            title="Platform Backups"
            description="The platform itself: the database, Core storage, and the servers you select. Sealed under a passphrase so a bundle can be restored onto a different Dylaris."
            icon={Archive}
            width="4xl"
            loading={loading}
            skeletonCards={2}
        >
            <PassphraseCard isSet={passphraseSet} onChanged={() => setPassphraseSet(true)} />

            <PlatformRestoreCard />

            <SettingsCard
                title="Backup jobs"
                description="What each run covers, and where it goes."
                icon={Archive}
            >
                {!passphraseSet && (
                    <div className="mb-3">
                        <Callout tone="warning" icon={<AlertTriangle size={14} />} role="alert">
                            Set a backup passphrase first. A run without one is refused, because a bundle nobody chose a passphrase for is one anybody can open.
                        </Callout>
                    </div>
                )}

                {jobs.length === 0 && !draft && (
                    <EmptyState
                        bare
                        icon={<Archive size={20} />}
                        title="No platform backup is configured yet"
                        description="Create a job to back up the database, Core storage and selected servers on a schedule."
                    />
                )}

                {jobs.length > 0 && (
                    <div className="-mx-1.5">
                        <RowList label="Platform backup jobs">
                            {jobs.map(job => (
                                <Row
                                    key={job.id}
                                    leading={
                                        <button
                                            type="button"
                                            onClick={() => toggleExpanded(job.id)}
                                            aria-label={expanded === job.id ? 'Hide run history' : 'Show run history'}
                                            aria-expanded={expanded === job.id}
                                            className="app-icon-btn"
                                        >
                                            {expanded === job.id ? <ChevronDown size={16} /> : <ChevronRight size={16} />}
                                        </button>
                                    }
                                    title={
                                        <>
                                            {job.name}
                                            {!job.enabled && <span className="ml-2 text-xs font-normal text-(--base-06)">(disabled)</span>}
                                        </>
                                    }
                                    meta={
                                        <>
                                            {summarise(job.selection, targets)} &middot; {describeSchedule(job.schedule, job.timezone)} &middot; last run {formatWhen(job.lastRunAt)}
                                            {job.enabled && job.schedule !== 'manual' && job.nextRunAt && <> &middot; next run {formatWhen(job.nextRunAt)}</>}
                                        </>
                                    }
                                    actions={
                                        <>
                                            <button type="button" onClick={() => runNow(job)} disabled={running || !passphraseSet}
                                                className="btn btn-secondary btn-sm flex items-center gap-1.5">
                                                <Play size={13} /> Run now
                                            </button>
                                            <Menu
                                                trigger={
                                                    <button type="button" className="app-icon-btn" aria-label={`Actions for ${job.name}`}>
                                                        <MoreHorizontal size={16} />
                                                    </button>
                                                }
                                                items={[
                                                    { label: 'Edit', icon: <Pencil size={14} />, onSelect: () => setDraft(draftFrom(job)) },
                                                    { type: 'separator' },
                                                    { label: 'Delete', icon: <Trash2 size={14} />, danger: true, onSelect: () => removeJob(job) },
                                                ]}
                                            />
                                        </>
                                    }
                                />
                            ))}
                        </RowList>
                        {/* Warnings and run history sit under the list: a list
                            only holds rows, and history is open for one job at a time. */}
                        <div className="set-sto-notes">
                            {jobs.map(job => {
                                const warning = scheduleWarning(job);
                                if (!warning && expanded !== job.id) return null;
                                return (
                                    <div key={job.id} className="space-y-2">
                                        <div className="srv-label">{job.name}</div>
                                        {warning && (
                                            <Callout tone="warning" icon={<AlertTriangle size={14} />}>{warning}</Callout>
                                        )}
                                        {expanded === job.id && <RunHistory runs={runs[job.id]} />}
                                    </div>
                                );
                            })}
                        </div>
                    </div>
                )}

                {!draft && (
                    <button type="button" onClick={() => setDraft(emptyDraft())}
                        className="btn btn-secondary btn-sm mt-3 flex items-center gap-1.5">
                        <Plus size={14} /> New backup job
                    </button>
                )}

                {draft && (
                    <JobEditor
                        draft={draft}
                        onChange={setDraft}
                        storages={storages}
                        targets={targets}
                        byonOffered={byonOffered}
                        saving={savingJob}
                        onSave={saveJob}
                        onCancel={() => setDraft(null)}
                    />
                )}
            </SettingsCard>
        </SettingsPage>
    );
}

// ───────────── Passphrase ─────────────

function PassphraseCard({ isSet, onChanged }: { isSet: boolean; onChanged: () => void }) {
    const [value, setValue] = useState('');
    const [repeat, setRepeat] = useState('');
    const [replacing, setReplacing] = useState(false);
    const [saving, runSaving] = useBusy();

    const tooShort = value.length > 0 && value.length < MIN_PASSPHRASE;
    const mismatch = repeat.length > 0 && repeat !== value;
    const canSave = value.length >= MIN_PASSPHRASE && repeat === value;

    const save = async () => {
        await runSaving(async () => {
            const res = await setPlatformBackupPassphrase(value, isSet);
            if (res.success) {
                toast('Passphrase saved.');
                setValue(''); setRepeat(''); setReplacing(false);
                onChanged();
            } else {
                toast(res.message || 'Could not save the passphrase.', false);
            }
        });
    };

    return (
        <SettingsCard
            title="Backup passphrase"
            description="Every platform bundle is encrypted with it, and nothing else can open one."
            icon={KeyRound}
        >
            <div className="mb-3">
                <Callout tone="warning" icon={<AlertTriangle size={14} />}>
                    <strong>Write this down somewhere outside Dylaris.</strong> A bundle is restored on
                    machines that hold no copy of it - a fresh install, or a different instance - so
                    losing the passphrase loses every bundle taken under it. There is no recovery and
                    no reset: that is what makes a downloaded bundle safe to keep anywhere.
                </Callout>
            </div>

            {isSet && !replacing ? (
                <div className="flex items-center gap-3">
                    <span className="flex items-center gap-1.5 text-sm text-(--base-08)">
                        <Check size={15} className="text-(--success-light)" /> A passphrase is set.
                    </span>
                    <button type="button" onClick={() => setReplacing(true)} className="btn btn-secondary btn-sm ml-auto">Replace</button>
                </div>
            ) : (
                <div className="space-y-3">
                    {isSet && (
                        <Callout tone="warning" icon={<AlertTriangle size={14} />}>
                            Replacing it does not re-encrypt bundles already written. Those still
                            need the old passphrase, so keep it as long as you keep them.
                        </Callout>
                    )}
                    <div>
                        <label className="input-label mb-1 block" htmlFor="pb-pass">Passphrase</label>
                        <input id="pb-pass" type="password" className="input-field w-full" value={value}
                            autoComplete="new-password"
                            onChange={e => setValue(e.target.value)} />
                        {tooShort && (
                            <p className="text-xs text-(--error-light) mt-1">At least {MIN_PASSPHRASE} characters.</p>
                        )}
                    </div>
                    <div>
                        <label className="input-label mb-1 block" htmlFor="pb-pass-2">Repeat</label>
                        <input id="pb-pass-2" type="password" className="input-field w-full" value={repeat}
                            autoComplete="new-password"
                            onChange={e => setRepeat(e.target.value)} />
                        {mismatch && (
                            <p className="text-xs text-(--error-light) mt-1">The two do not match.</p>
                        )}
                    </div>
                    <div className="flex gap-2">
                        <button type="button" className="btn btn-primary btn-sm" disabled={!canSave || saving} onClick={save}>
                            {saving ? 'Saving...' : 'Save passphrase'}
                        </button>
                        {isSet && (
                            <button type="button" className="btn btn-secondary btn-sm" onClick={() => { setReplacing(false); setValue(''); setRepeat(''); }}>
                                Cancel
                            </button>
                        )}
                    </div>
                </div>
            )}
        </SettingsCard>
    );
}

// ───────────── Job editor ─────────────

function JobEditor({
    draft, onChange, storages, targets, byonOffered, saving, onSave, onCancel,
}: {
    draft: JobDraft;
    onChange: (d: JobDraft) => void;
    storages: BackupStorage[];
    targets: BackupTargetServer[];
    byonOffered: boolean;
    saving: boolean;
    onSave: () => void;
    onCancel: () => void;
}) {
    const [filter, setFilter] = useState('');
    const problem = selectionProblem(draft.selection);

    const owners = useMemo(() => {
        const seen = new Map<string, number>();
        for (const t of targets) seen.set(t.ownerId, (seen.get(t.ownerId) || 0) + 1);
        return [...seen.entries()].sort((a, b) => b[1] - a[1]);
    }, [targets]);

    const visible = useMemo(() => {
        const q = filter.trim().toLowerCase();
        if (!q) return targets;
        return targets.filter(t => t.name.toLowerCase().includes(q) || t.uuid.toLowerCase().includes(q));
    }, [targets, filter]);

    const setSel = (patch: Partial<PlatformBackupSelection>) =>
        onChange({ ...draft, selection: { ...draft.selection, ...patch } });

    const setMode = (mode: PlatformBackupServerMode) =>
        onChange({ ...draft, selection: { ...draft.selection, servers: { ...draft.selection.servers, mode } } });

    const toggleServer = (id: number) => {
        const current = draft.selection.servers.serverIds || [];
        const next = current.includes(id) ? current.filter(x => x !== id) : [...current, id];
        onChange({ ...draft, selection: { ...draft.selection, servers: { ...draft.selection.servers, serverIds: next } } });
    };

    return (
        <div className="set-sto-editor space-y-4">
            <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                <div>
                    <label className="input-label mb-1 block" htmlFor="pb-name">Name</label>
                    <input id="pb-name" className="input-field w-full" value={draft.name}
                        onChange={e => onChange({ ...draft, name: e.target.value })} />
                </div>
                <div className="md:col-span-2">
                    <BackupScheduleEditor key={draft.id} idPrefix="pb-schedule" schedule={draft.schedule} timezone={draft.timezone}
                        onChange={(schedule, timezone) => onChange({ ...draft, schedule, timezone })} />
                </div>
                <div>
                    <label className="input-label mb-1 block" htmlFor="pb-storage">Destination</label>
                    {/* Already platform-only: a tenant's own storages live
                        behind /me/backup-storages and never appear here. */}
                    <Select id="pb-storage" ariaLabel="Destination" value={draft.storageId == null ? '' : String(draft.storageId)}
                        onChange={v => onChange({ ...draft, storageId: v ? Number(v) : null })}
                        options={[{ value: '', label: 'Default storage' }, ...storages.map(s => ({ value: String(s.id), label: s.name }))]} />
                </div>
                <div>
                    <label className="input-label mb-1 block" htmlFor="pb-retention">Keep</label>
                    <input id="pb-retention" type="number" min={1} className="input-field w-full" value={draft.retentionCount}
                        onChange={e => onChange({ ...draft, retentionCount: Math.max(1, Number(e.target.value) || 1) })} />
                </div>
            </div>

            <div>
                <p className="input-label mb-2">What to include</p>
                <div className="space-y-2">
                    {COMPONENTS.map(c => (
                        <Checkbox key={c.key} checked={!!draft.selection[c.key]}
                            onChange={checked => setSel({ [c.key]: checked } as Partial<PlatformBackupSelection>)}
                            label={c.label} hint={c.hint} />
                    ))}
                </div>
            </div>

            <div>
                <p className="input-label mb-2">Servers</p>
                <p className="text-xs text-(--base-06) mb-2">
                    Whole servers only. A selected server runs its OWN backup job, so the archive lands
                    under its owner&apos;s quota and retention and stays individually restorable. One that
                    no longer exists is skipped and recorded, never an error.
                </p>
                <Segmented<PlatformBackupServerMode> className="flex-wrap mb-2" ariaLabel="Servers to include"
                    value={draft.selection.servers.mode} onChange={setMode}
                    options={[
                        { id: 'none', label: 'None' },
                        { id: 'all', label: 'All servers' },
                        { id: 'owner', label: 'By owner' },
                        { id: 'list', label: 'Pick individually' },
                        // Only meaningful where BYON tenants exist. A self-hoster has none
                        // to tell apart, so the filter would be over an empty distinction.
                        ...(byonOffered ? [{ id: 'byon' as const, label: 'All BYON servers' }] : []),
                    ]} />

                {draft.selection.servers.mode === 'owner' && (
                    <Select ariaLabel="Owner" value={draft.selection.servers.ownerId || ''}
                        onChange={v => onChange({
                            ...draft,
                            selection: { ...draft.selection, servers: { ...draft.selection.servers, ownerId: v || undefined } },
                        })}
                        options={[{ value: '', label: 'Pick a user' }, ...owners.map(([id, count]) => ({ value: id, label: `${id} (${count} server${count === 1 ? '' : 's'})` }))]} />
                )}

                {draft.selection.servers.mode === 'list' && (
                    <div className="space-y-2">
                        <input className="input-field w-full" placeholder="Filter by name or UUID" value={filter}
                            onChange={e => setFilter(e.target.value)} />
                        <div className="set-sto-picklist">
                            {visible.length === 0 && (
                                <p className="text-xs text-(--base-06) p-3">No server matches.</p>
                            )}
                            {visible.map(t => (
                                <label key={t.id} className="set-sto-pick">
                                    <Checkbox ariaLabel={`Include ${t.name}`}
                                        checked={(draft.selection.servers.serverIds || []).includes(t.id)}
                                        onChange={() => toggleServer(t.id)} />
                                    <span className="flex-1 min-w-0">
                                        <span className="text-sm text-(--base-09) truncate block">{t.name}</span>
                                        <span className="text-xs text-(--base-06) font-mono truncate block">{t.uuid}</span>
                                    </span>
                                    {t.byon && <span className="mono-label shrink-0">BYON</span>}
                                </label>
                            ))}
                        </div>
                    </div>
                )}
            </div>

            <Checkbox checked={draft.enabled} onChange={c => onChange({ ...draft, enabled: c })} label="Enabled" />

            {problem && <p className="text-xs text-(--error-light)">{problem}</p>}

            <div className="flex gap-2">
                <button type="button" className="btn btn-primary btn-sm" disabled={saving || !!problem} onClick={onSave}>
                    {saving ? 'Saving...' : 'Save job'}
                </button>
                <button type="button" className="btn btn-secondary btn-sm" onClick={onCancel}>Cancel</button>
            </div>
        </div>
    );
}

// ───────────── Runs ─────────────

function RunHistory({ runs }: { runs?: PlatformBackupRun[] }) {
    if (!runs) return <SkeletonList rows={2} />;
    if (runs.length === 0) {
        return <p className="text-xs text-(--base-06)">This job has not run yet.</p>;
    }
    return (
        <div className="set-sto-runs">
            {runs.map(run => (
                <div key={run.id} className="set-sto-run">
                    <div className="flex items-center gap-3">
                        <span className={`mono-label shrink-0 ${
                            run.status === 'success' ? 'text-(--success-light)'
                                : run.status === 'failed' ? 'text-(--error-light)' : 'text-(--base-06)'
                        }`}>{run.status}</span>
                        <span className="text-xs text-(--base-07) flex-1 min-w-0 truncate">
                            {formatWhen(run.startedAt)} &middot; {formatSize(run.sizeBytes)}
                        </span>
                        {run.status === 'success' && run.storageKey && (
                            <a className="btn btn-secondary btn-sm flex items-center gap-1.5" href={platformBackupDownloadUrl(run.id)}>
                                <Download size={13} /> Download
                            </a>
                        )}
                    </div>
                    {run.errorMessage && (
                        <p className="text-xs text-(--error-light) mt-1">{run.errorMessage}</p>
                    )}
                    {/* Skipped and failed parts are the point of this list. A run that
                        succeeded while leaving three servers out is still a success, and
                        the status alone cannot say so. */}
                    {(run.components || []).filter(c => c.status !== 'included').map((c, i) => (
                        <p key={i} className="text-xs text-(--base-06) mt-1">
                            <span className={c.status === 'failed' ? 'text-(--error-light)' : 'text-(--warning-light)'}>
                                {c.status}
                            </span>
                            {' '}{c.kind}{c.ref ? ` ${c.ref}` : ''}{c.message ? ` - ${c.message}` : ''}
                        </p>
                    ))}
                </div>
            ))}
        </div>
    );
}

// ───────────── helpers ─────────────

function summarise(sel: PlatformBackupSelection | undefined, targets: BackupTargetServer[]): string {
    if (!sel) return 'nothing';
    const parts: string[] = [];
    if (sel.database) parts.push('database');
    if (sel.modpacks) parts.push('modpacks');
    if (sel.metricsDb) parts.push('statistics');
    switch (sel.servers?.mode) {
        case 'all': parts.push(`all servers (${targets.length})`); break;
        case 'byon': parts.push(`BYON servers (${targets.filter(t => t.byon).length})`); break;
        case 'owner': parts.push("one owner's servers"); break;
        case 'list': parts.push(`${(sel.servers.serverIds || []).length} servers`); break;
        default: break;
    }
    return parts.length ? parts.join(', ') : 'nothing';
}

export { selectionProblem, summarise };
