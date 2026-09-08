'use client';

import React, { useCallback, useEffect, useMemo, useState } from 'react';
import {
    AlertTriangle, Archive, Check, ChevronDown, ChevronRight, Download,
    KeyRound, Play, Plus, Trash2,
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

const MIN_PASSPHRASE = 12;

function formatSize(bytes: number): string {
    if (!bytes) return '0 B';
    if (bytes >= 1024 * 1024 * 1024) return `${(bytes / (1024 * 1024 * 1024)).toFixed(2)} GB`;
    if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
    return `${Math.round(bytes / 1024)} KB`;
}

function formatWhen(iso?: string): string {
    if (!iso) return 'never';
    return new Date(iso).toLocaleString();
}

const COMPONENTS: { key: keyof Omit<PlatformBackupSelection, 'servers'>; label: string; hint: string }[] = [
    { key: 'database', label: 'Platform database', hint: 'Users, servers, jobs, settings, and the encrypted credentials that make the rest usable.' },
    { key: 'library', label: 'Library', hint: 'Server jars, loaders and uploads in Core storage.' },
    { key: 'modpacks', label: 'Modpacks', hint: 'Published packs and their builds in Core storage.' },
    { key: 'metricsDb', label: 'Statistics database', hint: 'Not covered yet: it is TimescaleDB and needs its own path. Selecting it records the gap rather than backing it up half way.' },
];

interface JobDraft {
    id: number;
    name: string;
    schedule: string;
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
    const nothing = !sel.database && !sel.metricsDb && !sel.library && !sel.modpacks && sel.servers.mode === 'none';
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
                    <div className="alert alert-warning text-xs mb-3">
                        <AlertTriangle size={13} className="text-(--warning-light) shrink-0 mt-0.5" />
                        <p>Set a backup passphrase first. A run without one is refused, because a bundle nobody chose a passphrase for is one anybody can open.</p>
                    </div>
                )}

                {jobs.length === 0 && !draft && (
                    <p className="text-sm text-(--base-07) mb-3">No platform backup is configured yet.</p>
                )}

                <div className="space-y-2">
                    {jobs.map(job => (
                        <div key={job.id} className="border border-(--base-04) rounded-md bg-(--base-02)">
                            <div className="flex items-center gap-3 p-3">
                                <button
                                    type="button"
                                    onClick={() => toggleExpanded(job.id)}
                                    aria-label={expanded === job.id ? 'Hide run history' : 'Show run history'}
                                    className="text-(--base-06) hover:text-(--base-09) transition-colors"
                                >
                                    {expanded === job.id ? <ChevronDown size={16} /> : <ChevronRight size={16} />}
                                </button>
                                <div className="flex-1 min-w-0">
                                    <p className="text-sm text-(--base-09) truncate">
                                        {job.name}
                                        {!job.enabled && <span className="ml-2 text-xs text-(--base-06)">(disabled)</span>}
                                    </p>
                                    <p className="text-xs text-(--base-06) truncate">
                                        {summarise(job.selection, targets)} &middot; {job.schedule} &middot; last run {formatWhen(job.lastRunAt)}
                                    </p>
                                </div>
                                <button type="button" onClick={() => runNow(job)} disabled={running || !passphraseSet}
                                    className="btn btn-sm flex items-center gap-1.5">
                                    <Play size={13} /> Run now
                                </button>
                                <button type="button" onClick={() => setDraft(draftFrom(job))} className="btn btn-sm">Edit</button>
                                <button type="button" onClick={() => removeJob(job)}
                                    aria-label={`Delete ${job.name}`}
                                    className="text-(--base-06) hover:text-(--error-light) transition-colors">
                                    <Trash2 size={15} />
                                </button>
                            </div>

                            {expanded === job.id && (
                                <RunHistory runs={runs[job.id]} />
                            )}
                        </div>
                    ))}
                </div>

                {!draft && (
                    <button type="button" onClick={() => setDraft(emptyDraft())}
                        className="btn btn-sm mt-3 flex items-center gap-1.5">
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
            <div className="alert alert-warning text-xs mb-3">
                <AlertTriangle size={13} className="text-(--warning-light) shrink-0 mt-0.5" />
                <p>
                    <strong>Write this down somewhere outside Dylaris.</strong> A bundle is restored on
                    machines that hold no copy of it - a fresh install, or a different instance - so
                    losing the passphrase loses every bundle taken under it. There is no recovery and
                    no reset: that is what makes a downloaded bundle safe to keep anywhere.
                </p>
            </div>

            {isSet && !replacing ? (
                <div className="flex items-center gap-3">
                    <span className="flex items-center gap-1.5 text-sm text-(--base-08)">
                        <Check size={15} className="text-(--success-light)" /> A passphrase is set.
                    </span>
                    <button type="button" onClick={() => setReplacing(true)} className="btn btn-sm ml-auto">Replace</button>
                </div>
            ) : (
                <div className="space-y-3">
                    {isSet && (
                        <div className="alert alert-warning text-xs">
                            <AlertTriangle size={13} className="text-(--warning-light) shrink-0 mt-0.5" />
                            <p>
                                Replacing it does not re-encrypt bundles already written. Those still
                                need the old passphrase, so keep it as long as you keep them.
                            </p>
                        </div>
                    )}
                    <div>
                        <label className="input-label mb-1 block" htmlFor="pb-pass">Passphrase</label>
                        <input id="pb-pass" type="password" className="input w-full" value={value}
                            autoComplete="new-password"
                            onChange={e => setValue(e.target.value)} />
                        {tooShort && (
                            <p className="text-xs text-(--error-light) mt-1">At least {MIN_PASSPHRASE} characters.</p>
                        )}
                    </div>
                    <div>
                        <label className="input-label mb-1 block" htmlFor="pb-pass-2">Repeat</label>
                        <input id="pb-pass-2" type="password" className="input w-full" value={repeat}
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
                            <button type="button" className="btn btn-sm" onClick={() => { setReplacing(false); setValue(''); setRepeat(''); }}>
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
        <div className="mt-4 border border-(--base-04) rounded-md p-4 space-y-4 bg-(--base-02)">
            <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                <div>
                    <label className="input-label mb-1 block" htmlFor="pb-name">Name</label>
                    <input id="pb-name" className="input w-full" value={draft.name}
                        onChange={e => onChange({ ...draft, name: e.target.value })} />
                </div>
                <div>
                    <label className="input-label mb-1 block" htmlFor="pb-schedule">Schedule</label>
                    <input id="pb-schedule" className="input w-full" value={draft.schedule}
                        placeholder="manual, every 24h, every 7d"
                        onChange={e => onChange({ ...draft, schedule: e.target.value })} />
                </div>
                <div>
                    <label className="input-label mb-1 block" htmlFor="pb-storage">Destination</label>
                    <select id="pb-storage" className="input w-full" value={draft.storageId ?? ''}
                        onChange={e => onChange({ ...draft, storageId: e.target.value ? Number(e.target.value) : null })}>
                        <option value="">Default storage</option>
                        {/* Already platform-only: a tenant's own storages live
                            behind /me/backup-storages and never appear here. */}
                        {storages.map(s => (
                            <option key={s.id} value={s.id}>{s.name}</option>
                        ))}
                    </select>
                </div>
                <div>
                    <label className="input-label mb-1 block" htmlFor="pb-retention">Keep</label>
                    <input id="pb-retention" type="number" min={1} className="input w-full" value={draft.retentionCount}
                        onChange={e => onChange({ ...draft, retentionCount: Math.max(1, Number(e.target.value) || 1) })} />
                </div>
            </div>

            <div>
                <p className="input-label mb-2">What to include</p>
                <div className="space-y-2">
                    {COMPONENTS.map(c => (
                        <label key={c.key} className="flex items-start gap-2.5 cursor-pointer">
                            <input type="checkbox" className="mt-0.5" checked={!!draft.selection[c.key]}
                                onChange={e => setSel({ [c.key]: e.target.checked } as Partial<PlatformBackupSelection>)} />
                            <span className="min-w-0">
                                <span className="text-sm text-(--base-09)">{c.label}</span>
                                <span className="block text-xs text-(--base-06)">{c.hint}</span>
                            </span>
                        </label>
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
                <div className="flex flex-wrap gap-2 mb-2">
                    {(['none', 'all', 'owner', 'list'] as PlatformBackupServerMode[]).map(m => (
                        <button key={m} type="button" onClick={() => setMode(m)}
                            className={`btn btn-sm ${draft.selection.servers.mode === m ? 'btn-primary' : ''}`}>
                            {m === 'none' ? 'None' : m === 'all' ? 'All servers' : m === 'owner' ? 'By owner' : 'Pick individually'}
                        </button>
                    ))}
                    {/* Only meaningful where BYON tenants exist. A self-hoster has none
                        to tell apart, so the filter would be over an empty distinction. */}
                    {byonOffered && (
                        <button type="button" onClick={() => setMode('byon')}
                            className={`btn btn-sm ${draft.selection.servers.mode === 'byon' ? 'btn-primary' : ''}`}>
                            All BYON servers
                        </button>
                    )}
                </div>

                {draft.selection.servers.mode === 'owner' && (
                    <select className="input w-full" value={draft.selection.servers.ownerId || ''}
                        onChange={e => onChange({
                            ...draft,
                            selection: { ...draft.selection, servers: { ...draft.selection.servers, ownerId: e.target.value || undefined } },
                        })}>
                        <option value="">Pick a user</option>
                        {owners.map(([id, count]) => (
                            <option key={id} value={id}>{id} ({count} server{count === 1 ? '' : 's'})</option>
                        ))}
                    </select>
                )}

                {draft.selection.servers.mode === 'list' && (
                    <div className="space-y-2">
                        <input className="input w-full" placeholder="Filter by name or UUID" value={filter}
                            onChange={e => setFilter(e.target.value)} />
                        <div className="max-h-56 overflow-y-auto border border-(--base-04) rounded-md divide-y divide-(--base-03)">
                            {visible.length === 0 && (
                                <p className="text-xs text-(--base-06) p-3">No server matches.</p>
                            )}
                            {visible.map(t => (
                                <label key={t.id} className="flex items-center gap-2.5 px-3 py-2 cursor-pointer hover:bg-(--base-03)">
                                    <input type="checkbox"
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

            <label className="flex items-center gap-2.5 cursor-pointer">
                <input type="checkbox" checked={draft.enabled}
                    onChange={e => onChange({ ...draft, enabled: e.target.checked })} />
                <span className="text-sm text-(--base-09)">Enabled</span>
            </label>

            {problem && <p className="text-xs text-(--error-light)">{problem}</p>}

            <div className="flex gap-2">
                <button type="button" className="btn btn-primary btn-sm" disabled={saving || !!problem} onClick={onSave}>
                    {saving ? 'Saving...' : 'Save job'}
                </button>
                <button type="button" className="btn btn-sm" onClick={onCancel}>Cancel</button>
            </div>
        </div>
    );
}

// ───────────── Runs ─────────────

function RunHistory({ runs }: { runs?: PlatformBackupRun[] }) {
    if (!runs) return <div className="px-3 pb-3"><SkeletonList rows={2} /></div>;
    if (runs.length === 0) {
        return <p className="px-3 pb-3 text-xs text-(--base-06)">This job has not run yet.</p>;
    }
    return (
        <div className="border-t border-(--base-03) divide-y divide-(--base-03)">
            {runs.map(run => (
                <div key={run.id} className="px-3 py-2.5">
                    <div className="flex items-center gap-3">
                        <span className={`mono-label shrink-0 ${
                            run.status === 'success' ? 'text-(--success-light)'
                                : run.status === 'failed' ? 'text-(--error-light)' : 'text-(--base-06)'
                        }`}>{run.status}</span>
                        <span className="text-xs text-(--base-07) flex-1 min-w-0 truncate">
                            {formatWhen(run.startedAt)} &middot; {formatSize(run.sizeBytes)}
                        </span>
                        {run.status === 'success' && run.storageKey && (
                            <a className="btn btn-sm flex items-center gap-1.5" href={platformBackupDownloadUrl(run.id)}>
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
    if (sel.library) parts.push('library');
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
