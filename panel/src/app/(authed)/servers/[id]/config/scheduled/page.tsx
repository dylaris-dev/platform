"use client";

import React, { useCallback, useEffect, useMemo, useState } from 'react';

import { Clock, Plus, Pencil, Trash2, Play, Square, X, RotateCcw, AlertTriangle, CheckCircle2, MessageSquare, RefreshCw, SkipForward } from 'lucide-react';
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
import ModalPanel from '@/components/ui/ModalPanel';

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
    { label: 'Daily at 04:00 UTC',                         cron: '0 4 * * *' },
    { label: 'Daily at 06:00 UTC',                         cron: '0 6 * * *' },
    { label: 'Daily at 12:00 UTC (noon)',                  cron: '0 12 * * *' },
    { label: 'Weekly — Sunday at 03:00 UTC',               cron: '0 3 * * 0' },
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
        const d = new Date(iso);
        return d.toLocaleString(undefined, {
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
    const [deletingTask, runDelete] = useBusy();
    const [loading, setLoading] = useState(true);
    const [editing, setEditing] = useState<EditingTask | null>(null);
    const [deletePrompt, setDeletePrompt] = useState<ScheduledTask | null>(null);

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
        const v = await validateCron(cron);
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

    const handleDelete = async () => {
        if (!deletePrompt) return;
        const res = await deleteScheduledTask(serverId, deletePrompt.id);
        if (res.success) {
            setDeletePrompt(null);
            showToast('Task deleted.', true);
            refresh();
        } else {
            showToast(res.message || 'Delete failed', false);
        }
    };

    if (!server) return null;

    return (
        <div className="max-w-3xl space-y-4">
            <header className="flex items-center gap-3 justify-between">
                <div className="flex items-start gap-3">
                    <div className="w-9 h-9 rounded-md bg-(--accent-ghost) flex items-center justify-center shrink-0">
                        <Clock size={16} className="text-(--accent-light)" />
                    </div>
                    <div>
                        <h2 className="text-base font-display font-semibold text-(--base-09)">Scheduled Tasks</h2>
                        <p className="text-xs text-(--base-06) mt-0.5">
                            Cron jobs that fire against this server. All times are UTC.
                            Restart tasks re-launch the container; broadcasts run
                            <code className="font-mono mx-1">say &lt;message&gt;</code> in the server console.
                        </p>
                    </div>
                </div>
                <button onClick={openCreate} className="btn btn-primary btn-sm">
                    <Plus size={13} />
                    New Task
                </button>
            </header>

            {loading ? (
                <div className="space-y-2">
                    {Array.from({ length: 3 }).map((_, i) => (
                        <article key={i} className="card p-3 flex items-start gap-3">
                            <Skeleton className="w-9 h-9 rounded-md shrink-0" />
                            <div className="min-w-0 flex-1 space-y-2">
                                <div className="flex items-center gap-2">
                                    <SkeletonText width="w-32" className="h-3.5" />
                                    <Skeleton className="w-12 h-3 rounded-sm" />
                                </div>
                                <div className="grid grid-cols-1 sm:grid-cols-3 gap-x-4 gap-y-1">
                                    <SkeletonText width="w-24" className="h-2.5" />
                                    <SkeletonText width="w-28" className="h-2.5" />
                                    <SkeletonText width="w-24" className="h-2.5" />
                                </div>
                            </div>
                            <div className="flex items-center gap-1 shrink-0">
                                <Skeleton className="w-7 h-7 rounded-md" />
                                <Skeleton className="w-7 h-7 rounded-md" />
                                <Skeleton className="w-7 h-7 rounded-md" />
                            </div>
                        </article>
                    ))}
                </div>
            ) : tasks.length === 0 ? (
                <div className="card p-8 flex flex-col items-center text-center gap-2">
                    <Clock size={28} className="text-(--base-05)" />
                    <p className="text-sm text-(--base-07)">No scheduled tasks yet.</p>
                    <p className="text-xs text-(--base-06)">Common: daily restart at 04:00, hourly "10 min to restart" broadcast.</p>
                </div>
            ) : (
                <div className="space-y-2">
                    {tasks.map(t => (
                        <article key={t.id} className="card p-3 flex items-start gap-3">
                            {/* Type icon */}
                            <div className={`w-9 h-9 rounded-md flex items-center justify-center shrink-0 ${
                                t.taskType === 'restart' ? 'bg-(--warning-ghost) text-(--warning-light)' : 'bg-(--accent-ghost) text-(--accent-light)'
                            }`}>
                                {t.taskType === 'restart' ? <RotateCcw size={16} /> : <MessageSquare size={16} />}
                            </div>
                            <div className="min-w-0 flex-1">
                                <div className="flex items-center gap-2 flex-wrap">
                                    <span className="font-medium text-sm text-(--base-09)">{t.name || TASK_TYPE_LABEL[t.taskType]}</span>
                                    <span className="mono-label bg-(--base-03) px-1.5 rounded-sm text-(--base-07)">{t.taskType}</span>
                                    {!t.enabled && (
                                        <span className="mono-label bg-(--base-03) px-1.5 rounded-sm text-(--base-06)">disabled</span>
                                    )}
                                    {t.lastStatus === 'error' && (
                                        <span className="mono-label bg-(--error-ghost) px-1.5 rounded-sm text-(--error-light) flex items-center gap-1" title={t.lastError || ''}>
                                            <AlertTriangle size={9} />
                                            error
                                        </span>
                                    )}
                                    {t.lastStatus === 'skipped' && (
                                        <span className="mono-label bg-(--base-03) px-1.5 rounded-sm text-(--base-07) flex items-center gap-1" title={t.lastError || ''}>
                                            <SkipForward size={9} />
                                            skipped
                                        </span>
                                    )}
                                    {t.lastStatus === 'ok' && (
                                        <span className="mono-label bg-(--success-ghost) px-1.5 rounded-sm text-(--success-light) flex items-center gap-1">
                                            <CheckCircle2 size={9} />
                                            last ok
                                        </span>
                                    )}
                                </div>
                                <div className="mt-1 grid grid-cols-1 sm:grid-cols-3 gap-x-4 gap-y-0.5 text-xs">
                                    <div>
                                        <span className="text-(--base-06)">Schedule</span>{' '}
                                        <code className="font-mono text-(--base-08)">{t.scheduleCron}</code>
                                    </div>
                                    <div>
                                        <span className="text-(--base-06)">Next</span>{' '}
                                        <span className="text-(--base-08)">{formatDate(t.nextRun)}</span>
                                        {t.nextRun && (
                                            <span className="text-(--base-05)"> ({relativeFromNow(t.nextRun)})</span>
                                        )}
                                    </div>
                                    <div>
                                        <span className="text-(--base-06)">Last</span>{' '}
                                        <span className="text-(--base-08)">{formatDate(t.lastRun)}</span>
                                    </div>
                                </div>
                                {t.taskType === 'say' && t.payload && (
                                    <div className="mt-1 text-xs text-(--base-07) font-mono">
                                        &ldquo;{t.payload}&rdquo;
                                    </div>
                                )}
                            </div>
                            <div className="flex items-center gap-1 shrink-0">
                                <button
                                    onClick={() => handleToggleEnabled(t)}
                                    className="btn btn-secondary btn-sm"
                                    title={t.enabled ? 'Disable task' : 'Enable task'}
                                >
                                    {t.enabled ? <Square size={12} /> : <Play size={12} />}
                                </button>
                                <button onClick={() => openEdit(t)} className="btn btn-secondary btn-sm" title="Edit task">
                                    <Pencil size={12} />
                                </button>
                                <button onClick={() => setDeletePrompt(t)} className="btn btn-secondary btn-sm" title="Delete task">
                                    <Trash2 size={12} className="text-(--error)" />
                                </button>
                            </div>
                        </article>
                    ))}
                </div>
            )}

            {/* Create/Edit modal */}
            {editing && (
                <TaskEditor
                    editing={editing}
                    setEditing={setEditing}
                    onSave={handleSave}
                    onClose={() => setEditing(null)}
                />
            )}

            {/* Delete confirmation */}
            {deletePrompt && (
                <div className="modal-overlay animate-fade-in" onClick={() => setDeletePrompt(null)}>
                    <ModalPanel onClose={() => setDeletePrompt(null)} className="modal-panel max-w-sm" onClick={e => e.stopPropagation()}>
                        <div className="modal-header">
                            <h3 className="modal-title flex items-center gap-2 text-(--error-light)">
                                <AlertTriangle size={20} />
                                Delete scheduled task?
                            </h3>
                        </div>
                        <div className="modal-body">
                            <p className="text-sm text-(--base-07)">
                                <span className="font-semibold text-(--base-09)">{deletePrompt.name || TASK_TYPE_LABEL[deletePrompt.taskType]}</span>
                                {' '}will stop firing. Audit log is preserved.
                            </p>
                        </div>
                        <div className="modal-footer">
                            <button onClick={() => setDeletePrompt(null)} className="btn btn-secondary">Cancel</button>
                            <button onClick={() => runDelete(handleDelete)} disabled={deletingTask} className="btn btn-danger disabled:opacity-40">Delete</button>
                        </div>
                    </ModalPanel>
                </div>
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
            const res = await validateCron(activeCron);
            if (!cancelled) {
                setPreview({ valid: !!res.valid, nextRun: res.nextRun, error: res.error });
                setPreviewLoading(false);
            }
        }, 250);
        return () => { cancelled = true; clearTimeout(id); setPreviewLoading(false); };
    }, [activeCron]);

    return (
        <div className="modal-overlay animate-fade-in" onClick={onClose}>
            <ModalPanel onClose={onClose} className="modal-panel w-full max-w-lg" onClick={e => e.stopPropagation()}>
                <div className="modal-header">
                    <h3 className="modal-title flex items-center gap-2">
                        <Clock size={18} />
                        {editing.isNew ? 'New scheduled task' : 'Edit scheduled task'}
                    </h3>
                    <button onClick={onClose} className="text-(--base-06) hover:text-(--base-09)" aria-label="Close">
                        <X size={18} />
                    </button>
                </div>
                <div className="modal-body space-y-4">
                    {/* Task type */}
                    <div>
                        <label className="input-label">Task Type</label>
                        <div className="grid grid-cols-2 gap-2 mt-1">
                            {(['restart', 'say'] as ScheduledTaskType[]).map(t => (
                                <button
                                    key={t}
                                    type="button"
                                    onClick={() => setEditing({ ...editing, taskType: t })}
                                    className={`flex items-center gap-2 px-3 py-2 rounded-md border text-sm transition-colors text-left ${
                                        editing.taskType === t
                                            ? 'border-(--accent) bg-(--accent-ghost) text-(--accent-light)'
                                            : 'border-(--base-04) text-(--base-07) hover:bg-(--base-03)'
                                    }`}
                                >
                                    {t === 'restart' ? <RotateCcw size={14} /> : <MessageSquare size={14} />}
                                    {TASK_TYPE_LABEL[t]}
                                </button>
                            ))}
                        </div>
                    </div>

                    {/* Name */}
                    <div>
                        <label className="input-label">Name (optional)</label>
                        <input
                            type="text"
                            value={editing.name || ''}
                            onChange={e => setEditing({ ...editing, name: e.target.value })}
                            placeholder={TASK_TYPE_LABEL[editing.taskType as ScheduledTaskType]}
                            className="input-field w-full"
                            maxLength={128}
                        />
                    </div>

                    {/* Payload (only for say) */}
                    {editing.taskType === 'say' && (
                        <div>
                            <label className="input-label">Broadcast message</label>
                            <input
                                type="text"
                                value={editing.payload || ''}
                                onChange={e => setEditing({ ...editing, payload: e.target.value })}
                                placeholder="Server restarting in 10 minutes!"
                                className="input-field w-full"
                                maxLength={256}
                            />
                            <p className="text-xs text-(--base-06) mt-1">Sent via <code className="font-mono">say</code> — visible to all online players.</p>
                        </div>
                    )}

                    {/* Schedule — preset OR custom */}
                    <div>
                        <div className="flex items-center justify-between mb-1">
                            <label className="input-label">Schedule</label>
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
                                className="text-[10px] font-mono uppercase tracking-[0.08em] text-(--base-06) hover:text-(--accent-light)"
                            >
                                {customMode ? 'Use preset' : 'Custom cron'}
                            </button>
                        </div>
                        {!customMode ? (
                            <select
                                value={editing.presetCron || ''}
                                onChange={e => setEditing({ ...editing, presetCron: e.target.value })}
                                className="input-field w-full"
                            >
                                {CRON_PRESETS.map(p => (
                                    <option key={p.cron} value={p.cron}>{p.label}</option>
                                ))}
                            </select>
                        ) : (
                            <input
                                type="text"
                                value={editing.customCron || ''}
                                onChange={e => setEditing({ ...editing, customCron: e.target.value })}
                                placeholder="0 4 * * *"
                                className="input-field input-mono w-full"
                                autoComplete="off"
                                spellCheck={false}
                            />
                        )}

                        {/* Live preview of next run */}
                        <div className="mt-1 text-xs flex items-center gap-1.5 min-h-[18px]">
                            {previewLoading && <RefreshCw size={11} className="animate-spin text-(--base-05)" />}
                            {!previewLoading && preview?.valid && preview.nextRun && (
                                <span className="text-(--base-06)">
                                    Next run: <span className="text-(--accent-light) font-mono">{formatDate(preview.nextRun)}</span>
                                </span>
                            )}
                            {!previewLoading && preview && !preview.valid && (
                                <span className="text-(--error-light) flex items-center gap-1">
                                    <AlertTriangle size={11} />
                                    {preview.error || 'Invalid cron'}
                                </span>
                            )}
                            {customMode && !preview && !previewLoading && (
                                <span className="text-(--base-05) font-mono">format: min hour dom mon dow</span>
                            )}
                        </div>
                    </div>

                    {/* Enabled toggle */}
                    <div className="flex items-center justify-between border-t border-(--base-03) pt-3">
                        <div>
                            <div className="text-sm font-medium text-(--base-09)">Enabled</div>
                            <p className="text-xs text-(--base-06)">Disabled tasks stay in the list but don&apos;t fire.</p>
                        </div>
                        <button
                            type="button"
                            role="switch"
                            aria-checked={editing.enabled ?? true}
                            onClick={() => setEditing({ ...editing, enabled: !(editing.enabled ?? true) })}
                            className={`toggle-track ${(editing.enabled ?? true) ? 'toggle-track-on' : 'toggle-track-off'}`}
                        >
                            <span className={`toggle-knob ${(editing.enabled ?? true) ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                        </button>
                    </div>
                </div>
                <div className="modal-footer">
                    <button onClick={onClose} className="btn btn-secondary">Cancel</button>
                    <button
                        onClick={onSave}
                        className="btn btn-primary"
                        disabled={!!preview && !preview.valid}
                    >
                        {editing.isNew ? 'Create task' : 'Save changes'}
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
