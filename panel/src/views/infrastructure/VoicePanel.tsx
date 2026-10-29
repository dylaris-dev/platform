"use client";

import { useEffect, useState } from 'react';
import { AlertTriangle, Mic } from 'lucide-react';
import { SkeletonCard, SkeletonTable } from '@/components/Skeleton';
import { formatBytes } from '@/components/infra/TrafficPools';
import { getVoiceOverview, type VoiceOverview, type VoiceRegionStat, type BedrockServerStat } from '@/lib/api';

/**
 * Voice chat per region and per server for one month: the relays that are
 * alive, how much of each region's port pool is held, and the voice bytes.
 * Voice is part of player traffic, not on top of it; this screen only shows
 * the share.
 */

export type VoiceRegionRow = VoiceRegionStat & { pct: number };

/** Regions by name, each with how much of its port pool is held (floored). */
export function voiceRegionRows(o: VoiceOverview): VoiceRegionRow[] {
    return [...(o.regions ?? [])]
        .sort((a, b) => a.region.localeCompare(b.region))
        .map(r => ({ ...r, pct: r.portsTotal > 0 ? Math.floor((r.portsUsed * 100) / r.portsTotal) : 0 }));
}

/** Bedrock rows, busiest first. Absent from an older Core, so never undefined. */
export function bedrockRows(o: VoiceOverview): BedrockServerStat[] {
    return [...(o.bedrock ?? [])].sort((a, b) => (b.rxBytes + b.txBytes) - (a.rxBytes + a.txBytes));
}

const MOD_LABEL: Record<string, string> = { svc: 'Simple Voice Chat', plasmo: 'Plasmo Voice' };

function currentMonth(): string {
    const d = new Date();
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`;
}

export default function VoicePanel() {
    const [period, setPeriod] = useState(currentMonth());
    const [overview, setOverview] = useState<VoiceOverview | null>(null);
    const [error, setError] = useState('');
    const [loading, setLoading] = useState(true);

    useEffect(() => {
        let cancelled = false;
        setLoading(true);
        setError('');
        getVoiceOverview(period)
            .then(res => {
                if (cancelled) return;
                if (res?.success === false || !Array.isArray(res?.regions)) {
                    setOverview(null);
                    setError(res?.message || 'The voice overview could not be loaded.');
                } else {
                    setOverview(res);
                }
            })
            .catch(() => { if (!cancelled) { setOverview(null); setError('The request could not be sent.'); } })
            .finally(() => { if (!cancelled) setLoading(false); });
        return () => { cancelled = true; };
    }, [period]);

    const regions = overview ? voiceRegionRows(overview) : [];
    const bedrock = overview ? bedrockRows(overview) : [];

    return (
        <div className="flex flex-col gap-4">
            <div className="flex items-end justify-between gap-3 flex-wrap">
                <div className="flex items-center gap-2">
                    <Mic size={16} className="text-(--base-06)" aria-hidden="true" />
                    <span className="mono-label">Voice chat</span>
                </div>
                <label className="flex flex-col gap-1">
                    <span className="mono-label">Month</span>
                    <input
                        type="month"
                        value={period}
                        onChange={e => setPeriod(e.target.value || currentMonth())}
                        className="input-field w-44"
                    />
                </label>
            </div>

            {loading && !overview ? (
                <>
                    <SkeletonCard height="h-32" />
                    <SkeletonTable />
                </>
            ) : error ? (
                <div className="alert alert-error text-sm flex items-start gap-2" role="alert">
                    <AlertTriangle size={15} className="mt-0.5 shrink-0" />
                    <span>{error}</span>
                </div>
            ) : regions.length === 0 && bedrock.length === 0 ? (
                <div className="card p-8 text-center text-(--base-06) text-sm">
                    No voice relay is running and no server has voice chat on.
                </div>
            ) : (
                <>
                    {regions.length > 0 && <section className="card p-4 overflow-x-auto" aria-label="Voice per region">
                        <table className="w-full text-sm">
                            <thead>
                                <tr className="border-b border-(--base-03)">
                                    <th className="mono-label text-left pb-2 pr-4">Region</th>
                                    <th className="mono-label text-left pb-2 pr-4">Relays</th>
                                    <th className="mono-label text-left pb-2 pr-4">Ports</th>
                                    <th className="mono-label text-right pb-2 pr-4">To servers</th>
                                    <th className="mono-label text-right pb-2">To players</th>
                                </tr>
                            </thead>
                            <tbody>
                                {regions.map(r => (
                                    <tr key={r.region} className="border-b border-(--base-03)/50 last:border-0">
                                        <td className="py-2.5 pr-4 text-(--base-09) font-medium">{r.region}</td>
                                        <td className={`py-2.5 pr-4 tabular-nums ${r.relays === 0 ? 'text-(--warning-light)' : 'text-(--base-07)'}`}>
                                            {r.relays === 0 ? 'None running' : r.relays}
                                        </td>
                                        <td className="py-2.5 pr-4 min-w-40">
                                            <div className="text-xs text-(--base-07) tabular-nums mb-1">
                                                {r.portsUsed} of {r.portsTotal} ({r.pct}%)
                                            </div>
                                            <div
                                                className="h-1.5 rounded-full bg-(--base-03) overflow-hidden"
                                                role="progressbar"
                                                aria-valuenow={r.pct}
                                                aria-valuemin={0}
                                                aria-valuemax={100}
                                                aria-label={`${r.region}: ${r.portsUsed} of ${r.portsTotal} voice ports held`}
                                            >
                                                <div
                                                    className={`h-full rounded-full ${r.pct >= 90 ? 'bg-(--error)' : r.pct >= 80 ? 'bg-(--warning)' : 'bg-(--accent)'}`}
                                                    style={{ width: `${Math.min(100, r.pct)}%` }}
                                                />
                                            </div>
                                        </td>
                                        <td className="py-2.5 pr-4 text-right tabular-nums text-(--base-07)">{formatBytes(r.rxBytes)}</td>
                                        <td className="py-2.5 text-right tabular-nums text-(--base-07)">{formatBytes(r.txBytes)}</td>
                                    </tr>
                                ))}
                            </tbody>
                        </table>
                    </section>}

                    {regions.length > 0 && <section className="card p-4 overflow-x-auto" aria-label="Voice per server">
                        {overview!.servers.length === 0 ? (
                            <p className="text-sm text-(--base-06) text-center py-4">No server has voice chat on or moved voice this month.</p>
                        ) : (
                            <table className="w-full text-sm">
                                <thead>
                                    <tr className="border-b border-(--base-03)">
                                        <th className="mono-label text-left pb-2 pr-4">Server</th>
                                        <th className="mono-label text-left pb-2 pr-4">Region</th>
                                        <th className="mono-label text-left pb-2 pr-4 hidden md:table-cell">Port</th>
                                        <th className="mono-label text-left pb-2 pr-4 hidden md:table-cell">Mod</th>
                                        <th className="mono-label text-right pb-2 pr-4">To server</th>
                                        <th className="mono-label text-right pb-2">To players</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {overview!.servers.map(s => (
                                        <tr key={`${s.serverId}|${s.region}`} className="border-b border-(--base-03)/50 last:border-0 hover:bg-(--base-03)/30 transition-colors">
                                            <td className="py-2.5 pr-4 text-(--base-09)">{s.serverName || `Server ${s.serverId}`}</td>
                                            <td className="py-2.5 pr-4 text-(--base-07)">{s.region}</td>
                                            <td className="py-2.5 pr-4 text-(--base-07) font-mono text-xs hidden md:table-cell">{s.port > 0 ? s.port : 'Off'}</td>
                                            <td className="py-2.5 pr-4 text-(--base-07) hidden md:table-cell">{MOD_LABEL[s.mod] || '-'}</td>
                                            <td className="py-2.5 pr-4 text-right tabular-nums text-(--base-07)">{formatBytes(s.rxBytes)}</td>
                                            <td className="py-2.5 text-right tabular-nums text-(--base-07)">{formatBytes(s.txBytes)}</td>
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        )}
                    </section>}

                    {bedrock.length > 0 && (
                        <section className="card p-4 overflow-x-auto" aria-label="Bedrock per server">
                            <span className="mono-label block mb-2">Bedrock players</span>
                            <table className="w-full text-sm">
                                <thead>
                                    <tr className="border-b border-(--base-03)">
                                        <th className="mono-label text-left pb-2 pr-4">Server</th>
                                        <th className="mono-label text-left pb-2 pr-4">Region</th>
                                        <th className="mono-label text-right pb-2 pr-4">To server</th>
                                        <th className="mono-label text-right pb-2">To players</th>
                                    </tr>
                                </thead>
                                <tbody>
                                    {bedrock.map(s => (
                                        <tr key={`${s.serverId}|${s.region}`} className="border-b border-(--base-03)/50 last:border-0 hover:bg-(--base-03)/30 transition-colors">
                                            <td className="py-2.5 pr-4 text-(--base-09)">{s.serverName || `Server ${s.serverId}`}</td>
                                            <td className="py-2.5 pr-4 text-(--base-07)">{s.region}</td>
                                            <td className="py-2.5 pr-4 text-right tabular-nums text-(--base-07)">{formatBytes(s.rxBytes)}</td>
                                            <td className="py-2.5 text-right tabular-nums text-(--base-07)">{formatBytes(s.txBytes)}</td>
                                        </tr>
                                    ))}
                                </tbody>
                            </table>
                        </section>
                    )}
                </>
            )}
        </div>
    );
}
