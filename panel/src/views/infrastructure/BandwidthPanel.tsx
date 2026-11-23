"use client";

import { useEffect, useMemo, useState, useCallback } from 'react';
import { AlertTriangle, Info, RefreshCw, ServerCrash } from 'lucide-react';
import {
  getGatewayBandwidthOverview,
  getGatewayBandwidthHistory,
  getGatewayRebalance,
  setWarpRebalanceMode,
  type RebalanceView,
} from '@/lib/api';
import BandwidthHostRow from './BandwidthRow';
import Select from '@/components/ui/Select';
import Segmented from '@/components/ui/Segmented';
import Section from '@/components/ui/Section';
import EmptyState from '@/components/ui/EmptyState';
import Callout from '@/components/server/Callout';
import { RowList, Row } from '@/components/ui/RowList';
import { SkeletonCard } from '@/components/Skeleton';
import {
  hostRows,
  seriesKey,
  summarizeAlerts,
  summarizeDecision,
  BANDWIDTH_RANGES,
  GATEWAY_KINDS,
  type BandwidthRange,
  type BandwidthHistory,
  type GatewayBandwidthOverview,
} from '@/lib/bandwidth';

/**
 * Bandwidth: one row per HOST, one column per gateway kind.
 *
 * The layout is the point. Edges, warp leaders and beam relays share machines
 * today and get their own later, and the question this screen answers - is this
 * uplink running out - is a question about the HOST, not about any one service
 * on it. So the cap and the total live in the rail on the left, once per row,
 * and the cells hold only what belongs to a single service.
 *
 * Deliberately bandwidth ONLY. What each component costs in CPU and RAM is the
 * Gateway tab; mixing the two put two unrelated judgements in one card.
 */


// Written out rather than composed, because a class Tailwind cannot read in the
// source is a class it never generates - the utilisation figure would simply
// lose its colour at exactly the percentages that need one.


// timeAgo renders a compact relative age for a unix-seconds timestamp (the
// rebalancer decision feed's `ts`, same unit as BandwidthHistoryPoint.ts).
function timeAgo(ts: number): string {
  const seconds = Math.max(0, Math.floor(Date.now() / 1000 - ts));
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

export default function BandwidthPanel() {
  const [overview, setOverview] = useState<GatewayBandwidthOverview | null>(null);
  const [range, setRange] = useState<BandwidthRange>('1h');
  const [history, setHistory] = useState<BandwidthHistory | null>(null);
  const [rebalance, setRebalance] = useState<RebalanceView | null>(null);
  const [savingMode, setSavingMode] = useState(false);
  const [modeError, setModeError] = useState<string | null>(null);
  // Set when the overview cannot be fetched and there is no earlier snapshot to
  // keep showing; otherwise the panel would sit on its loading state forever.
  const [loadFailed, setLoadFailed] = useState(false);

  const load = useCallback(async () => {
    try {
      setOverview(await getGatewayBandwidthOverview());
      setLoadFailed(false);
    } catch {
      // best-effort; keep the last snapshot
      setLoadFailed(true);
    }
    try {
      setRebalance(await getGatewayRebalance());
    } catch {
      // best-effort; keep the last snapshot
    }
  }, []);

  useEffect(() => {
    load();
    const t = setInterval(load, 10000);
    return () => clearInterval(t);
  }, [load]);

  // History is one request for every series, refreshed on the persist cadence
  // (Core writes a row per component every 30s, so polling faster only re-sends
  // the same points).
  useEffect(() => {
    let cancelled = false;
    const fetchHistory = () => {
      getGatewayBandwidthHistory(range)
        .then(res => { if (!cancelled) setHistory(res); })
        .catch(() => { if (!cancelled) setHistory(null); });
    };
    fetchHistory();
    const t = setInterval(fetchHistory, 30000);
    return () => { cancelled = true; clearInterval(t); };
  }, [range]);

  const handleModeChange = async (next: 'off' | 'dry-run' | 'armed') => {
    if (!rebalance || savingMode) return;
    const prevMode = rebalance.mode;
    setModeError(null);
    setSavingMode(true);
    setRebalance({ ...rebalance, mode: next });
    try {
      // fetchAPI does not throw on a non-2xx response - it resolves the parsed
      // body - so success has to be checked explicitly, same as every other
      // panel writer (GatewayTab.tsx, InfrastructureView.tsx, ...).
      const res = await setWarpRebalanceMode(next);
      if (res?.success !== true) {
        setRebalance((cur) => (cur ? { ...cur, mode: prevMode } : cur));
        setModeError(res?.message || 'Failed to update rebalancer mode.');
        return;
      }
      setRebalance(await getGatewayRebalance());
    } catch {
      setRebalance((cur) => (cur ? { ...cur, mode: prevMode } : cur));
      setModeError('Failed to update rebalancer mode.');
    } finally {
      setSavingMode(false);
    }
  };

  const rows = useMemo(
    () => hostRows(overview?.hosts ?? [], overview?.components ?? []),
    [overview],
  );

  // Every chart looks its history up by the same key its cell carries. Built in
  // one place so the two sides cannot drift into building it differently, which
  // would leave every chart silently empty.
  const compSeries = useMemo(() => {
    const m = new Map<string, { tx: number[]; rx: number[] }>();
    for (const s of history?.components ?? []) {
      m.set(seriesKey(s), { tx: s.points.map(p => p.txBps), rx: s.points.map(p => p.rxBps) });
    }
    return m;
  }, [history]);

  // The fallback scale, used only where no cap is configured. Where a cap IS
  // known each chart is drawn against it, so a height means the same share of
  // the link in every box on the screen.
  const fallbackScale = useMemo(() => {
    let max = 0;
    for (const s of history?.components ?? []) {
      for (const p of s.points) {
        if (p.txBps > max) max = p.txBps;
        if (p.rxBps > max) max = p.rxBps;
      }
    }
    return max;
  }, [history]);

  if (!overview) {
    if (loadFailed) {
      return (
        <EmptyState
          tone="error"
          icon={<ServerCrash size={20} />}
          title="Could not load bandwidth telemetry"
          description="The gateway did not answer. Check that its components are running, then try again."
          action={<button type="button" onClick={load} className="btn btn-secondary btn-sm"><RefreshCw size={14} /> Retry</button>}
        />
      );
    }
    return (
      <div className="flex flex-col gap-4" aria-busy="true">
        <SkeletonCard height="h-32" />
        <SkeletonCard height="h-56" />
      </div>
    );
  }

  const bellItems = summarizeAlerts(overview.alerts);

  // Rebalancer mode control + recent decisions. Independent of whether any
  // gateway component is currently reporting telemetry, so it renders in
  // both the "no components" empty state and the normal overview below.
  const rebalancerSection = rebalance && (
    <Section
      title="Warp rebalancer"
      description="Moves peers away from saturated warp leaders."
      actions={
        <Select
          ariaLabel="Warp rebalancer mode"
          value={rebalance.mode}
          onChange={(v) => handleModeChange(v as 'off' | 'dry-run' | 'armed')}
          disabled={savingMode}
          className="w-32"
          options={[
            { value: 'off', label: 'Off' },
            { value: 'dry-run', label: 'Dry run' },
            { value: 'armed', label: 'Armed' },
          ]}
        />
      }
    >
      <div className="srv-surface">
        {rebalance.mode === 'dry-run' && (
          <p className="px-4 pt-3 text-[13px] text-(--base-06)">Computing only, no moves applied.</p>
        )}
        {rebalance.mode === 'armed' && (
          <p className="px-4 pt-3 text-[13px] text-(--base-06)">Moves are applied automatically to relieve saturated warp leaders.</p>
        )}
        {modeError && <p role="alert" className="px-4 pt-3 text-[13px] text-(--error-light)">{modeError}</p>}
        {rebalance.decisions.length === 0 ? (
          <p className="px-4 py-4 text-[13px] text-(--base-06)">No recent rebalancer activity</p>
        ) : (
          <RowList label="Recent rebalancer decisions">
            {rebalance.decisions.map((d, i) => (
              <Row
                key={`${d.ts}-${i}`}
                title={<span className="font-normal text-(--base-08)">{summarizeDecision(d)}</span>}
                trailing={<span className="srv-num">{timeAgo(d.ts)}</span>}
              />
            ))}
          </RowList>
        )}
      </div>
    </Section>
  );

  if (rows.length === 0) {
    return (
      <div className="flex flex-col gap-4">
        {rebalancerSection}
        <EmptyState
          icon={<Info size={20} />}
          title="No gateway components reporting"
          description="Edges, beam relays and warp leaders publish bandwidth once BANDWIDTH_MBIT is set and traffic flows."
        />
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-4">
      {rebalancerSection}

      {bellItems.length > 0 && (
        <div className="flex flex-col gap-2">
          {bellItems.map((it) => (
            <Callout key={it.id} tone="warning" icon={<AlertTriangle size={16} />} title={it.title}>{it.message}</Callout>
          ))}
        </div>
      )}

      <Section
        title="Throughput by host"
        description="Outbound above the line, inbound below it."
        actions={<RangePicker range={range} onChange={setRange} />}
      >
        {history?.note && <Callout tone="info" icon={<Info size={16} />}>{history.note}</Callout>}
      </Section>

      {/* One row per host, one column per kind. The charts line up down the
          screen, which is what makes two hosts comparable at a glance. */}
      <div className="flex flex-col gap-3">
        {rows.map(({ host, cells }) => (
          <BandwidthHostRow
            key={host.host}
            host={host}
            kinds={GATEWAY_KINDS}
            cells={cells}
            seriesFor={c => compSeries.get(seriesKey(c)) ?? { tx: [], rx: [] }}
            scaleBps={fallbackScale}
            range={range}
          />
        ))}
      </div>

    </div>
  );
}

function RangePicker({ range, onChange }: { range: BandwidthRange; onChange: (r: BandwidthRange) => void }) {
  return (
    <Segmented options={BANDWIDTH_RANGES.map(r => ({ id: r, label: r }))} value={range} onChange={onChange} ariaLabel="Time range" />
  );
}

