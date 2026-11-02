// The backup schedule grammar as the editor sees it. Core owns the meaning
// (core/services/backup_schedule.go) and previews the next runs, including the
// daylight-saving rule; this file only turns a schedule string into form fields
// and back, and says it in words.

import { browserTimeZone, getTimePrefs, prefOptions, uses12h } from '@/lib/time';

export type ScheduleKind = 'manual' | 'interval' | 'anchored' | 'daily' | 'weekly' | 'cron';
export type Weekday = 'mon' | 'tue' | 'wed' | 'thu' | 'fri' | 'sat' | 'sun';

export interface ScheduleDraft {
    kind: ScheduleKind;
    /** interval: n of unit; anchored: n hours. */
    every: number;
    unit: 'h' | 'd';
    /** 24-hour HH:MM, for anchored, daily and weekly. */
    time: string;
    weekday: Weekday;
    /** The five cron fields, without the "cron " prefix. */
    cron: string;
}

export const WEEKDAYS: { value: Weekday; label: string }[] = [
    { value: 'mon', label: 'Monday' }, { value: 'tue', label: 'Tuesday' }, { value: 'wed', label: 'Wednesday' },
    { value: 'thu', label: 'Thursday' }, { value: 'fri', label: 'Friday' }, { value: 'sat', label: 'Saturday' },
    { value: 'sun', label: 'Sunday' },
];

/** The hour steps an anchored interval may take: the ones that divide a day. */
export const ANCHOR_STEPS = [1, 2, 3, 4, 6, 8, 12, 24];

const DEFAULTS: ScheduleDraft = { kind: 'manual', every: 6, unit: 'h', time: '03:00', weekday: 'mon', cron: '0 3 * * *' };

const CLOCK = /^([01]?\d|2[0-3]):([0-5]\d)$/;

function pad(n: number): string { return String(n).padStart(2, '0'); }

function normClock(s: string): string {
    const m = CLOCK.exec(s);
    return m ? `${pad(Number(m[1]))}:${m[2]}` : s;
}

/** Form fields for a stored schedule. Anything unrecognised lands in the cron field as written, so saving never silently changes it. */
export function parseSchedule(schedule: string): ScheduleDraft {
    const s = schedule.trim().replace(/\s+/g, ' ');
    if (s === '' || s === 'manual') return { ...DEFAULTS };
    let m = /^every (\d+)h from (\S+)$/.exec(s);
    if (m && CLOCK.test(m[2])) return { ...DEFAULTS, kind: 'anchored', every: Number(m[1]), time: normClock(m[2]) };
    m = /^every (\d+)([hd])$/.exec(s);
    if (m) return { ...DEFAULTS, kind: 'interval', every: Number(m[1]), unit: m[2] as 'h' | 'd' };
    m = /^daily (\S+)$/.exec(s);
    if (m && CLOCK.test(m[1])) return { ...DEFAULTS, kind: 'daily', time: normClock(m[1]) };
    m = /^weekly (mon|tue|wed|thu|fri|sat|sun) (\S+)$/.exec(s);
    if (m && CLOCK.test(m[2])) return { ...DEFAULTS, kind: 'weekly', weekday: m[1] as Weekday, time: normClock(m[2]) };
    if (s.startsWith('cron ')) return { ...DEFAULTS, kind: 'cron', cron: s.slice(5) };
    return { ...DEFAULTS, kind: 'cron', cron: s };
}

/** The schedule string Core stores. */
export function buildSchedule(d: ScheduleDraft): string {
    switch (d.kind) {
        case 'manual': return 'manual';
        case 'interval': return `every ${d.every}${d.unit}`;
        case 'anchored': return `every ${d.every}h from ${normClock(d.time)}`;
        case 'daily': return `daily ${normClock(d.time)}`;
        case 'weekly': return `weekly ${d.weekday} ${normClock(d.time)}`;
        case 'cron': {
            const c = d.cron.trim().replace(/\s+/g, ' ');
            // A legacy value parseSchedule could not read is kept as written.
            return /^(every|daily|weekly|manual)\b/.test(c) ? c : `cron ${c}`;
        }
    }
}

/** Whether the schedule is read on the wall clock, so its time zone matters. */
export function usesTimeZone(kind: ScheduleKind): boolean {
    return kind !== 'manual' && kind !== 'interval';
}

/** The wall times an anchored interval runs at, earliest first: every 6h from 14:30 -> 02:30, 08:30, 14:30, 20:30. */
export function anchoredTimes(every: number, time: string): string[] {
    const m = CLOCK.exec(time);
    if (!m || every <= 0 || 24 % every !== 0) return [];
    const out: string[] = [];
    for (let h = Number(m[1]) % every; h < 24; h += every) out.push(`${pad(h)}:${m[2]}`);
    return out;
}

/** A wall time (HH:MM) in the viewer's clock: 14:30 or 2:30 PM, per their preference or locale. */
export function fmtClock(time: string, opts: Intl.DateTimeFormatOptions = prefOptions()): string {
    const m = CLOCK.exec(time);
    if (!m) return time;
    // A wall time, not an instant: formatted in UTC so no zone moves it.
    const d = new Date(Date.UTC(2000, 0, 1, Number(m[1]), Number(m[2])));
    const twelve = opts.hour12 ?? (opts.hourCycle ? false : uses12h());
    return d.toLocaleTimeString(undefined, twelve
        ? { hour: 'numeric', minute: '2-digit', hour12: true, timeZone: 'UTC' }
        : { hour: '2-digit', minute: '2-digit', hourCycle: 'h23', timeZone: 'UTC' });
}

/** The schedule in words, for job lists. */
export function describeSchedule(schedule: string, timezone?: string, opts?: Intl.DateTimeFormatOptions): string {
    const d = parseSchedule(schedule);
    const zone = timezone && usesTimeZone(d.kind) ? ` (${timezone.replace(/_/g, ' ')})` : '';
    const clock = (t: string) => fmtClock(t, opts);
    switch (d.kind) {
        case 'manual': return 'Manual only';
        case 'interval': return `Every ${d.every} ${d.unit === 'd' ? (d.every === 1 ? 'day' : 'days') : (d.every === 1 ? 'hour' : 'hours')}`;
        case 'anchored': {
            const times = anchoredTimes(d.every, d.time);
            if (times.length === 1) return `Daily at ${clock(times[0])}${zone}`;
            const shown = times.length > 4 ? `${times.slice(0, 4).map(clock).join(', ')}, ...` : times.map(clock).join(', ');
            return `Every ${d.every} ${d.every === 1 ? 'hour' : 'hours'}, at ${shown}${zone}`;
        }
        case 'daily': return `Daily at ${clock(d.time)}${zone}`;
        case 'weekly': return `${WEEKDAYS.find(w => w.value === d.weekday)!.label}s at ${clock(d.time)}${zone}`;
        case 'cron': return `${buildSchedule(d)}${zone}`;
    }
}

/**
 * The zone the editor starts with. A relative schedule never read its zone, so
 * the UTC stored for it is no choice anybody made: switching such a job to
 * "daily 03:00" offers the viewer's own zone instead.
 */
export function editorZone(schedule: string, timezone?: string): string {
    if (timezone && usesTimeZone(parseSchedule(schedule).kind)) return timezone;
    return getTimePrefs().timezone || browserTimeZone();
}
