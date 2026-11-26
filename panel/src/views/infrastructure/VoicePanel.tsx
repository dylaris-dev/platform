"use client";

import { useEffect, useState } from 'react';
import { AlertTriangle, Mic } from 'lucide-react';
import { SkeletonCard, SkeletonTable } from '@/components/Skeleton';
import { formatBytes } from '@/components/infra/TrafficPools';
import { Meter, MeterIndicator, MeterTrack } from '@ui-sdk/react/meter';
import Section from '@/components/ui/Section';
import EmptyState from '@/components/ui/EmptyState';
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

// Quiet rows with fixed columns, so the header and every row line up.
const REGION_GRID = 'grid grid-cols-[minmax(6rem,1fr)_6rem_minmax(10rem,1.5fr)_6rem_6rem] items-center gap-4';
const SERVER_GRID = 'grid grid-cols-[minmax(8rem,1.5fr)_minmax(5rem,1fr)_4rem_minmax(8rem,1fr)_6rem_6rem] items-center gap-4';
const BEDROCK_GRID = 'grid grid-cols-[minmax(8rem,1.5fr)_minmax(5rem,1fr)_6rem_6rem] items-center gap-4';

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
        <div className="flex flex-col gap-6">
            <Section
                title="Voice chat"
                description="Relays, port pools and voice traffic for one month."
                actions={
                    <label className="flex items-center gap-2">
                        <span className="srv-label">Month</span>
                        <input
                            type="month"
                            value={period}
                            onChange={e => setPeriod(e.target.value || currentMonth())}
                            className="input-field w-44"
                        />
                    </label>
                }
            >
                {loading && !overview ? (
                    <div className="flex flex-col gap-3" aria-busy="true">
                        <SkeletonCard height="h-32" />
                        <SkeletonTable />
                    </div>
                ) : error ? (
                    <EmptyState tone="error" icon={<AlertTriangle size={20} />} title="Could not load voice chat" description={error} />
                ) : regions.length === 0 && bedrock.length === 0 ? (
                    <EmptyState icon={<Mic size={20} />} title="No voice relay is running" description="No server has voice chat on either." />
                ) : (
                    <div className="flex flex-col gap-3">
                        {regions.length > 0 && (
                            <section className="srv-surface overflow-x-auto" aria-label="Voice per region">
                                <div className="min-w-[36rem] p-1.5">
                                    <div className={`${REGION_GRID} srv-label px-3 pb-1 pt-2`}>
                                        <span>Region</span><span>Relays</span><span>Ports</span>
                                        <span className="text-right">To servers</span><span className="text-right">To players</span>
                                    </div>
                                    {regions.map(r => (
                                        <div key={r.region} className={`app-row ${REGION_GRID}`}>
                                            <span className="truncate text-sm font-medium text-(--base-09)">{r.region}</span>
                                            <span className={`srv-num text-sm ${r.relays === 0 ? 'text-(--warning-light)' : 'text-(--base-07)'}`}>
                                                {r.relays === 0 ? 'None running' : r.relays}
                                            </span>
                                            <div className="min-w-0 space-y-1.5">
                                                <div className="srv-num text-xs text-(--base-07)">
                                                    {r.portsUsed} of {r.portsTotal} ({r.pct}%)
                                                </div>
                                                <Meter value={Math.min(100, r.pct)} min={0} max={100} aria-label={`${r.region}: ${r.portsUsed} of ${r.portsTotal} voice ports held`}>
                                                    <MeterTrack className="srv-meter-track">
                                                        <MeterIndicator className="srv-meter-fill" data-tone={r.pct >= 90 ? 'error' : r.pct >= 80 ? 'warning' : undefined} />
                                                    </MeterTrack>
                                                </Meter>
                                            </div>
                                            <span className="srv-num text-right text-sm text-(--base-07)">{formatBytes(r.rxBytes)}</span>
                                            <span className="srv-num text-right text-sm text-(--base-07)">{formatBytes(r.txBytes)}</span>
                                        </div>
                                    ))}
                                </div>
                            </section>
                        )}

                        {regions.length > 0 && (
                            <section className="srv-surface overflow-x-auto" aria-label="Voice per server">
                                {overview!.servers.length === 0 ? (
                                    <p className="py-6 text-center text-[13px] text-(--base-06)">No server has voice chat on or moved voice this month.</p>
                                ) : (
                                    <div className="min-w-[36rem] p-1.5">
                                        <div className={`${SERVER_GRID} srv-label px-3 pb-1 pt-2`}>
                                            <span>Server</span><span>Region</span><span>Port</span><span>Mod</span>
                                            <span className="text-right">To server</span><span className="text-right">To players</span>
                                        </div>
                                        {overview!.servers.map(s => (
                                            <div key={`${s.serverId}|${s.region}`} className={`app-row ${SERVER_GRID}`}>
                                                <span className="truncate text-sm text-(--base-09)">{s.serverName || `Server ${s.serverId}`}</span>
                                                <span className="truncate text-sm text-(--base-07)">{s.region}</span>
                                                <span className="srv-num text-xs text-(--base-07)">{s.port > 0 ? s.port : 'Off'}</span>
                                                <span className="truncate text-sm text-(--base-07)">{MOD_LABEL[s.mod] || '-'}</span>
                                                <span className="srv-num text-right text-sm text-(--base-07)">{formatBytes(s.rxBytes)}</span>
                                                <span className="srv-num text-right text-sm text-(--base-07)">{formatBytes(s.txBytes)}</span>
                                            </div>
                                        ))}
                                    </div>
                                )}
                            </section>
                        )}

                        {bedrock.length > 0 && (
                            <section className="srv-surface overflow-x-auto" aria-label="Bedrock per server">
                                <div className="min-w-[30rem] p-1.5">
                                    <div className="srv-label px-3 pb-1 pt-2 text-[13px] font-semibold text-(--base-09)">Bedrock players</div>
                                    <div className={`${BEDROCK_GRID} srv-label px-3 pb-1`}>
                                        <span>Server</span><span>Region</span>
                                        <span className="text-right">To server</span><span className="text-right">To players</span>
                                    </div>
                                    {bedrock.map(s => (
                                        <div key={`${s.serverId}|${s.region}`} className={`app-row ${BEDROCK_GRID}`}>
                                            <span className="truncate text-sm text-(--base-09)">{s.serverName || `Server ${s.serverId}`}</span>
                                            <span className="truncate text-sm text-(--base-07)">{s.region}</span>
                                            <span className="srv-num text-right text-sm text-(--base-07)">{formatBytes(s.rxBytes)}</span>
                                            <span className="srv-num text-right text-sm text-(--base-07)">{formatBytes(s.txBytes)}</span>
                                        </div>
                                    ))}
                                </div>
                            </section>
                        )}
                    </div>
                )}
            </Section>
        </div>
    );
}
