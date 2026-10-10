"use client";

import React, { useState, useEffect, useCallback } from 'react';
import { getAllUsage, formatBytes, TrafficUsage } from '@/lib/api/usage';
import { SkeletonHeader, SkeletonCard } from '@/components/Skeleton';
import { Activity, FolderDown, HardDrive, Info, AlertTriangle } from 'lucide-react';
import SettingsPage from '@/components/settings/SettingsPage';

// currentMonth returns YYYY-MM for the <input type="month"> default.
function currentMonth(): string {
    const d = new Date();
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`;
}

export default function UsageTab() {
    const [period, setPeriod] = useState<string>(currentMonth());
    const [rows, setRows] = useState<TrafficUsage[]>([]);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState<string | null>(null);

    const load = useCallback(async (p: string) => {
        setLoading(true);
        setError(null);
        const res = await getAllUsage(p);
        if (res.success) {
            setRows(res.usage || []);
        } else {
            setError(res.message || 'Failed to load usage.');
            setRows([]);
        }
        setLoading(false);
    }, []);

    useEffect(() => { load(period); }, [period, load]);

    if (loading) {
        return (
            <div className="space-y-4">
                <SkeletonHeader />
                <SkeletonCard />
            </div>
        );
    }

    const totals = rows.reduce(
        (acc, r) => {
            acc.edge += r.edgeBytes;
            acc.relay += r.relayBytes;
            acc.backup += r.backupBytes;
            return acc;
        },
        { edge: 0, relay: 0, backup: 0 },
    );

    return (
        <SettingsPage
            title="Traffic and storage usage"
            icon={Activity}
            width="full"
            description="Per-tenant metering for BYON."
            actions={
                <label className="flex flex-col gap-1">
                    <span className="font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-06)">Billing month</span>
                    <input
                        type="month"
                        value={period}
                        onChange={e => setPeriod(e.target.value || currentMonth())}
                        className="input-field w-44"
                    />
                </label>
            }
        >
            {/* Context note */}
            <div className="flex items-start gap-2 rounded-md border border-(--base-04) bg-(--base-01) p-3">
                <Info size={15} className="text-(--base-06) shrink-0 mt-0.5" />
                <p className="text-sm text-(--base-07)">
                    Edge is billable player traffic. Relay is filebrowser transfer through the Beam relay,
                    and Backup is R2 storage held right now. Only servers on tenant-owned (BYON) nodes are
                    metered; platform-owned nodes never appear here. A value shown as <span className="font-mono">used / limit</span> is
                    capped by the tenant&apos;s plan; a red row is over its (warn-only) traffic limit and is not blocked.
                </p>
            </div>

            {error && (
                <div className="rounded-md border border-(--error) bg-(--error)/10 p-3 text-sm text-(--error-light)">
                    {error}
                </div>
            )}

            {/* Summary cards */}
            <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
                <SummaryCard icon={<Activity size={16} />} label="Edge (player)" value={formatBytes(totals.edge)} />
                <SummaryCard icon={<FolderDown size={16} />} label="Relay (files)" value={formatBytes(totals.relay)} />
                <SummaryCard icon={<HardDrive size={16} />} label="Backup (R2)" value={formatBytes(totals.backup)} />
            </div>

            {/* Per-tenant table */}
            {rows.length === 0 ? (
                <div className="rounded-md border border-(--base-04) p-8 text-center text-(--base-06) text-sm">
                    No metered usage for this month.
                </div>
            ) : (
                <div className="overflow-x-auto rounded-md border border-(--base-04)">
                    <table className="w-full text-sm">
                        <thead>
                            <tr className="border-b border-(--base-04) text-left">
                                <Th>Tenant</Th>
                                <Th right>Edge</Th>
                                <Th right>Relay</Th>
                                <Th right>Backup</Th>
                                <Th right>Total</Th>
                            </tr>
                        </thead>
                        <tbody>
                            {rows.map(r => (
                                <tr key={r.userId} className="border-b border-(--base-03) last:border-0 hover:bg-(--base-01)">
                                    <td className="px-4 py-2.5">
                                        <div className="text-(--base-09)">{r.username || '(unknown)'}</div>
                                        <div className="font-mono text-[10px] text-(--base-05)">{r.userId.slice(0, 8)}</div>
                                    </td>
                                    <UsageCell bytes={r.edgeBytes} limitGb={r.limits?.trafficEdgeGb} over={r.over?.edge} />
                                    <UsageCell bytes={r.relayBytes} limitGb={r.limits?.trafficRelayGb} over={r.over?.relay} />
                                    <UsageCell bytes={r.backupBytes} limitGb={r.limits?.r2QuotaGb} over={r.over?.r2} />
                                    <UsageCell bytes={r.edgeBytes + r.relayBytes} limitGb={r.limits?.trafficCombinedGb} over={r.over?.combined} strong />
                                </tr>
                            ))}
                        </tbody>
                    </table>
                </div>
            )}
        </SettingsPage>
    );
}

function SummaryCard({ icon, label, value }: { icon: React.ReactNode; label: string; value: string }) {
    return (
        <div className="rounded-md border border-(--base-04) bg-(--base-01) p-4">
            <div className="flex items-center gap-2 text-(--base-06)">
                {icon}
                <span className="font-mono text-[10px] uppercase tracking-[0.08em]">{label}</span>
            </div>
            <div className="mt-2 font-display text-2xl text-(--base-09)">{value}</div>
        </div>
    );
}

function Th({ children, right }: { children: React.ReactNode; right?: boolean }) {
    return (
        <th className={`px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.08em] text-(--base-06) ${right ? 'text-right' : ''}`}>
            {children}
        </th>
    );
}

function Td({ children, right, strong }: { children: React.ReactNode; right?: boolean; strong?: boolean }) {
    return (
        <td className={`px-4 py-2.5 ${right ? 'text-right font-mono' : ''} ${strong ? 'text-(--base-09)' : 'text-(--base-07)'}`}>
            {children}
        </td>
    );
}

// UsageCell shows a metered value, optionally against its plan limit, red + warn
// when over (traffic limits are warn-only — display, not enforcement).
function UsageCell({ bytes, limitGb, over, strong }: { bytes: number; limitGb?: number; over?: boolean; strong?: boolean }) {
    return (
        <td className={`px-4 py-2.5 text-right font-mono ${over ? 'text-(--error-light)' : strong ? 'text-(--base-09)' : 'text-(--base-07)'}`}>
            <span className="inline-flex items-center justify-end gap-1">
                {over && <AlertTriangle size={12} />}
                {formatBytes(bytes)}
                {limitGb != null && limitGb > 0 && <span className="text-(--base-05)"> / {limitGb} GB</span>}
            </span>
        </td>
    );
}
