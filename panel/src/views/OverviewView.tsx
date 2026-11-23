"use client";

import { useState, useEffect, useMemo, useRef } from 'react';
import { AreaChart, Area, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid, ReferenceLine } from 'recharts';
import { Meter, MeterIndicator, MeterTrack } from '@ui-sdk/react/meter';
import {
  Server, ServerStats, DiskUsage, BackupConfig, BackupUsage,
  getStatsHistory, getDiskUsage, getBackupConfig, getBackupUsage,
} from '@/lib/api';
import { subscribeEventSource } from '@/lib/sse';
import { latestValue, chartSeries, appendSamples } from '@/lib/statsSeries';
import { jvmHeapMB } from '@/lib/jvmHeap';
import { parseStatsSample } from '@/lib/statsSample';
import MemoryHelp from '@/components/MemoryHelp';
import { Skeleton } from '@/components/Skeleton';
import CoreLoadBars from '@/components/CoreLoadBars';
import Callout from '@/components/server/Callout';
import Segmented from '@/components/ui/Segmented';
import { stateDot, stateWord } from '@/components/server/ServerHeader';
import { useShownStatus } from '@/components/server/ShownStatus';
import { formatBytes, formatMB } from '@/lib/format';
import { fmtDateTime, timeAgo } from '@/lib/time';

import { AlertTriangle, PowerOff } from 'lucide-react';

function formatTime(ts: number): string {
  const d = new Date(ts * 1000);
  return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}:${d.getSeconds().toString().padStart(2, '0')}`;
}

function formatTimeShort(ts: number): string {
  const d = new Date(ts * 1000);
  return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}`;
}

const RANGE_SEC: Record<string, number> = { '1h': 3600, '6h': 6 * 3600, '12h': 12 * 3600, '24h': 24 * 3600 };
const RANGE_OPTIONS = ['1h', '6h', '12h', '24h'].map(id => ({ id, label: id }));
const MODE_OPTIONS = [{ id: 'live' as const, label: 'Live' }, { id: 'history' as const, label: 'History' }];
// Wider than one sampling interval by a margin: live ticks every 2 s, history
// rows every 2 min. Anything longer is time the server was not reporting.
const LIVE_GAP_SEC = 10;
const HISTORY_GAP_SEC = 300;
// How long live samples collect before they are drawn.
const STATS_BATCH_MS = 250;

// Chart ink: steel for the measured series, a quieter grey for the container
// total. Violet stays reserved for selection and the primary action.
const INK_PRIMARY = 'var(--primary-light)';
const INK_QUIET = 'var(--base-06)';

// The `state` event Core sends first on the stats stream.
interface StreamState {
  live: boolean;
  lastSampleAt: number | null; // unix ms
}

// Warns from 75 %, alarms from 90 %: the meter turns amber, then red.
function toneFor(pct: number): 'warning' | 'error' | undefined {
  return pct >= 90 ? 'error' : pct >= 75 ? 'warning' : undefined;
}

function BoundedMeter({ label, value, max, tone }: { label: string; value: number; max: number; tone?: 'warning' | 'error' }) {
  const safeMax = max > 0 ? max : 1;
  return (
    <Meter value={Math.min(Math.max(value, 0), safeMax)} min={0} max={safeMax} aria-label={label}>
      <MeterTrack className="srv-meter-track">
        <MeterIndicator className="srv-meter-fill" data-tone={tone} />
      </MeterTrack>
    </Meter>
  );
}

function Kpi({ label, value, unit, sub, meter, lead }: {
  label: string;
  value: React.ReactNode;
  unit?: string;
  sub?: React.ReactNode;
  meter?: React.ReactNode;
  lead?: React.ReactNode;
}) {
  return (
    <div className="srv-surface flex min-w-0 flex-col gap-3 p-5">
      <span className="srv-label">{label}</span>
      <div className="flex items-baseline gap-2">
        {lead}
        <span className="srv-num truncate text-[28px] font-medium leading-none text-(--base-09)">{value}</span>
        {unit && <span className="srv-num text-sm text-(--base-06)">{unit}</span>}
      </div>
      {meter ?? <div className="h-1.5" aria-hidden="true" />}
      <div className="min-h-4 truncate text-xs text-(--base-06)">{sub}</div>
    </div>
  );
}

interface ChartCardProps {
  label: string;
  currentValue: React.ReactNode;
  legend?: React.ReactNode;
  children: React.ReactNode;
}

function ChartCard({ label, currentValue, legend, children }: ChartCardProps) {
  return (
    <div className="srv-surface min-w-0 overflow-hidden p-5">
      {/* Wraps rather than shrinking the label: a long value (heap plus
          process) on a half-width card used to slide over it. */}
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
        <h2 className="text-sm font-medium text-(--base-09)">{label}</h2>
        <span className="srv-num ml-auto whitespace-nowrap text-sm text-(--base-08)">{currentValue}</span>
      </div>
      {legend && <div className="mt-1 flex flex-wrap items-center gap-x-4 gap-y-1">{legend}</div>}
      <div className="-mx-2 mt-3 h-48">
        {children}
      </div>
    </div>
  );
}

function LegendItem({ color, label, dashed }: { color: string; label: string; dashed?: boolean }) {
  return (
    <span className="flex items-center gap-1.5 text-xs text-(--base-06)">
      <span
        className="inline-block w-3 shrink-0"
        style={dashed ? { borderTop: `1px dashed ${color}` } : { height: 2, borderRadius: 1, backgroundColor: color }}
      />
      {label}
    </span>
  );
}

interface Segment { key: string; label: string; bytes: number; color: string }

// One storage card: a Meter whose track holds a segment per consumer, the
// legend underneath. Used for the server's disk and for node-local backups.
function StorageCard({ title, used, limit, capacity, segments, freeBytes, note }: {
  title: string;
  used: number;
  limit: number;
  capacity: number;
  segments: Segment[];
  freeBytes: number;
  note?: React.ReactNode;
}) {
  const pct = capacity > 0 ? (used / capacity) * 100 : 0;
  return (
    <div className="srv-surface p-5">
      <div className="mb-4 flex items-baseline justify-between gap-3">
        <h2 className="text-sm font-medium text-(--base-09)">{title}</h2>
        <span className="srv-num text-sm text-(--base-09)">
          {formatBytes(used)}
          {limit > 0 && <span className="text-(--base-06)"> / {formatBytes(limit)}</span>}
        </span>
      </div>
      {note}
      <Meter value={Math.min(used, capacity > 0 ? capacity : used)} min={0} max={capacity > 0 ? capacity : 1} aria-label={title}>
        <MeterTrack className="srv-meter-track h-2">
          {segments.map(seg => {
            const w = capacity > 0 ? (seg.bytes / capacity) * 100 : 0;
            if (w < 0.3) return null;
            return (
              <div
                key={seg.key}
                title={`${seg.label}: ${formatBytes(seg.bytes)}`}
                style={{ width: `${Math.min(w, 100)}%`, backgroundColor: seg.color, minWidth: '2px' }}
                className="h-full transition-[width] duration-300 ease-out motion-reduce:transition-none"
              />
            );
          })}
        </MeterTrack>
      </Meter>
      <span className="sr-only">{Math.round(pct)}% used</span>
      <div className="mt-4 flex flex-wrap gap-x-6 gap-y-2">
        {segments.map(seg => (
          <div key={seg.key} className="flex items-center gap-2 text-sm">
            <span className="size-2.5 shrink-0 rounded-sm" style={{ backgroundColor: seg.color }} />
            <span className="text-(--base-08)">{seg.label}</span>
            <span className="srv-num text-xs text-(--base-06)">{formatBytes(seg.bytes)}</span>
          </div>
        ))}
        {limit > 0 && freeBytes > 0 && (
          <div className="flex items-center gap-2 text-sm">
            <span className="size-2.5 shrink-0 rounded-sm bg-(--base-03) ring-1 ring-(--base-04)" />
            <span className="text-(--base-06)">Free</span>
            <span className="srv-num text-xs text-(--base-06)">{formatBytes(freeBytes)}</span>
          </div>
        )}
      </div>
    </div>
  );
}

// Steel and neutral ramp: segments need telling apart, not decorating.
const SEGMENT_COLORS = [
  'var(--primary)',
  'var(--primary-light)',
  'var(--success-light)',
  'var(--base-06)',
  'var(--warning)',
  'var(--base-08)',
  'var(--success)',
  'var(--base-05)',
];
const BACKUP_COLOR = 'var(--base-08)';

interface OverviewViewProps {
  server: Server;
}

export default function OverviewView({ server }: OverviewViewProps) {
  const [mode, setMode] = useState<'live' | 'history'>('live');
  const [liveData, setLiveData] = useState<ServerStats[]>([]);
  // The live series for the KPI tiles, which stay current whichever chart mode is shown.
  const [kpiSeries, setKpiSeries] = useState<ServerStats[]>([]);
  const [streamState, setStreamState] = useState<StreamState | null>(null);
  const [historyData, setHistoryData] = useState<ServerStats[]>([]);
  const [historyRange, setHistoryRange] = useState('24h');
  const [historyAt, setHistoryAt] = useState(() => Date.now());
  const [diskUsage, setDiskUsage] = useState<DiskUsage | null>(null);
  // Until the first answer the disk card's place is held by a skeleton, so the
  // page does not jump when it arrives.
  const [diskCheckedFor, setDiskCheckedFor] = useState<number | null>(null);
  const [backupConfig, setBackupConfig] = useState<BackupConfig | null>(null);
  const [backupUsage, setBackupUsage] = useState<BackupUsage | null>(null);

  const shown = useShownStatus(server.status);
  const isOffline = server.status === 'stopped' || server.status === 'offline' || server.status === 'pending_setup';

  // Live samples keep arriving while the history is shown, but only reach
  // the chart state (and re-render the charts) while the live view is shown.
  const liveRef = useRef<ServerStats[]>([]);
  const modeRef = useRef(mode);
  modeRef.current = mode;
  const showLive = (next: ServerStats[]) => {
    liveRef.current = next;
    setKpiSeries(next);
    if (modeRef.current === 'live') setLiveData(next);
  };
  useEffect(() => {
    if (mode === 'live') setLiveData(liveRef.current);
  }, [mode]);

  useEffect(() => {
    showLive([]);
    setStreamState(null);
    // Samples are applied in batches: a stream opening replays the buffer,
    // and taking each sample as its own render redrew both charts per sample.
    let batch: ServerStats[] = [];
    let timer: ReturnType<typeof setTimeout> | null = null;
    const flushBatch = () => {
      timer = null;
      const samples = batch;
      batch = [];
      if (samples.length === 0) return;
      showLive(appendSamples(liveRef.current, samples));
      // A sample arriving is the server reporting again.
      setStreamState(s => (s && !s.live ? { live: true, lastSampleAt: Date.now() } : s));
    };

    const stop = subscribeEventSource(`/servers/${server.id}/stats/stream`, (e) => {
      try {
        const s = parseStatsSample(e.data);
        if (!s) return;
        batch.push(s);
        timer ??= setTimeout(flushBatch, STATS_BATCH_MS);
      } catch { /* ignore */ }
    }, {
      // Sent first on every (re)connect. Not live means the buffer is old: what
      // the chart would hold is a stopped server's last hour, not the present.
      state: (e) => {
        try {
          const s = JSON.parse(e.data) as { live?: boolean; lastSampleAt?: number | null };
          const live = s.live === true;
          setStreamState({ live, lastSampleAt: typeof s.lastSampleAt === 'number' ? s.lastSampleAt : null });
          if (!live) {
            batch = [];
            showLive([]);
          }
        } catch { /* ignore */ }
      },
    }, { pauseWhenHidden: true });
    return () => {
      if (timer) clearTimeout(timer);
      stop();
    };
  }, [server.id]);

  // A stop while watching: drop the live points so a restart does not draw a
  // line from the old ones to the new.
  useEffect(() => {
    if (isOffline) showLive([]);
  }, [isOffline]);

  useEffect(() => {
    if (mode !== 'history') return;
    getStatsHistory(server.id, historyRange).then((res: { points?: ServerStats[] }) => {
      setHistoryData(res.points || []);
      setHistoryAt(Date.now());
    }).catch(() => setHistoryData([]));
  }, [mode, historyRange, server.id]);

  useEffect(() => {
    getDiskUsage(server.id).then((data: DiskUsage) => {
      if (data && typeof data.total === 'number') setDiskUsage(data);
    }).catch(() => {}).finally(() => setDiskCheckedFor(server.id));

    const interval = setInterval(() => {
      getDiskUsage(server.id).then((data: DiskUsage) => {
        if (data && typeof data.total === 'number') setDiskUsage(data);
      }).catch(() => {});
    }, 15000);
    return () => clearInterval(interval);
  }, [server.id]);

  // Backup config is a global setting — fetch once on mount. Backup
  // usage per server only matters when mode == "node-local"; refresh
  // it on the same cadence as disk usage so the two stay coherent in
  // the rendered storage card.
  useEffect(() => {
    getBackupConfig().then((res) => {
      if (res.success && res.settings) setBackupConfig(res.settings);
    }).catch(() => {});
  }, []);

  useEffect(() => {
    if (!backupConfig || backupConfig.mode !== 'node-local') {
      setBackupUsage(null);
      return;
    }
    const tick = () => {
      getBackupUsage(server.id).then((u) => {
        if (u && u.success) setBackupUsage(u);
      }).catch(() => {});
    };
    tick();
    const interval = setInterval(tick, 15000);
    return () => clearInterval(interval);
  }, [server.id, backupConfig]);

  const rawData = mode === 'live' ? liveData : historyData;
  // Memoised so the KPI ticks, which re-render this view while History is
  // shown, do not hand the charts a new array to redraw.
  const chartData = useMemo(
    () => chartSeries(rawData, mode === 'live' ? LIVE_GAP_SEC : HISTORY_GAP_SEC),
    [rawData, mode],
  );
  const timeFormatter = mode === 'live' ? formatTime : formatTimeShort;
  // History is scaled to the whole range, so the hours the server was off
  // show as empty space instead of being squeezed out.
  const historyEnd = Math.floor(historyAt / 1000);
  const xDomain: [number | string, number | string] = mode === 'history'
    ? [historyEnd - (RANGE_SEC[historyRange] ?? RANGE_SEC['24h']), historyEnd]
    : ['dataMin', 'dataMax'];

  const latestCpu = rawData.length > 0 ? (rawData[rawData.length - 1].cpu ?? 0) : 0;
  // Two different measurements, both shown: the container's memory (what the
  // host and the OOM killer see; with Xms=Xmx it sits near the heap size) and
  // the post-GC JVM heap (what the server actually holds live). The heap is
  // absent on Java 8 and before the first GC.
  const hasHeap = rawData.some(d => typeof d.javaHeapUsed === 'number' && d.javaHeapUsed > 0);
  const latestMemMb = latestValue(rawData, 'memUsed');
  // The LAST sample that carries a value, not simply the last sample: the heap
  // field is omitted on any tick where no fresh GC reading was available.
  const latestHeapMb = latestValue(rawData, 'javaHeapUsed');
  // The node always appends its own -Xmx last, so this is the heap's ceiling.
  const heapMaxMb = hasHeap && server.memory > 0 ? jvmHeapMB(server.memory) : 0;
  const memLimitMb = Math.max(latestValue(rawData, 'memLimit'), server.memory || 0);

  const liveOffline = mode === 'live' && liveData.length === 0 && (isOffline || streamState?.live === false);
  const lastSampleAt = streamState?.lastSampleAt ?? null;
  const hasChart = !liveOffline && rawData.length > 1;

  // KPI figures, always from the live series.
  const kpiLast = kpiSeries.length > 0 ? kpiSeries[kpiSeries.length - 1] : null;
  const kpiOffline = kpiSeries.length === 0 && (isOffline || streamState?.live === false);
  const kpiHasHeap = kpiSeries.some(d => typeof d.javaHeapUsed === 'number' && d.javaHeapUsed > 0);
  const kpiHeapMb = latestValue(kpiSeries, 'javaHeapUsed');
  const kpiMemMb = latestValue(kpiSeries, 'memUsed');
  const kpiLimitMb = Math.max(latestValue(kpiSeries, 'memLimit'), server.memory || 0);
  const kpiHeapMax = kpiHasHeap && server.memory > 0 ? jvmHeapMB(server.memory) : 0;
  const kpiMemUsed = kpiHasHeap && kpiHeapMax > 0 ? kpiHeapMb : kpiMemMb;
  const kpiMemMax = kpiHasHeap && kpiHeapMax > 0 ? kpiHeapMax : kpiLimitMb;
  const kpiCpu = kpiLast ? (kpiLast.cpu ?? 0) : 0;
  const hasPlayers = !!kpiLast && typeof kpiLast.players === 'number';

  const tooltipStyle = {
    backgroundColor: 'var(--base-02)',
    border: '1px solid var(--base-03)',
    borderRadius: 'var(--radius-xl)',
    fontSize: '12px',
    color: 'var(--base-09)',
    boxShadow: 'var(--shadow-lg)',
    padding: '8px 12px',
  };

  const emptyState = liveOffline ? (
    <div className="h-full flex flex-col items-center justify-center gap-1.5 text-center px-4">
      <PowerOff size={20} className="text-(--base-05)" />
      <span className="text-sm text-(--base-08)">Server offline</span>
      <span className="text-xs text-(--base-06)">
        {lastSampleAt
          ? `Last sample ${fmtDateTime(lastSampleAt)} (${timeAgo(new Date(lastSampleAt).toISOString())})`
          : 'No recent samples'}
      </span>
    </div>
  ) : (
    <div className="h-full flex items-center justify-center text-(--base-06) text-sm">
      {mode === 'history' ? 'No samples in this range' : isOffline ? 'Server offline' : 'Waiting for data...'}
    </div>
  );

  const axisTick = { fontSize: 11, fill: 'var(--base-05)' };
  const xAxis = (
    <XAxis
      dataKey="ts"
      type="number"
      scale="time"
      domain={xDomain}
      tickFormatter={timeFormatter}
      tick={axisTick}
      axisLine={false}
      tickLine={false}
      minTickGap={32}
    />
  );
  const grid = <CartesianGrid stroke="var(--base-03)" strokeOpacity={0.6} vertical={false} />;
  const cursor = { stroke: 'var(--base-05)', strokeWidth: 1, strokeDasharray: '4 4' };

  const players = hasPlayers ? kpiLast!.players : 0;
  const maxPlayers = hasPlayers ? kpiLast!.maxPlayers : 0;

  // Storage, shared by the disk card and the folded-backup variant.
  const diskCard = diskUsage && (() => {
    const sorted = diskUsage.subServers
      ? Object.entries(diskUsage.subServers).sort(([, a], [, b]) => b - a)
      : [];

    // When mode is "node-local" AND the admin folded the backup budget into the
    // server quota, render the backup bytes as an extra segment of the same
    // bar. Otherwise the backup card (below) handles its own bar. The combined
    // path is the one the docs warn needs XFS/ZFS project quotas to actually
    // enforce; one bar reflects the operator intent even when the cap is still
    // application-level.
    const folded = backupConfig?.mode === 'node-local'
      && backupConfig.shareQuotaWithServer
      && backupUsage
      && !backupUsage.degraded;
    const foldedBackupBytes = folded ? (backupUsage?.usedBytes ?? 0) : 0;

    const total = diskUsage.total + foldedBackupBytes;
    const capacity = diskUsage.limit > 0 ? diskUsage.limit : total;
    const freeBytes = diskUsage.limit > 0 ? Math.max(diskUsage.limit - total, 0) : 0;

    const segments: Segment[] = sorted.map(([name, bytes], i) => ({
      key: name, label: name, bytes, color: SEGMENT_COLORS[i % SEGMENT_COLORS.length],
    }));
    if (folded && foldedBackupBytes > 0) {
      segments.push({ key: '__backups', label: 'Backups', bytes: foldedBackupBytes, color: BACKUP_COLOR });
    }

    // What differs on a filesystem without project quotas is HOW the limit is
    // held, not whether it is: the disk guard still measures (du instead of a
    // quota read), still stops the server at the limit, and Core still refuses
    // to start it again until usage drops (node/disk_guard.go). What is missing
    // is the filesystem's own hard stop, so a server can overshoot between two
    // scans, which run every few minutes. Saying "not enforced" would be a lie;
    // the useful fact is the overshoot. Explicitly === false: a node too old to
    // send the field leaves it undefined, and that must not read as a warning.
    const note = diskUsage.limit > 0 && diskUsage.enforceable === false ? (
      <div className="mb-4">
        <Callout tone="warning" icon={<AlertTriangle size={16} />}>
          This storage cannot hold the limit itself (disk quotas need xfs or ext4), so
          the server is stopped by a periodic check instead. Usage is measured every few
          minutes and can briefly go over the limit before that happens.
        </Callout>
      </div>
    ) : undefined;

    return (
      <StorageCard
        title={folded ? 'Disk usage (incl. backups)' : 'Disk usage'}
        used={total}
        limit={diskUsage.limit}
        capacity={capacity}
        segments={segments}
        freeBytes={freeBytes}
        note={note}
      />
    );
  })();

  // Separate "Backups" card: only when mode is "node-local" and the quota is
  // NOT shared with server storage. The quota comes from the global
  // BackupConfig; the used bytes from the per-server backup-usage RPC.
  const backupCard = backupConfig?.mode === 'node-local'
    && !backupConfig.shareQuotaWithServer
    && backupUsage
    && !backupUsage.degraded
    && (() => {
      const used = backupUsage.usedBytes;
      // backupConfig.quotaPerServerGb is the GLOBAL cap: null = no cap,
      // 0 = none allowed. A cap of 0 has to render as a real ceiling, not as
      // "unlimited" - the bar would otherwise say a server with backups
      // disabled has room.
      const quota = backupConfig.quotaPerServerGb;
      const limit = quota !== null ? quota * 1024 ** 3 : 0;
      const capacity = limit > 0 ? limit : Math.max(used, 1);
      const freeBytes = limit > 0 ? Math.max(limit - used, 0) : 0;
      return (
        <StorageCard
          title="Backups"
          used={used}
          limit={limit}
          capacity={capacity}
          segments={[{
            key: 'archives',
            label: `${backupUsage.count} ${backupUsage.count === 1 ? 'archive' : 'archives'}`,
            bytes: used,
            color: BACKUP_COLOR,
          }]}
          freeBytes={freeBytes}
        />
      );
    })();

  return (
    <div className="flex flex-col gap-6">
      {/* An install that cannot progress. Without this the page shows an
          "installing" badge and a spinner that never resolves, which on BYON -
          where the node is somebody's home PC - is an ordinary support ticket.
          The state is not wrong, it was just never explained. */}
      {server.installStalled && (
        <Callout tone="warning" icon={<AlertTriangle size={16} />} title="Installation paused">
          {server.installStallReason || 'The node running this install is offline.'}
        </Callout>
      )}

      {/* Disk limit warning, above everything it affects. */}
      {diskUsage?.warning && diskUsage.limit > 0 && (
        <Callout
          tone={diskUsage.warning === '80' ? 'warning' : 'error'}
          icon={<AlertTriangle size={16} />}
          role="status"
          title={diskUsage.warning === 'full'
            ? 'Storage limit reached'
            : diskUsage.warning === '90'
              ? 'Storage limit critical'
              : 'Storage running low'}
        >
          {Math.round((diskUsage.total / diskUsage.limit) * 100)}% used — {formatBytes(diskUsage.total)} of {formatBytes(diskUsage.limit)}
        </Callout>
      )}

      {/* KPI row: 4 / 2 / 1 columns. */}
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <Kpi
          label="State"
          lead={<span className={`size-2.5 shrink-0 self-center rounded-full ${stateDot(shown)}`} aria-hidden="true" />}
          value={<span className="font-body">{stateWord(shown)}</span>}
          sub={kpiOffline && lastSampleAt
            ? `Last sample ${timeAgo(new Date(lastSampleAt).toISOString())}`
            : server.nodeName}
        />
        <Kpi
          label="Players online"
          value={hasPlayers && !kpiOffline ? players : '—'}
          unit={hasPlayers && !kpiOffline && maxPlayers > 0 ? `/ ${maxPlayers}` : undefined}
          meter={<BoundedMeter label="Players online" value={hasPlayers && !kpiOffline ? players : 0} max={maxPlayers} />}
          sub={hasPlayers && !kpiOffline ? undefined : 'No live sample'}
        />
        <Kpi
          label="CPU"
          value={kpiLast && !kpiOffline ? kpiCpu.toFixed(1) : '—'}
          unit={kpiLast && !kpiOffline ? '%' : undefined}
          meter={<BoundedMeter label="CPU" value={kpiOffline ? 0 : kpiCpu} max={100} tone={toneFor(kpiCpu)} />}
          sub={server.cpuLimit ? `Limit ${server.cpuLimit} cores` : 'No CPU limit'}
        />
        <Kpi
          label={kpiHasHeap && kpiHeapMax > 0 ? 'Memory (heap)' : 'Memory'}
          value={kpiLast && !kpiOffline ? formatMB(kpiMemUsed) : '—'}
          unit={kpiLast && !kpiOffline && kpiMemMax > 0 ? `/ ${formatMB(kpiMemMax)}` : undefined}
          meter={<BoundedMeter label="Memory" value={kpiOffline ? 0 : kpiMemUsed} max={kpiMemMax} tone={kpiMemMax > 0 ? toneFor((kpiMemUsed / kpiMemMax) * 100) : undefined} />}
          sub={kpiHasHeap && kpiHeapMax > 0 && kpiLast && !kpiOffline ? `Process ${formatMB(kpiMemMb)} / ${formatMB(kpiLimitMb)}` : undefined}
        />
      </div>

      {/* Chart controls */}
      <div className="flex flex-wrap items-center gap-3">
        <Segmented options={MODE_OPTIONS} value={mode} onChange={setMode} ariaLabel="Chart mode" />
        {mode === 'history' && (
          <Segmented options={RANGE_OPTIONS} value={historyRange} onChange={setHistoryRange} ariaLabel="History range" />
        )}
        <span className="srv-num ml-auto text-xs text-(--base-06)">
          {mode === 'live' ? `${liveData.length} pts` : `${historyData.length} pts`}
        </span>
      </div>

      {/* Charts */}
      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <ChartCard
          label="CPU"
          currentValue={!liveOffline && rawData.length > 0 ? `${latestCpu.toFixed(1)}%` : '—'}
        >
          {hasChart ? (
            <ResponsiveContainer width="100%" height="100%">
              <AreaChart data={chartData} margin={{ top: 8, right: 8, bottom: 0, left: -12 }}>
                {grid}
                {xAxis}
                <YAxis domain={[0, 100]} tick={axisTick} axisLine={false} tickLine={false} unit="%" />
                <Tooltip
                  contentStyle={tooltipStyle}
                  labelFormatter={(v) => formatTime(v as number)}
                  formatter={(v) => [`${Number(v).toFixed(1)}%`, 'CPU']}
                  cursor={cursor}
                />
                <Area type="monotone" dataKey="cpu" stroke={INK_PRIMARY} fill={INK_PRIMARY} fillOpacity={0.08} strokeWidth={1.75} dot={false} connectNulls={false} isAnimationActive={false} />
              </AreaChart>
            </ResponsiveContainer>
          ) : emptyState}
        </ChartCard>

        {/* Memory: container memory and JVM heap, the same two series in Live
            and History so switching modes does not change what is drawn. */}
        <ChartCard
          label="Memory"
          currentValue={!liveOffline && rawData.length > 0 ? (
            <>
              {hasHeap ? (
                <>Heap {formatMB(latestHeapMb)}<span className="text-(--base-06)"> / {formatMB(heapMaxMb)}<span className="hidden sm:inline">, process {formatMB(latestMemMb)} / {formatMB(memLimitMb)}</span></span></>
              ) : (
                <>{formatMB(latestMemMb)}<span className="text-(--base-06)"> / {formatMB(memLimitMb)}</span></>
              )}
              <MemoryHelp bookedMb={server.memory} limitMb={memLimitMb} />
            </>
          ) : '—'}
          legend={hasChart ? (
            <>
              <LegendItem color={INK_QUIET} label="Container" />
              {hasHeap && <LegendItem color={INK_PRIMARY} label="JVM heap (post-GC)" />}
              {heapMaxMb > 0 && <LegendItem color="var(--base-05)" dashed label={`Heap max ${formatMB(heapMaxMb)}`} />}
            </>
          ) : undefined}
        >
          {hasChart ? (
            <ResponsiveContainer width="100%" height="100%">
              <AreaChart data={chartData} margin={{ top: 8, right: 8, bottom: 0, left: -12 }}>
                {grid}
                {xAxis}
                <YAxis domain={[0, memLimitMb > 0 ? memLimitMb : 'auto']} tick={axisTick} axisLine={false} tickLine={false} tickFormatter={(v: number) => formatMB(v)} width={60} />
                <Tooltip
                  contentStyle={tooltipStyle}
                  labelFormatter={(v) => formatTime(v as number)}
                  formatter={(v, name) => [formatMB(Number(v)), name === 'javaHeapUsed' ? 'JVM heap' : 'Container']}
                  // Container first, as it sits above the heap on the chart. The
                  // default sorts by dataKey, which put the heap on top.
                  itemSorter={(item) => (item.dataKey === 'memUsed' ? 0 : 1)}
                  cursor={cursor}
                />
                {heapMaxMb > 0 && <ReferenceLine y={heapMaxMb} stroke="var(--base-05)" strokeDasharray="4 4" ifOverflow="extendDomain" />}
                <Area type="monotone" dataKey="memUsed" stroke={INK_QUIET} fill={INK_QUIET} fillOpacity={0.08} strokeWidth={1.75} dot={false} connectNulls={false} isAnimationActive={false} />
                {hasHeap && <Area type="monotone" dataKey="javaHeapUsed" stroke={INK_PRIMARY} fill={INK_PRIMARY} fillOpacity={0.1} strokeWidth={1.75} dot={false} connectNulls={false} isAnimationActive={false} />}
              </AreaChart>
            </ResponsiveContainer>
          ) : emptyState}
        </ChartCard>
      </div>

      <CoreLoadBars serverId={server.id} active={mode === 'live' && !isOffline} cpuLimit={server.cpuLimit} />

      {!diskUsage && diskCheckedFor !== server.id && (
        <Skeleton className="h-28 w-full rounded-xl" aria-hidden="true" />
      )}
      {diskCard}
      {backupCard}
    </div>
  );
}
