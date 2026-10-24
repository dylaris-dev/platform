// The panel's one way to show a point in time. Every timestamp goes through
// here so it reads in the zone and clock format the account chose (Account ->
// Preferences), not in whatever the browser happens to default to.
//
// The choice is loaded once at boot (loadTimePrefs in the authed layout) and
// held here; until then, and for a choice never made, the browser decides.

import { API_URL, getAuthHeader, handleResponse, handleError } from './api/core';

export type TimeFormat = 'auto' | '24h' | '12h';

export interface TimePrefs {
    /** IANA zone; "" = not chosen. */
    timezone: string;
    timeFormat: TimeFormat;
}

let current: TimePrefs = { timezone: '', timeFormat: 'auto' };

export function setTimePrefs(p: TimePrefs) { current = p; }
export function getTimePrefs(): TimePrefs { return current; }

/** The browser's own zone, the default an account starts with. */
export function browserTimeZone(): string {
    try { return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'; } catch { return 'UTC'; }
}

/** Every zone this browser knows, for the pickers. */
export function timeZoneList(): string[] {
    try {
        const list = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.('timeZone');
        if (list && list.length) return list.includes('UTC') ? list : ['UTC', ...list];
    } catch { /* older browser */ }
    return ['UTC', browserTimeZone()];
}

/** The Intl options that carry the account's choice. */
export function prefOptions(p: TimePrefs = current): Intl.DateTimeFormatOptions {
    const o: Intl.DateTimeFormatOptions = {};
    if (p.timezone) o.timeZone = p.timezone;
    if (p.timeFormat === '12h') o.hour12 = true;
    if (p.timeFormat === '24h') o.hourCycle = 'h23';
    return o;
}

function toDate(v: string | number | Date): Date { return v instanceof Date ? v : new Date(v); }

/** Date and time: what Date.toLocaleString() showed, in the account's zone. */
export function fmtDateTime(v: string | number | Date, extra?: Intl.DateTimeFormatOptions): string {
    const d = toDate(v);
    if (Number.isNaN(d.getTime())) return String(v);
    return d.toLocaleString(undefined, { ...extra, ...prefOptions() });
}

/** Date only. */
export function fmtDate(v: string | number | Date, extra?: Intl.DateTimeFormatOptions): string {
    const d = toDate(v);
    if (Number.isNaN(d.getTime())) return String(v);
    return d.toLocaleDateString(undefined, { ...extra, ...prefOptions() });
}

/** Time only. */
export function fmtTime(v: string | number | Date, extra?: Intl.DateTimeFormatOptions): string {
    const d = toDate(v);
    if (Number.isNaN(d.getTime())) return String(v);
    return d.toLocaleTimeString(undefined, { ...extra, ...prefOptions() });
}

/** Whether the account reads a 12-hour clock (explicitly, or by its locale). */
export function uses12h(p: TimePrefs = current): boolean {
    if (p.timeFormat !== 'auto') return p.timeFormat === '12h';
    // Asked once: the console calls this per line.
    if (localeTwelve === undefined) {
        try { localeTwelve = !!new Intl.DateTimeFormat(undefined, { hour: 'numeric' }).resolvedOptions().hour12; } catch { localeTwelve = false; }
    }
    return localeTwelve;
}
let localeTwelve: boolean | undefined;

const CONSOLE_CLOCK = /^((?:\x1b\[[0-9;]*m)*\[)(\d{2}):(\d{2}):(\d{2})/;

/**
 * A console line's leading [HH:MM:SS] in the account's clock. The server
 * prints it in 24 hours and in its own zone (Configuration -> Time zone), so
 * only the clock format can be changed here, never the hour itself.
 */
export function consoleClock(line: string, twelve: boolean): string {
    if (!twelve) return line;
    return line.replace(CONSOLE_CLOCK, (_m, pre: string, h: string, mi: string, s: string) => {
        const n = Number(h);
        const hh = n % 12 === 0 ? 12 : n % 12;
        return `${pre}${hh}:${mi}:${s} ${n < 12 ? 'AM' : 'PM'}`;
    });
}

export async function getMyTimePrefs(): Promise<TimePrefs | null> {
    try {
        // Bounded: the authed shell waits for this, and a slow Core must not
        // hold every page on its skeleton.
        const res = await fetch(`${API_URL}/me/time-preferences`, { headers: getAuthHeader(), signal: AbortSignal.timeout(3000) });
        if (!res.ok) return null;
        return await res.json();
    } catch { return null; }
}

export async function saveMyTimePrefs(p: TimePrefs) {
    try {
        const res = await fetch(`${API_URL}/me/time-preferences`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify(p),
        });
        const out = await handleResponse(res);
        if (out.success) setTimePrefs(p);
        return out;
    } catch (err) { return handleError(err); }
}

/**
 * Boot: load the account's choice and, the first time, adopt the browser's
 * zone for it, so servers and new scheduled tasks inherit something better
 * than UTC. A failure leaves the browser defaults in place.
 */
export async function loadTimePrefs(): Promise<void> {
    const p = await getMyTimePrefs();
    if (!p) return;
    setTimePrefs(p);
    if (!p.timezone) {
        // Shown at once, saved in the background: nothing waits for the write.
        const adopted: TimePrefs = { timezone: browserTimeZone(), timeFormat: p.timeFormat || 'auto' };
        setTimePrefs(adopted);
        void saveMyTimePrefs(adopted);
    }
}

export async function getServerTimezone(serverId: number): Promise<{ timezone: string; effective: string } | null> {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/timezone`, { headers: getAuthHeader() });
        if (!res.ok) return null;
        return await res.json();
    } catch { return null; }
}

export async function setServerTimezone(serverId: number, timezone: string) {
    try {
        const res = await fetch(`${API_URL}/servers/${serverId}/timezone`, {
            method: 'PUT',
            headers: { ...getAuthHeader(), 'Content-Type': 'application/json' },
            body: JSON.stringify({ timezone }),
        });
        return handleResponse(res);
    } catch (err) { return handleError(err); }
}

// Compact "Xs/Xm/Xh/Xd ago" relative time. Shared so the connectivity UI and
// the infra node cards format last-seen identically.
export function timeAgo(dateStr: string): string {
  const now = Date.now();
  const then = new Date(dateStr).getTime();
  const diff = Math.max(0, now - then);
  const seconds = Math.floor(diff / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}
