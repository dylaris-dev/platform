// The long-term statistics record.
//
// Read-only. There is no write endpoint by design: the value of this record is
// that every number in it was measured, so nothing reachable from a browser can
// put one there.

import { API_URL, getAuthHeader, handleResponse } from '@/lib/api/core';

/** How a series is re-aggregated over a longer window. */
export type MetricKind = 'counter' | 'gauge';

export type MetricUnit = 'count' | 'bytes' | 'bps' | 'percent' | 'seconds';

export interface MetricSeriesInfo {
    metric: string;
    label: string;
    group: string;
    kind: MetricKind;
    unit: MetricUnit;
    help?: string;
    /** Recorded once per component, so a chart has to say whose line it draws. */
    perSubject?: boolean;
}

export interface MetricPoint {
    t: string;
    min: number;
    max: number;
    sum: number;
    count: number;
    /** sum/count - the average of the SAMPLES, not of the buckets. */
    avg: number;
}

export interface MetricSeries {
    metric: string;
    subject?: string;
    points: MetricPoint[];
}

export interface MetricCoverage {
    /** The first sample ever recorded. Null before anything is. */
    since: string | null;
    latest: string | null;
    resolution: string;
}

export interface MetricHeadline {
    metric: string;
    label: string;
    unit: MetricUnit;
    value: number;
    how: 'peak' | 'total' | 'avg';
    /** What the number does NOT mean, where the label alone would mislead. */
    help?: string;
}

/**
 * Why there is nothing to show, when there is nothing to show.
 *
 * `disabled` is the DEFAULT and not a fault - recording starts when an operator
 * switches it on. `unavailable` means the database could not be reached, which
 * is a problem to fix. Collapsing the two would send someone looking for a
 * broken database when nobody had turned the feature on.
 */
export type MetricsUnavailableReason = 'disabled' | 'unavailable';

/** What every metrics response carries, whether or not it has data. */
export interface MetricsEnvelope {
    available: boolean;
    reason?: MetricsUnavailableReason;
    message?: string;
}

export interface MetricsCatalogResponse extends MetricsEnvelope {
    series?: MetricSeriesInfo[];
    coverage?: MetricCoverage;
}

export interface MetricsSeriesResponse extends MetricsEnvelope {
    series?: MetricSeries[];
    from?: string;
    to?: string;
}

export interface MetricsSummaryResponse extends MetricsEnvelope {
    headlines?: MetricHeadline[];
    coverage?: MetricCoverage;
    from?: string;
    to?: string;
}

/**
 * A network failure is turned into the SAME shape the server sends when there
 * is nothing to show, rather than into the generic {success:false} the other
 * API modules return.
 *
 * The screen then has one thing to render for "no data, and here is why",
 * whether the reason came from the server or from the request never arriving.
 * The alternative is a page that has to distinguish two failure shapes and, in
 * practice, renders blank for one of them.
 */
async function get<T extends MetricsEnvelope>(path: string): Promise<T> {
    try {
        const res = await fetch(`${API_URL}${path}`, { headers: getAuthHeader() });
        return await handleResponse(res);
    } catch (e) {
        console.error('metrics API:', e);
        return {
            available: false,
            reason: 'unavailable',
            message: 'Could not reach the platform.',
        } as T;
    }
}

export const getMetricsCatalog = () =>
    get<MetricsCatalogResponse>('/admin/metrics/catalog');

export const getMetricsSummary = (range: string) =>
    get<MetricsSummaryResponse>(`/admin/metrics/summary?range=${encodeURIComponent(range)}`);

export interface SeriesRequest {
    metrics: string[];
    range: string;
    /** Bucket width in seconds. Omitted lets the server pick a drawable one. */
    step?: number;
    subject?: string;
    region?: string;
    /** One line per component instead of one folded line. */
    split?: boolean;
}

export function seriesQueryString(req: SeriesRequest): string {
    const p = new URLSearchParams();
    p.set('metric', req.metrics.join(','));
    p.set('range', req.range);
    if (req.step) p.set('step', String(req.step));
    if (req.subject) p.set('subject', req.subject);
    if (req.region) p.set('region', req.region);
    if (req.split) p.set('split', '1');
    return p.toString();
}

export const getMetricsSeries = (req: SeriesRequest) =>
    get<MetricsSeriesResponse>(`/admin/metrics/series?${seriesQueryString(req)}`);

/**
 * The export URL.
 *
 * Built rather than fetched: the response is a file download, and the browser
 * has to be the one to receive it. The auth header cannot ride on a plain
 * navigation, so the caller fetches it and hands the blob to a download - see
 * downloadMetricsExport below.
 */
export function metricsExportPath(req: SeriesRequest, format: 'csv' | 'json'): string {
    return `/admin/metrics/export?${seriesQueryString(req)}&format=${format}`;
}

/**
 * Fetch the export and hand it to the browser as a file.
 *
 * Through fetch rather than a link, because the API is behind a bearer token
 * that a plain <a href> cannot carry - a link would arrive unauthenticated and
 * download a 401 body named like a spreadsheet.
 */
export async function downloadMetricsExport(req: SeriesRequest, format: 'csv' | 'json'): Promise<void> {
    const res = await fetch(`${API_URL}${metricsExportPath(req, format)}`, { headers: getAuthHeader() });
    if (!res.ok) {
        throw new Error(`Export failed (${res.status})`);
    }
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `dylaris-metrics-${new Date().toISOString().slice(0, 10)}.${format}`;
    document.body.appendChild(a);
    a.click();
    a.remove();
    // Released on the next tick rather than immediately: revoking before the
    // browser has started reading the blob cancels the download in Firefox.
    setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** Formats a value for display, by unit. */
export function formatMetric(value: number, unit: MetricUnit): string {
    switch (unit) {
        case 'bytes':
            return formatScaled(value, ['B', 'KB', 'MB', 'GB', 'TB', 'PB'], 1024);
        case 'bps':
            // "Mbit/s", not "Mbps" - the same spelling the Bandwidth tab uses,
            // and for the same reason: Mbps is widely read as megaBYTES, which
            // is an eightfold error in the direction of "plenty of room". These
            // series are bits per second.
            return formatScaled(value, ['bit/s', 'kbit/s', 'Mbit/s', 'Gbit/s', 'Tbit/s'], 1000);
        case 'percent':
            return `${round(value, 1)}%`;
        case 'seconds':
            return formatDuration(value);
        default:
            return formatCount(value);
    }
}

function round(v: number, places: number): number {
    const f = 10 ** places;
    return Math.round(v * f) / f;
}

function formatScaled(value: number, units: string[], base: number): string {
    let v = Math.abs(value);
    let i = 0;
    while (v >= base && i < units.length - 1) {
        v /= base;
        i++;
    }
    const sign = value < 0 ? '-' : '';
    return `${sign}${round(v, v < 10 ? 2 : v < 100 ? 1 : 0)} ${units[i]}`;
}

function formatCount(value: number): string {
    if (Math.abs(value) >= 1_000_000) return `${round(value / 1_000_000, 1)}M`;
    if (Math.abs(value) >= 10_000) return `${round(value / 1000, 1)}k`;
    // Small counts keep one decimal only when they have one: an average of 3.4
    // players is real, but "12.0 nodes" reads like a bug.
    return Number.isInteger(value) ? String(value) : String(round(value, 1));
}

function formatDuration(seconds: number): string {
    if (seconds < 60) return `${Math.round(seconds)}s`;
    if (seconds < 3600) return `${Math.round(seconds / 60)}m`;
    if (seconds < 86400) return `${round(seconds / 3600, 1)}h`;
    return `${round(seconds / 86400, 1)}d`;
}

/**
 * Which of a point's numbers a series MEANS.
 *
 * A counter is a total (how many handovers happened in this bucket); a gauge is
 * an average (what the CPU was doing across it). Reading a counter as an
 * average understates it by the number of samples in the bucket, and reading a
 * gauge as a total produces a figure with no meaning at all.
 */
export function pointValue(p: MetricPoint, kind: MetricKind): number {
    return kind === 'counter' ? p.sum : p.avg;
}
