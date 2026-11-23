"use client";

import React, { useCallback, useEffect, useMemo, useState } from 'react';

import { Clock, Plus, Pencil, Trash2, MoreHorizontal, X, RotateCcw, AlertTriangle, CheckCircle2, MessageSquare, RefreshCw, SkipForward } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { systemEvents } from '@/lib/systemEvents';
import {
    listScheduledTasks, createScheduledTask, updateScheduledTask, deleteScheduledTask, validateCron,
    type ScheduledTask, type ScheduledTaskType, type ScheduledTaskInput,
} from '@/lib/api/scheduledTasks';
import { Skeleton, SkeletonText } from '@/components/Skeleton';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';
import Menu from '@/components/ui/Menu';
import Segmented from '@/components/ui/Segmented';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { Input } from '@ui-sdk/react/input';
import { Dialog, DialogBackdrop, DialogClose, DialogContent, DialogDescription, DialogTitle } from '@ui-sdk/react/dialog';
import Select from '@/components/ui/Select';
import Switch from '@/components/ui/Switch';
import TimeZoneSelect from '@/components/ui/TimeZoneSelect';
import { fmtDateTime, getServerTimezone, setServerTimezone } from '@/lib/time';

// Scheduled Tasks sub-tab. Per-server cron jobs (restart, say).
// Presets cover the 90% of operator wishes (daily restart at 4 AM, "10
// minutes to restart" message, hourly broadcast); the Custom toggle exposes
// raw 5-field cron for the rest. Server-side `validateCron` previews the
// next firing time so users don't fly blind.

// Presets are keyed by the cron string we send; labels are what the user
// sees. Order chosen by frequency-of-actual-use, not alphabetical.
const CRON_PRESETS: { label: string; cron: string }[] = [
    { label: 'Every hour',                                 cron: '0 * * * *' },
    { label: 'Every 6 hours',                              cron: '0 */6 * * *' },
    { label: 'Every 12 hours',                             cron: '0 */12 * * *' },
    { label: 'Daily at 04:00',                             cron: '0 4 * * *' },
    { label: 'Daily at 06:00',                             cron: '0 6 * * *' },
    { label: 'Daily at 12:00 (noon)',                      cron: '0 12 * * *' },
    { label: 'Weekly, Sunday at 03:00',                    cron: '0 3 * * 0' },
];

const TASK_TYPE_LABEL: Record<ScheduledTaskType, string> = {
    restart: 'Restart server',
    say:     'Broadcast message (say)',
};

interface EditingTask extends Partial<ScheduledTask> {
    isNew?: boolean;
    presetCron?: string;        // selected preset cron, '' = custom
    customCron?: string;        // raw cron when Custom is on
}

function formatDate(iso: string | undefined): string {
    if (!iso) return '—';
    try {
        return fmtDateTime(iso, {
            year: 'numeric', month: '2-digit', day: '2-digit',
            hour: '2-digit', minute: '2-digit', timeZoneName: 'short',
        });
    } catch { return iso; }
}

function relativeFromNow(iso: string | undefined): string {
    if (!iso) return '';
    const diff = new Date(iso).getTime() - Date.now();
    if (diff <= 0) return 'due now';
    const min = Math.floor(diff / 60_000);
    if (min < 60) return `in ${min}m`;
    const hr = Math.floor(min / 60);
    if (hr < 24) return `in ${hr}h ${min % 60}m`;
    const d = Math.floor(hr / 24);
    return `in ${d}d ${hr % 24}h`;
}

export default function ServerConfigScheduledPage() {
    const paramId = useRouteId('servers');
    const { servers } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);

    const [tasks, setTasks] = useState<ScheduledTask[]>([]);
    const [loading, setLoading] = useState(true);
    const [editing, setEditing] = useState<EditingTask | null>(null);
    // The zone a new task starts in: the server's own, else its owner's.
    const [serverZone, setServerZone] = useState('UTC');
    const [ownZone, setOwnZone] = useState<string | null>(null);
    const [savingZone, runSaveZone] = useBusy();
    const [deletingTask, runDelete] = useBusy();
    const loadZone = useCallback(async () => {
        if (!serverId) return;
        const r = await getServerTimezone(serverId);
        if (r) { setServerZone(r.effective || 'UTC'); setOwnZone(r.timezone); }
    }, [serverId]);
    useEffect(() => { loadZone(); }, [loadZone]);

    const changeZone = (tz: string) => runSaveZone(async () => {
        const res = await setServerTimezone(serverId, tz);
        if (res.success) {
            toast('Time zone saved. The console uses it from the next start.', true);
            loadZone();
        } else {
            toast(res.message || 'Could not save the time zone', false);
        }
    });

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        if (!serverId) return;
        const res = await listScheduledTasks(serverId);
        if (res.success && res.tasks) setTasks(res.tasks);
        setLoading(false);
    }, [serverId]);

    useEffect(() => { refresh(); }, [refresh]);

    // Refresh on `scheduled_tasks.changed` so executor-driven changes show up live.
    useEffect(() => {
        const unsub = systemEvents.on('scheduled_tasks.changed', () => { refresh(); });
        return () => { unsub(); };
    }, [refresh]);

    const openCreate = () => {
        setEditing({
            isNew: true,
            name: '',
            taskType: 'restart',
            scheduleCron: CRON_PRESETS[3].cron,
            timezone: serverZone,
            payload: '',
            enabled: true,
            presetCron: CRON_PRESETS[3].cron,
            customCron: '',
        });
    };

    const openEdit = (t: ScheduledTask) => {
        const matchingPreset = CRON_PRESETS.find(p => p.cron === t.scheduleCron);
        setEditing({
            ...t,
            presetCron: matchingPreset ? matchingPreset.cron : '',
            customCron: matchingPreset ? '' : t.scheduleCron,
        });
    };

    const handleSave = async () => {
        if (!editing) return;
        const cron = (editing.presetCron && editing.presetCron !== 'custom')
            ? editing.presetCron
            : (editing.customCron || '').trim();
        if (!cron) {
            showToast('Choose a preset or enter a custom cron', false);
            return;
        }
        if (editing.taskType === 'say' && !(editing.payload || '').trim()) {
            showToast('Broadcast message can\'t be empty', false);
            return;
        }
        // Pre-validate via backend so the user gets a clean error instead of
        // a generic "Failed to create" if the cron is malformed.
        const tz = editing.timezone || serverZone;
        const v = await validateCron(cron, tz);
        if (!v.valid) {
            showToast(`Invalid cron: ${v.error || 'parse error'}`, false);
            return;
        }
        const input: ScheduledTaskInput = {
            name: (editing.name || '').trim() || TASK_TYPE_LABEL[editing.taskType as ScheduledTaskType],
            taskType: editing.taskType as ScheduledTaskType,
            scheduleCron: cron,
            payload: editing.taskType === 'say' ? (editing.payload || '').trim() : '',
            enabled: editing.enabled ?? true,
            timezone: tz,
        };
        const res = editing.isNew
            ? await createScheduledTask(serverId, input)
            : await updateScheduledTask(serverId, editing.id!, input);
        if (res.success) {
            setEditing(null);
            showToast(editing.isNew ? 'Task created.' : 'Task saved.', true);
            refresh();
        } else {
            showToast(res.message || 'Save failed', false);
        }
    };

    const handleToggleEnabled = async (t: ScheduledTask) => {
        const res = await updateScheduledTask(serverId, t.id, { enabled: !t.enabled, taskType: t.taskType, scheduleCron: t.scheduleCron });
        if (res.success) refresh(); else showToast(res.message || 'Toggle failed', false);
    };

    const handleDelete = async (t: ScheduledTask) => {
        if (!(await confirmDialog({
            title: 'Delete scheduled task?',
            message: `${t.name || TASK_TYPE_LABEL[t.taskType]} will stop firing. Audit log is preserved.`,
        }))) return;
        const res = await deleteScheduledTask(serverId, t.id);
        if (res.success) {
            showToast('Task deleted.', true);
            refresh();
        } else {
            showToast(res.message || 'Delete failed', false);
        }
    };

    if (!server) return null;

    return (
        <div className="flex flex-col gap-6">
            <div className="srv-config-head">
                <div className="min-w-0">
                    <h1 className="srv-config-title">Scheduled tasks</h1>
                    <p className="srv-config-desc max-w-2xl">
                        Cron jobs that fire against this server, each in its own time zone.
                        Restart tasks re-launch the container; broadcasts run
                        <code className="srv-num mx-1 text-(--base-08)">say &lt;message&gt;</code> in the server console.
                    </p>
                </div>
                <button type="button" onClick={openCreate} className="btn btn-primary btn-sm">
                    <Plus size={14} />
                    New task
                </button>
            </div>

            <section className="srv-surface flex flex-col gap-3 p-5 sm:flex-row sm:items-center" aria-label="Server time zone">
                <div className="min-w-0 flex-1">
                    <label htmlFor="server-tz" className="text-sm font-medium text-(--base-09)">Server time zone</label>
                    <p className="mt-1 text-xs text-(--base-06)">
                        The console&apos;s timestamps (from the next start) and the zone new tasks start in.
                    </p>
                </div>
                <div className="shrink-0 sm:w-64">
                    {ownZone === null ? (
                        <Skeleton className="h-9 w-full rounded-md" />
                    ) : (
                        <TimeZoneSelect
                            id="server-tz"
                            value={ownZone}
                            onChange={changeZone}
                            disabled={savingZone}
                            inheritLabel={`Same as the owner's account${!ownZone ? ` (${serverZone.replace(/_/g, ' ')})` : ''}`}
                        />
                    )}
                </div>
            </section>

            {loading ? (
                <div className="srv-surface overflow-hidden" aria-busy="true" aria-label="Loading scheduled tasks">
                    {Array.from({ length: 3 }).map((_, i) => (
                        <div key={i} className="srv-config-item">
                            <Skeleton className="h-9 w-9 shrink-0 rounded-md" />
                            <div className="min-w-0 flex-1 space-y-2">
                                <SkeletonText width="w-40" className="h-3.5" />
                                <SkeletonText width="w-64" className="h-2.5" />
                            </div>
                            <Skeleton className="h-6 w-10 shrink-0 rounded-full" />
                        </div>
                    ))}
                </div>
            ) : tasks.length === 0 ? (
                <div className="srv-surface flex flex-col items-center gap-2 px-6 py-14 text-center">
                    <Clock size={32} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">No scheduled tasks yet.</p>
                    <p className="text-xs text-(--base-06)">Common: daily restart at 04:00, hourly &ldquo;10 min to restart&rdquo; broadcast.</p>
                    <button type="button" onClick={openCreate} className="btn btn-secondary btn-sm mt-2">
                        <Plus size={14} />
                        New task
                    </button>
                </div>
            ) : (
                <ul className="srv-surface overflow-hidden">
                    {tasks.map(t => (
                        <li key={t.id} className="srv-config-item">
                            <div className={`flex h-9 w-9 shrink-0 items-center justify-center rounded-md ${
                                t.taskType === 'restart' ? 'bg-(--warning-ghost) text-(--warning-light)' : 'bg-(--base-03) text-(--base-07)'
                            }`}>
                                {t.taskType === 'restart' ? <RotateCcw size={16} /> : <MessageSquare size={16} />}
                            </div>
                            <div className={`min-w-0 flex-1 ${t.enabled ? '' : 'opacity-60'}`}>
                                <div className="flex flex-wrap items-center gap-2">
                                    <span className="text-sm font-medium text-(--base-09)">{t.name || TASK_TYPE_LABEL[t.taskType]}</span>
                                    <span className="srv-config-pill">{t.taskType}</span>
                                    {t.lastStatus === 'error' && (
                                        <span className="srv-config-pill inline-flex items-center gap-1 bg-(--error-ghost) text-(--error-light)" title={t.lastError || ''}>
                                            <AlertTriangle size={12} />
                                            Error
                                        </span>
                                    )}
                                    {t.lastStatus === 'skipped' && (
                                        <span className="srv-config-pill inline-flex items-center gap-1" title={t.lastError || ''}>
                                            <SkipForward size={12} />
                                            Skipped
                                        </span>
                                    )}
                                    {t.lastStatus === 'ok' && (
                                        <span className="srv-config-pill inline-flex items-center gap-1 bg-(--success-ghost) text-(--success-light)">
                                            <CheckCircle2 size={12} />
                                            Last run ok
                                        </span>
                                    )}
                                </div>
                                <dl className="mt-2 grid grid-cols-1 gap-x-6 gap-y-1 text-xs sm:grid-cols-[auto_auto_auto] sm:justify-start">
                                    <div className="flex gap-2">
                                        <dt className="text-(--base-06)">Schedule</dt>
                                        <dd><code className="srv-num text-(--base-08)">{t.scheduleCron}</code> <span className="text-(--base-05)">{t.timezone || 'UTC'}</span></dd>
                                    </div>
                                    <div className="flex gap-2">
                                        <dt className="text-(--base-06)">Next run</dt>
                                        <dd className="text-(--base-08)">
                                            {formatDate(t.nextRun)}
                                            {t.nextRun && <span className="text-(--base-05)"> ({relativeFromNow(t.nextRun)})</span>}
                                        </dd>
                                    </div>
                                    <div className="flex gap-2">
                                        <dt className="text-(--base-06)">Last run</dt>
                                        <dd className="text-(--base-08)">{formatDate(t.lastRun)}</dd>
                                    </div>
                                </dl>
                                {t.taskType === 'say' && t.payload && (
                                    <p className="srv-num mt-2 text-xs text-(--base-07)">&ldquo;{t.payload}&rdquo;</p>
                                )}
                            </div>
                            <div className="flex shrink-0 items-center gap-2">
                                <Switch checked={t.enabled} onChange={() => handleToggleEnabled(t)} ariaLabel={`${t.enabled ? 'Disable' : 'Enable'} ${t.name || TASK_TYPE_LABEL[t.taskType]}`} />
                                <Menu
                                    trigger={
                                        <button type="button" aria-label={`Actions for ${t.name || TASK_TYPE_LABEL[t.taskType]}`} className="btn btn-ghost btn-icon btn-sm">
                                            <MoreHorizontal size={16} />
                                        </button>
                                    }
                                    items={[
                                        { label: 'Edit', icon: <Pencil size={14} />, onSelect: () => openEdit(t) },
                                        { type: 'separator' },
                                        { label: 'Delete', icon: <Trash2 size={14} />, danger: true, disabled: deletingTask, onSelect: () => runDelete(() => handleDelete(t)) },
                                    ]}
                                />
                            </div>
                        </li>
                    ))}
                </ul>
            )}

            {editing && (
                <TaskEditor
                    editing={editing}
                    setEditing={setEditing}
                    onSave={handleSave}
                    onClose={() => setEditing(null)}
                />
            )}
        </div>
    );
}

// ---- Editor sub-component ----

interface TaskEditorProps {
    editing: EditingTask;
    setEditing: (next: EditingTask | null) => void;
    onSave: () => void;
    onClose: () => void;
}

function TaskEditor({ editing, setEditing, onSave, onClose }: TaskEditorProps) {
    const [customMode, setCustomMode] = useState(!editing.presetCron);
    const [previewLoading, setPreviewLoading] = useState(false);
    const [preview, setPreview] = useState<{ valid: boolean; nextRun?: string; error?: string } | null>(null);

    const activeCron = useMemo(() => {
        if (!customMode) return editing.presetCron || '';
        return (editing.customCron || '').trim();
    }, [customMode, editing.presetCron, editing.customCron]);

    useEffect(() => {
        if (!activeCron) { setPreview(null); return; }
        let cancelled = false;
        setPreviewLoading(true);
        const id = setTimeout(async () => {
            const res = await validateCron(activeCron, editing.timezone || 'UTC');
            if (!cancelled) {
                setPreview({ valid: !!res.valid, nextRun: res.nextRun, error: res.error });
                setPreviewLoading(false);
            }
        }, 250);
        return () => { cancelled = true; clearTimeout(id); setPreviewLoading(false); };
    }, [activeCron, editing.timezone]);

    return (
        <Dialog open onOpenChange={open => { if (!open) onClose(); }}>
            <DialogBackdrop className="modal-overlay animate-fade-in" />
            <DialogContent className="modal-panel fixed inset-0 z-50 m-auto flex h-fit max-h-[calc(100dvh-2rem)] w-[calc(100%-2rem)] max-w-lg flex-col">
                <div className="modal-header flex items-start justify-between gap-4">
                    <div className="min-w-0">
                        <DialogTitle className="modal-title">{editing.isNew ? 'New scheduled task' : 'Edit scheduled task'}</DialogTitle>
                        <DialogDescription className="mt-1 text-xs text-(--base-06)">Runs on a cron schedule in the chosen time zone.</DialogDescription>
                    </div>
                    <DialogClose aria-label="Close" className="btn btn-ghost btn-icon btn-sm shrink-0">
                        <X size={16} />
                    </DialogClose>
                </div>
                <div className="modal-body min-h-0 flex-1 space-y-5 overflow-y-auto">
                    <div>
                        <span className="srv-label mb-1.5 block">Task type</span>
                        <Segmented<ScheduledTaskType>
                            ariaLabel="Task type"
                            value={editing.taskType as ScheduledTaskType}
                            onChange={t => setEditing({ ...editing, taskType: t })}
                            options={(['restart', 'say'] as ScheduledTaskType[]).map(t => ({ id: t, label: TASK_TYPE_LABEL[t] }))}
                        />
                    </div>

                    <div>
                        <label htmlFor="task-name" className="srv-label mb-1.5 block">Name (optional)</label>
                        <Input
                            id="task-name"
                            type="text"
                            value={editing.name || ''}
                            onChange={e => setEditing({ ...editing, name: e.target.value })}
                            placeholder={TASK_TYPE_LABEL[editing.taskType as ScheduledTaskType]}
                            className="input-field w-full"
                            maxLength={128}
                        />
                    </div>

                    {editing.taskType === 'say' && (
                        <div>
                            <label htmlFor="task-payload" className="srv-label mb-1.5 block">Broadcast message</label>
                            <Input
                                id="task-payload"
                                type="text"
                                value={editing.payload || ''}
                                onChange={e => setEditing({ ...editing, payload: e.target.value })}
                                placeholder="Server restarting in 10 minutes!"
                                className="input-field w-full"
                                maxLength={256}
                            />
                            <p className="mt-1.5 text-xs text-(--base-06)">Sent via <code className="srv-num">say</code> &mdash; visible to all online players.</p>
                        </div>
                    )}

                    <div>
                        <div className="mb-1.5 flex items-center justify-between">
                            <span className="srv-label">Schedule</span>
                            <button
                                type="button"
                                onClick={() => {
                                    setCustomMode(m => {
                                        const next = !m;
                                        if (next) {
                                            // Switching to custom: seed from current preset so the
                                            // user can tweak rather than start from blank.
                                            if (!editing.customCron && editing.presetCron) {
                                                setEditing({ ...editing, customCron: editing.presetCron });
                                            }
                                        }
                                        return next;
                                    });
                                }}
                                className="rounded text-xs text-(--base-06) transition-colors hover:text-(--accent-light) focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
                            >
                                {customMode ? 'Use preset' : 'Custom cron'}
                            </button>
                        </div>
                        {!customMode ? (
                            <Select
                                ariaLabel="Schedule preset"
                                value={editing.presetCron || ''}
                                onChange={v => setEditing({ ...editing, presetCron: v })}
                                options={CRON_PRESETS.map(p => ({ value: p.cron, label: p.label }))}
                            />
                        ) : (
                            <Input
                                type="text"
                                aria-label="Cron expression"
                                value={editing.customCron || ''}
                                onChange={e => setEditing({ ...editing, customCron: e.target.value })}
                                placeholder="0 4 * * *"
                                className="input-field input-mono w-full"
                                autoComplete="off"
                                spellCheck={false}
                            />
                        )}

                        <div className="mt-1.5 flex min-h-[18px] items-center gap-1.5 text-xs">
                            {previewLoading && <RefreshCw size={12} className="animate-spin text-(--base-05)" />}
                            {!previewLoading && preview?.valid && preview.nextRun && (
                                <span className="text-(--base-06)">
                                    Next run: <span className="srv-num text-(--base-08)">{formatDate(preview.nextRun)}</span>
                                </span>
                            )}
                            {!previewLoading && preview && !preview.valid && (
                                <span className="flex items-center gap-1 text-(--error-light)">
                                    <AlertTriangle size={12} />
                                    {preview.error || 'Invalid cron'}
                                </span>
                            )}
                            {customMode && !preview && !previewLoading && (
                                <span className="srv-num text-(--base-05)">format: min hour dom mon dow</span>
                            )}
                        </div>
                    </div>

                    <div>
                        <label htmlFor="task-tz" className="srv-label mb-1.5 block">Time zone</label>
                        <TimeZoneSelect id="task-tz" value={editing.timezone || 'UTC'} onChange={tz => setEditing({ ...editing, timezone: tz })} />
                        <p className="mt-1.5 text-xs text-(--base-06)">The schedule above is read in this zone, daylight saving included.</p>
                    </div>

                    <div className="flex items-center justify-between gap-4 rounded-lg bg-(--base-03) px-4 py-3">
                        <div>
                            <div className="text-sm font-medium text-(--base-09)">Enabled</div>
                            <p className="text-xs text-(--base-06)">Disabled tasks stay in the list but don&apos;t fire.</p>
                        </div>
                        <Switch checked={editing.enabled ?? true} onChange={v => setEditing({ ...editing, enabled: v })} ariaLabel="Enabled" />
                    </div>
                </div>
                <div className="modal-footer">
                    <DialogClose className="btn btn-secondary">Cancel</DialogClose>
                    <button
                        type="button"
                        onClick={onSave}
                        className="btn btn-primary"
                        disabled={!!preview && !preview.valid}
                    >
                        {editing.isNew ? 'Create task' : 'Save changes'}
                    </button>
                </div>
            </DialogContent>
        </Dialog>
    );
}
