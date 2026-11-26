"use client";

import React, { useCallback, useEffect, useRef, useState } from 'react';
import {
    CheckCircle2,
    AlertTriangle,
    XCircle,
    MinusCircle,
    RefreshCw,
    Loader2,
    Activity,
} from 'lucide-react';
import { getSystemHealth, type SystemHealth, type HealthStatus, type HealthComponent } from '@/lib/api';
import { healthGuidance } from '@/lib/healthGuidance';
import { fmtTime } from '@/lib/time';
import SettingsPage from '@/components/settings/SettingsPage';
import EmptyState from '@/components/ui/EmptyState';

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
            return { text: 'text-(--base-06)', bg: 'bg-(--base-03)', border: 'border-(--base-04)' };
    }
}

function StatusPill({ status }: { status: HealthStatus }) {
    const meta = STATUS_META[status];
    const c = colorClasses(meta.color);
    const Icon = meta.Icon;
    return (
        <span className={`inline-flex h-5 items-center gap-1.5 rounded-full px-2 text-[11px] font-medium ${c.text} ${c.bg}`}>
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
        <div className="srv-surface p-5">
            <div className="flex items-start justify-between gap-3">
                <div className="flex items-center gap-2.5 min-w-0">
                    <Icon size={18} className={`${c.text} shrink-0`} />
                    <div className="min-w-0">
                        <div className="truncate text-sm font-semibold text-(--base-09)">{comp.name}</div>
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
                <div className="srv-callout mt-3 !block" data-tone={guidance.selfHealing ? undefined : 'warning'}>
                    <div className="flex items-center gap-1.5">
                        <span className="srv-label">
                            What to do
                        </span>
                        {!guidance.selfHealing && (
                            <span className="text-xs font-medium text-(--warning-light)">
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
                <div className="mt-3 space-y-1.5 border-t border-(--base-03) pt-3">
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
        return <SettingsPage title="Status" loading skeletonCards={3} width="3xl">{null}</SettingsPage>;
    }

    if (error && !health) {
        return (
            <SettingsPage title="Status" icon={Activity} width="3xl">
                <EmptyState
                    tone="error"
                    icon={<XCircle size={20} />}
                    title="Status could not be loaded"
                    description={error}
                    action={
                        <button onClick={() => load(true)} className="btn btn-secondary btn-sm inline-flex items-center gap-1.5">
                            <RefreshCw size={13} className={refreshing ? 'animate-spin' : ''} />
                            Retry
                        </button>
                    }
                />
            </SettingsPage>
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
        <SettingsPage
            title="Status"
            icon={Activity}
            width="3xl"
            description="Live health of every platform component. Refreshes every 10 seconds."
            actions={
                <button
                    onClick={() => load(true)}
                    disabled={refreshing}
                    className="btn btn-secondary btn-sm inline-flex items-center gap-1.5"
                >
                    {refreshing ? <Loader2 size={13} className="animate-spin" /> : <RefreshCw size={13} />}
                    Refresh
                </button>
            }
        >
            {/* Overall banner */}
            <div className={`flex items-center gap-3 rounded-xl p-4 ${oc.bg}`} role="status">
                <OIcon size={22} className={oc.text} />
                <div>
                    <div className={`text-base font-semibold ${oc.text}`}>{overall.label}</div>
                    <div className="mt-0.5 font-mono text-xs text-(--base-06)">Last checked {checked}</div>
                </div>
            </div>

            <div className="space-y-3">
                {health.components.map(comp => (
                    <ComponentCard key={comp.key} comp={comp} />
                ))}
            </div>
        </SettingsPage>
    );
}
