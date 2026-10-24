"use client";

import React, { useCallback, useEffect, useRef, useState } from 'react';
import {
    CheckCircle2,
    AlertTriangle,
    XCircle,
    MinusCircle,
    RefreshCw,
    Loader2,
} from 'lucide-react';
import { getSystemHealth, type SystemHealth, type HealthStatus, type HealthComponent } from '@/lib/api';
import { healthGuidance } from '@/lib/healthGuidance';
import { fmtTime } from '@/lib/time';

const POLL_INTERVAL_MS = 10_000;

// Per-status presentation. `color` selects the matching design-token family
// (success / warning / error), `muted` falls back to neutral base tokens.
const STATUS_META: Record<HealthStatus, { label: string; color: 'success' | 'warning' | 'error' | 'muted'; Icon: typeof CheckCircle2 }> = {
    up: { label: 'Operational', color: 'success', Icon: CheckCircle2 },
    degraded: { label: 'Degraded', color: 'warning', Icon: AlertTriangle },
    down: { label: 'Down', color: 'error', Icon: XCircle },
    disabled: { label: 'Disabled', color: 'muted', Icon: MinusCircle },
};

const OVERALL_META: Record<SystemHealth['overall'], { label: string; color: 'success' | 'warning' | 'error'; Icon: typeof CheckCircle2 }> = {
    healthy: { label: 'All systems operational', color: 'success', Icon: CheckCircle2 },
    degraded: { label: 'Degraded performance', color: 'warning', Icon: AlertTriangle },
    down: { label: 'Major outage', color: 'error', Icon: XCircle },
};

// Token class fragments per color family. Kept as full literal strings so
// Tailwind's scanner keeps them.
function colorClasses(color: 'success' | 'warning' | 'error' | 'muted') {
    switch (color) {
        case 'success':
            return { text: 'text-(--success-light)', bg: 'bg-(--success-ghost)', border: 'border-(--success-border)' };
        case 'warning':
            return { text: 'text-(--warning-light)', bg: 'bg-(--warning-ghost)', border: 'border-(--warning-border)' };
        case 'error':
            return { text: 'text-(--error-light)', bg: 'bg-(--error-ghost)', border: 'border-(--error-border)' };
        default:
            return { text: 'text-(--base-06)', bg: 'bg-transparent', border: 'border-(--base-04)' };
    }
}

function StatusPill({ status }: { status: HealthStatus }) {
    const meta = STATUS_META[status];
    const c = colorClasses(meta.color);
    const Icon = meta.Icon;
    return (
        <span className={`inline-flex items-center gap-1.5 rounded-(--radius-sm) border px-2 py-0.5 font-mono text-[10px] uppercase tracking-[0.08em] ${c.text} ${c.bg} ${c.border}`}>
            <Icon size={11} />
            {meta.label}
        </span>
    );
}

function ComponentCard({ comp }: { comp: HealthComponent }) {
    const meta = STATUS_META[comp.status] ?? STATUS_META.disabled;
    const c = colorClasses(meta.color);
    const Icon = meta.Icon;
    const guidance = healthGuidance(comp.key, comp.cause);
    return (
        <div className="rounded-(--radius-lg) border border-(--base-04) bg-(--base-01) p-4">
            <div className="flex items-start justify-between gap-3">
                <div className="flex items-center gap-2.5 min-w-0">
                    <Icon size={18} className={`${c.text} shrink-0`} />
                    <div className="min-w-0">
                        <div className="font-display text-sm font-semibold text-(--base-09) truncate">{comp.name}</div>
                        {comp.detail && (
                            <div className="font-mono text-xs text-(--base-06) mt-0.5">{comp.detail}</div>
                        )}
                    </div>
                </div>
                <StatusPill status={comp.status} />
            </div>

            {/* What to do about it, when the component classified its failure.
                Shown ABOVE the raw server message: the message is evidence, the
                guidance is the action, and an operator reading top to bottom
                should hit the action first. */}
            {guidance && (
                <div className="mt-3 rounded-(--radius-md) border border-(--base-04) bg-(--base-02) p-3">
                    <div className="flex items-center gap-1.5">
                        <span className="font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-06)">
                            What to do
                        </span>
                        {!guidance.selfHealing && (
                            <span className="font-mono text-[10px] uppercase tracking-[0.08em] text-(--warning-light)">
                                needs you
                            </span>
                        )}
                    </div>
                    <p className="mt-1.5 text-xs leading-relaxed text-(--base-08)">{guidance.advice}</p>
                </div>
            )}

            {comp.reason && (
                <p className="mt-3 font-mono text-xs leading-relaxed break-words text-(--base-07)">{comp.reason}</p>
            )}

            {comp.items && comp.items.length > 0 && (
                <div className="mt-3 border-t border-(--base-03) pt-3 space-y-1.5">
                    {comp.items.map((item, i) => {
                        const im = STATUS_META[item.status] ?? STATUS_META.disabled;
                        const ic = colorClasses(im.color);
                        const IIcon = im.Icon;
                        // A short detail ("online", "offline, last seen ...") reads
                        // best on one line beside the name. A node that reported
                        // WHY it cannot reach Core sends a sentence, and truncating
                        // that to the width left over next to a hostname hid the one
                        // part worth reading - the row said "offline, last seen 12:0…"
                        // exactly as before the reason existed. Long details wrap onto
                        // their own line instead.
                        const longDetail = (item.detail?.length ?? 0) > 48;
                        return (
                            <div key={i} className={`text-xs ${longDetail ? 'space-y-1' : 'flex items-center justify-between gap-3'}`}>
                                <span className="flex items-center gap-1.5 min-w-0 text-(--base-08)">
                                    <IIcon size={12} className={`${ic.text} shrink-0`} />
                                    <span className="truncate">{item.name}</span>
                                </span>
                                {item.detail && (
                                    <span
                                        className={
                                            longDetail
                                                ? 'block font-mono text-[11px] leading-relaxed break-words text-(--base-07) pl-[18px]'
                                                : 'font-mono text-[11px] text-(--base-06) truncate text-right'
                                        }
                                    >
                                        {item.detail}
                                    </span>
                                )}
                            </div>
                        );
                    })}
                </div>
            )}
        </div>
    );
}

export default function StatusTab() {
    const [health, setHealth] = useState<SystemHealth | null>(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);
    const [refreshing, setRefreshing] = useState(false);
    const mounted = useRef(true);

    const load = useCallback(async (isManual: boolean) => {
        if (isManual) setRefreshing(true);
        try {
            const res = await getSystemHealth();
            if (!mounted.current) return;
            if (res?.success && res.health) {
                setHealth(res.health);
                setError(null);
            } else {
                setError((res as any)?.error || 'Failed to load status');
            }
        } catch (e: any) {
            if (mounted.current) setError(e?.message || 'Failed to load status');
        } finally {
            if (mounted.current) {
                setLoading(false);
                setRefreshing(false);
            }
        }
    }, []);

    useEffect(() => {
        mounted.current = true;
        load(false);
        const id = setInterval(() => load(false), POLL_INTERVAL_MS);
        return () => {
            mounted.current = false;
            clearInterval(id);
        };
    }, [load]);

    // First-load skeleton.
    if (loading && !health) {
        return (
            <div className="space-y-3 max-w-3xl">
                <div className="h-16 rounded-(--radius-lg) bg-(--base-02) animate-pulse" />
                {[0, 1, 2, 3, 4].map(i => (
                    <div key={i} className="h-20 rounded-(--radius-lg) bg-(--base-02) animate-pulse" />
                ))}
            </div>
        );
    }

    if (error && !health) {
        return (
            <div className="max-w-3xl">
                <div className="rounded-(--radius-lg) border border-(--error-border) bg-(--error-ghost) p-4 text-(--error-light) text-sm">
                    {error}
                </div>
                <button onClick={() => load(true)} className="btn btn-secondary text-xs mt-3 inline-flex items-center gap-1.5">
                    <RefreshCw size={13} className={refreshing ? 'animate-spin' : ''} />
                    Retry
                </button>
            </div>
        );
    }

    if (!health) return null;

    const overall = OVERALL_META[health.overall];
    const oc = colorClasses(overall.color);
    const OIcon = overall.Icon;
    const checked = (() => {
        try { return fmtTime(health.checkedAt); } catch { return health.checkedAt; }
    })();

    return (
        <div className="space-y-3 max-w-3xl">
            {/* Overall banner */}
            <div className={`flex items-center justify-between gap-3 rounded-(--radius-lg) border p-4 ${oc.bg} ${oc.border}`}>
                <div className="flex items-center gap-3">
                    <OIcon size={22} className={oc.text} />
                    <div>
                        <div className={`font-display text-base font-semibold ${oc.text}`}>{overall.label}</div>
                        <div className="font-mono text-[11px] text-(--base-06) mt-0.5">Last checked {checked}</div>
                    </div>
                </div>
                <button
                    onClick={() => load(true)}
                    disabled={refreshing}
                    className="btn btn-secondary text-xs py-1.5 px-3 inline-flex items-center gap-1.5 disabled:opacity-40"
                >
                    {refreshing ? <Loader2 size={13} className="animate-spin" /> : <RefreshCw size={13} />}
                    Refresh
                </button>
            </div>

            {/* Component cards */}
            <div className="space-y-3">
                {health.components.map(comp => (
                    <ComponentCard key={comp.key} comp={comp} />
                ))}
            </div>
        </div>
    );
}
