"use client";

import { useEffect, useState } from 'react';
import { CircleAlert } from 'lucide-react';
import Select from '@/components/ui/Select';
import TimeZoneSelect from '@/components/ui/TimeZoneSelect';
import { previewBackupSchedule } from '@/lib/api';
import {
    ANCHOR_STEPS, WEEKDAYS, anchoredTimes, buildSchedule, fmtClock, parseSchedule, usesTimeZone,
    type ScheduleDraft, type ScheduleKind,
} from '@/lib/backupSchedule';
import { fmtDateTime } from '@/lib/time';

const KINDS: { value: ScheduleKind; label: string }[] = [
    { value: 'manual', label: 'Manual only' },
    { value: 'interval', label: 'Every few hours or days' },
    { value: 'anchored', label: 'Every few hours, at fixed times' },
    { value: 'daily', label: 'Daily' },
    { value: 'weekly', label: 'Weekly' },
    { value: 'cron', label: 'Cron expression' },
];

const RUN_FORMAT: Intl.DateTimeFormatOptions = { weekday: 'short', day: 'numeric', month: 'short', hour: 'numeric', minute: '2-digit' };

interface Preview { valid: boolean; runs: string[]; error?: string }

/**
 * The one schedule editor for server and platform backup jobs. It edits the
 * stored string and the job's zone, and asks Core for the next runs rather than
 * computing them here, so the preview and the scheduler cannot disagree.
 */
export default function BackupScheduleEditor({ schedule, timezone, onChange, idPrefix }: {
    schedule: string;
    timezone: string;
    onChange: (schedule: string, timezone: string) => void;
    idPrefix: string;
}) {
    const [draft, setDraft] = useState<ScheduleDraft>(() => parseSchedule(schedule));
    const [preview, setPreview] = useState<Preview | null>(null);
    const [stale, setStale] = useState(false);
    const built = buildSchedule(draft);

    const update = (patch: Partial<ScheduleDraft>) => {
        const next = { ...draft, ...patch };
        if (next.kind === 'anchored' && !ANCHOR_STEPS.includes(next.every)) next.every = 6;
        setDraft(next);
        onChange(buildSchedule(next), timezone);
    };

    useEffect(() => {
        if (draft.kind === 'manual') { setPreview(null); return; }
        let cancelled = false;
        setStale(true);
        // Debounced: the cron field asks on every keystroke otherwise.
        const t = setTimeout(async () => {
            const res = await previewBackupSchedule(built, timezone);
            if (cancelled) return;
            setStale(false);
            if (!res.success) setPreview({ valid: false, runs: [], error: res.message || res.error || 'Could not preview this schedule.' });
            else setPreview({ valid: !!res.valid, runs: res.nextRuns ?? [], error: res.error });
        }, 300);
        return () => { cancelled = true; clearTimeout(t); };
    }, [built, timezone, draft.kind]);

    const timeInput = (label: string) => (
        <div className="form-group">
            <label className="input-label" htmlFor={`${idPrefix}-time`}>{label}</label>
            <input id={`${idPrefix}-time`} type="time" value={draft.time} required
                onChange={e => e.target.value && update({ time: e.target.value })} className="input-field tabular-nums" />
        </div>
    );

    return (
        <div className="space-y-3">
            <div className="form-group">
                <label className="input-label" htmlFor={`${idPrefix}-kind`}>Schedule</label>
                <Select id={`${idPrefix}-kind`} value={draft.kind} onChange={v => update({ kind: v as ScheduleKind })} options={KINDS} ariaLabel="Schedule" />
            </div>

            {draft.kind === 'interval' && (
                <div className="grid grid-cols-2 gap-3">
                    <div className="form-group">
                        <label className="input-label" htmlFor={`${idPrefix}-every`}>Every</label>
                        <input id={`${idPrefix}-every`} type="number" min={1} max={365} value={draft.every}
                            onChange={e => update({ every: Math.max(1, Math.floor(Number(e.target.value) || 1)) })} className="input-field text-right tabular-nums" />
                    </div>
                    <div className="form-group">
                        <label className="input-label" htmlFor={`${idPrefix}-unit`}>Unit</label>
                        <Select id={`${idPrefix}-unit`} value={draft.unit} onChange={v => update({ unit: v as 'h' | 'd' })}
                            options={[{ value: 'h', label: 'Hours' }, { value: 'd', label: 'Days' }]} ariaLabel="Unit" />
                    </div>
                    <p className="col-span-2 text-xs text-(--base-06)">Counted from the previous run, so the times drift with when the job was saved. Pick fixed times to run at the same hours every day.</p>
                </div>
            )}

            {draft.kind === 'anchored' && (
                <>
                    <div className="grid grid-cols-2 gap-3">
                        <div className="form-group">
                            <label className="input-label" htmlFor={`${idPrefix}-step`}>Every</label>
                            <Select id={`${idPrefix}-step`} value={String(draft.every)} onChange={v => update({ every: Number(v) })}
                                options={ANCHOR_STEPS.map(n => ({ value: String(n), label: n === 1 ? '1 hour' : `${n} hours` }))} ariaLabel="Every" />
                        </div>
                        {timeInput('Starting at')}
                    </div>
                    <p className="text-xs text-(--base-06)">
                        Runs at {anchoredTimes(draft.every, draft.time).map(t => fmtClock(t)).join(', ')} every day.
                    </p>
                </>
            )}

            {draft.kind === 'daily' && timeInput('At')}

            {draft.kind === 'weekly' && (
                <div className="grid grid-cols-2 gap-3">
                    <div className="form-group">
                        <label className="input-label" htmlFor={`${idPrefix}-day`}>On</label>
                        <Select id={`${idPrefix}-day`} value={draft.weekday} onChange={v => update({ weekday: v as ScheduleDraft['weekday'] })} options={WEEKDAYS} ariaLabel="Weekday" />
                    </div>
                    {timeInput('At')}
                </div>
            )}

            {draft.kind === 'cron' && (
                <div className="form-group">
                    <label className="input-label" htmlFor={`${idPrefix}-cron`}>Cron expression</label>
                    <input id={`${idPrefix}-cron`} type="text" value={draft.cron} spellCheck={false} placeholder="0 3 * * *"
                        onChange={e => update({ cron: e.target.value })} className="input-field font-mono" />
                    <p className="text-xs text-(--base-06)">Five fields: minute, hour, day of month, month, day of week. The minute takes a single value, so a backup runs at most once an hour.</p>
                </div>
            )}

            {usesTimeZone(draft.kind) && (
                <div className="form-group">
                    <label className="input-label" htmlFor={`${idPrefix}-tz`}>Time zone</label>
                    <TimeZoneSelect id={`${idPrefix}-tz`} value={timezone} onChange={tz => onChange(built, tz)} />
                    <p className="text-xs text-(--base-06)">When the clocks go forward, a skipped time runs that much later; when they go back, a repeated time runs once.</p>
                </div>
            )}

            {draft.kind !== 'manual' && preview && (
                <div className={`rounded-md border border-(--base-04) bg-(--base-02) p-3 transition-opacity ${stale ? 'opacity-60' : ''}`} aria-live="polite">
                    {preview.valid ? (
                        <>
                            <div className="text-xs text-(--base-06) mb-1.5">Next runs, in your time</div>
                            <ul className="space-y-0.5 text-sm text-(--base-09) tabular-nums">
                                {preview.runs.map(r => <li key={r}>{fmtDateTime(r, RUN_FORMAT)}</li>)}
                            </ul>
                        </>
                    ) : (
                        <p className="text-xs text-(--error-light) flex items-start gap-1.5">
                            <CircleAlert size={13} className="shrink-0 mt-0.5" /> {preview.error || 'This schedule is not valid.'}
                        </p>
                    )}
                </div>
            )}
        </div>
    );
}
