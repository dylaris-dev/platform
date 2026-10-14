"use client";

import { useState, useEffect, useRef } from 'react';
import { AreaChart, Area, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid, ReferenceLine } from 'recharts';
import {
  Server, ServerStats, DiskUsage, BackupConfig, BackupUsage,
  getStatsHistory, getDiskUsage, getBackupConfig, getBackupUsage,
} from '@/lib/api';
import { subscribeEventSource } from '@/lib/sse';
import { latestValue, chartSeries, appendSamples } from '@/lib/statsSeries';
import { jvmHeapMB } from '@/lib/jvmHeap';
import { formatBytes, formatMB } from '@/lib/format';
import { timeAgo } from '@/lib/time';

import { Cpu, MemoryStick, AlertTriangle, HardDrive, Archive, PowerOff } from 'lucide-react';

function formatTime(ts: number): string {
  const d = new Date(ts * 1000);
  return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}:${d.getSeconds().toString().padStart(2, '0')}`;
}

function formatTimeShort(ts: number): string {
  const d = new Date(ts * 1000);
  return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}`;
}

const RANGE_SEC: Record<string, number> = { '1h': 3600, '6h': 6 * 3600, '12h': 12 * 3600, '24h': 24 * 3600 };
// Wider than one sampling interval by a margin: live ticks every 2 s, history
// rows every 2 min. Anything longer is time the server was not reporting.
const LIVE_GAP_SEC = 10;
const HISTORY_GAP_SEC = 300;
// How long live samples collect before they are drawn.
const STATS_BATCH_MS = 250;

// The `state` event Core sends first on the stats stream.
interface StreamState {
  live: boolean;
  lastSampleAt: number | null; // unix ms
}

interface ChartCardProps {
  icon: React.ReactNode;
  label: string;
  currentValue: React.ReactNode;
  accentColor: string;
  legend?: React.ReactNode;
  children: React.ReactNode;
}

function ChartCard({ icon, label, currentValue, accentColor, legend, children }: ChartCardProps) {
  return (
    <div className="rounded-xl bg-(--base-01) border border-(--base-03) overflow-hidden">
      <div className="flex items-center justify-between gap-3 px-4 pt-3.5 pb-1">
        <div className="flex items-center gap-2 min-w-0">
          <div className="w-7 h-7 rounded-md bg-(--base-03) flex items-center justify-center shrink-0">
            {icon}
          </div>
          <span className="input-label text-[11px]">{label}</span>
        </div>
        <span
          className="font-display text-xl font-bold tabular-nums whitespace-nowrap"
          style={{ color: accentColor }}
        >
          {currentValue}
        </span>
      </div>
      {legend && <div className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 pb-1">{legend}</div>}
      <div className="h-44 px-1 pb-2">
        {children}
      </div>
    </div>
  );
}

function LegendItem({ color, label, dashed }: { color: string; label: string; dashed?: boolean }) {
  return (
    <span className="flex items-center gap-1.5 text-[11px] text-(--base-07)">
      <span
        className="inline-block w-3 shrink-0"
        style={dashed ? { borderTop: `1px dashed ${color}` } : { height: 2, borderRadius: 1, backgroundColor: color }}
      />
      {label}
    </span>
  );
}

interface OverviewViewProps {
  server: Server;
}

export default function OverviewView({ server }: OverviewViewProps) {
  const [mode, setMode] = useState<'live' | 'history'>('live');
  const [liveData, setLiveData] = useState<ServerStats[]>([]);
  const [streamState, setStreamState] = useState<StreamState | null>(null);
  const [historyData, setHistoryData] = useState<ServerStats[]>([]);
  const [historyRange, setHistoryRange] = useState('24h');
  const [historyAt, setHistoryAt] = useState(() => Date.now());
  const [diskUsage, setDiskUsage] = useState<DiskUsage | null>(null);
  const [backupConfig, setBackupConfig] = useState<BackupConfig | null>(null);
  const [backupUsage, setBackupUsage] = useState<BackupUsage | null>(null);

  const isOffline = server.status === 'stopped' || server.status === 'offline' || server.status === 'pending_setup';

  // Live samples keep arriving while the history is shown, but only reach
  // state (and re-render the charts) while the live view is the one shown.
  const liveRef = useRef<ServerStats[]>([]);
  const modeRef = useRef(mode);
  modeRef.current = mode;
  const showLive = (next: ServerStats[]) => {
    liveRef.current = next;
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
        batch.push(JSON.parse(e.data) as ServerStats);
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
    }).catch(() => {});

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
  const chartData = chartSeries(rawData, mode === 'live' ? LIVE_GAP_SEC : HISTORY_GAP_SEC);
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

  const tooltipStyle = {
    backgroundColor: 'var(--base-02)',
    border: '1px solid var(--base-04)',
    borderRadius: 'var(--radius-md)',
    fontSize: '12px',
    color: 'var(--base-09)',
    boxShadow: 'var(--shadow-md)',
    padding: '8px 12px',
  };

  const emptyState = liveOffline ? (
    <div className="h-full flex flex-col items-center justify-center gap-1.5 text-center px-4">
      <PowerOff size={18} className="text-(--base-05)" />
      <span className="text-sm text-(--base-08)">Server offline</span>
      <span className="text-xs text-(--base-06)">
        {lastSampleAt
          ? `Last sample ${new Date(lastSampleAt).toLocaleString()} (${timeAgo(new Date(lastSampleAt).toISOString())})`
          : 'No recent samples'}
      </span>
    </div>
  ) : (
    <div className="h-full flex items-center justify-center text-(--base-06) text-sm font-mono">
      {mode === 'history' ? 'No samples in this range' : isOffline ? 'Server offline' : 'Waiting for data...'}
    </div>
  );

  const xAxis = (
    <XAxis
      dataKey="ts"
      type="number"
      scale="time"
      domain={xDomain}
      tickFormatter={timeFormatter}
      tick={{ fontSize: 10, fill: 'var(--base-05)' }}
      axisLine={false}
      tickLine={false}
      minTickGap={24}
    />
  );

  return (
    <div className="h-full flex flex-col gap-4 overflow-y-auto">
      {/* An install that cannot progress. Without this the page shows an
          "installing" badge and a spinner that never resolves, which on BYON -
          where the node is somebody's home PC - is an ordinary support ticket.
          The state is not wrong, it was just never explained. */}
      {server.installStalled && (
        <div className="rounded-md border border-(--warning)/40 bg-(--warning-ghost) px-4 py-3">
          <div className="flex items-start gap-2.5">
            <AlertTriangle size={16} className="shrink-0 mt-0.5 text-(--warning-light)" />
            <div className="min-w-0 text-sm leading-relaxed">
              <div className="font-medium text-(--warning-light)">Installation paused</div>
              <div className="text-(--base-08) mt-0.5">
                {server.installStallReason || 'The node running this install is offline.'}
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Header Controls */}
      <div className="flex items-center gap-3">
        <div className="flex bg-(--base-03) rounded-md p-0.5">
          <button
            onClick={() => setMode('live')}
            className={`btn text-sm px-3 py-1.5 border-0 rounded-sm ${
              mode === 'live' ? 'bg-(--accent) text-white' : 'bg-transparent text-(--base-07) hover:text-(--base-09)'
            }`}
          >
            Live
          </button>
          <button
            onClick={() => setMode('history')}
            className={`btn text-sm px-3 py-1.5 border-0 rounded-sm ${
              mode === 'history' ? 'bg-(--accent) text-white' : 'bg-transparent text-(--base-07) hover:text-(--base-09)'
            }`}
          >
            History
          </button>
        </div>
        {mode === 'history' && (
          <div className="flex gap-1">
            {['1h', '6h', '12h', '24h'].map(r => (
              <button
                key={r}
                onClick={() => setHistoryRange(r)}
                className={`px-2.5 py-1 rounded-sm text-xs font-medium transition-colors ${
                  historyRange === r ? 'bg-(--base-04) text-(--base-09)' : 'text-(--base-07) hover:text-(--base-09)'
                }`}
              >
                {r}
              </button>
            ))}
          </div>
        )}
        <span className="mono-label ml-auto">
          {mode === 'live' ? `${liveData.length} pts` : `${historyData.length} pts`}
        </span>
      </div>

      {/* Charts Grid */}
      <div className="grid grid-cols-1 lg:grid-cols-2 gap-3">
        {/* CPU Chart */}
        <ChartCard
          icon={<Cpu size={14} className="text-(--base-06)" />}
          label="CPU"
          currentValue={!liveOffline && rawData.length > 0 ? `${latestCpu.toFixed(1)}%` : '—'}
          accentColor="var(--primary-light)"
        >
          {hasChart ? (
            <ResponsiveContainer width="100%" height="100%">
              <AreaChart data={chartData} margin={{ top: 8, right: 8, bottom: 0, left: -12 }}>
                <defs>
                  <linearGradient id="cpuFill" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="var(--primary)" stopOpacity={0.2} />
                    <stop offset="100%" stopColor="var(--primary)" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid stroke="var(--base-03)" strokeDasharray="3 3" vertical={false} />
                {xAxis}
                <YAxis domain={[0, 100]} tick={{ fontSize: 10, fill: 'var(--base-05)' }} axisLine={false} tickLine={false} unit="%" />
                <Tooltip
                  contentStyle={tooltipStyle}
                  labelFormatter={(v) => formatTime(v as number)}
                  formatter={(v) => [`${Number(v).toFixed(1)}%`, 'CPU']}
                  cursor={{ stroke: 'var(--base-05)', strokeWidth: 1, strokeDasharray: '4 4' }}
                />
                <Area type="monotone" dataKey="cpu" stroke="var(--primary)" fill="url(#cpuFill)" strokeWidth={2} dot={false} connectNulls={false} isAnimationActive={false} />
              </AreaChart>
            </ResponsiveContainer>
          ) : emptyState}
        </ChartCard>

        {/* Memory Chart: container memory and JVM heap, the same two series in
            Live and History so switching modes does not change what is drawn. */}
        <ChartCard
          icon={<MemoryStick size={14} className="text-(--base-06)" />}
          label="Memory"
          currentValue={!liveOffline && rawData.length > 0 ? (
            <>
              {formatMB(latestMemMb)}
              {hasHeap && <span className="text-sm font-semibold text-(--base-07)"> / heap {formatMB(latestHeapMb)}</span>}
            </>
          ) : '—'}
          accentColor="var(--accent-light)"
          legend={hasChart ? (
            <>
              <LegendItem color="var(--accent)" label="Container" />
              {hasHeap && <LegendItem color="var(--primary)" label="JVM heap (post-GC)" />}
              {heapMaxMb > 0 && <LegendItem color="var(--base-06)" dashed label={`Heap max ${formatMB(heapMaxMb)}`} />}
            </>
          ) : undefined}
        >
          {hasChart ? (
            <ResponsiveContainer width="100%" height="100%">
              <AreaChart data={chartData} margin={{ top: 8, right: 8, bottom: 0, left: -12 }}>
                <defs>
                  <linearGradient id="ramFill" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="var(--accent)" stopOpacity={0.2} />
                    <stop offset="100%" stopColor="var(--accent)" stopOpacity={0} />
                  </linearGradient>
                  <linearGradient id="heapFill" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="var(--primary)" stopOpacity={0.15} />
                    <stop offset="100%" stopColor="var(--primary)" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid stroke="var(--base-03)" strokeDasharray="3 3" vertical={false} />
                {xAxis}
                <YAxis domain={[0, memLimitMb > 0 ? memLimitMb : 'auto']} tick={{ fontSize: 10, fill: 'var(--base-05)' }} axisLine={false} tickLine={false} tickFormatter={(v: number) => formatMB(v)} width={60} />
                <Tooltip
                  contentStyle={tooltipStyle}
                  labelFormatter={(v) => formatTime(v as number)}
                  formatter={(v, name) => [formatMB(Number(v)), name === 'javaHeapUsed' ? 'JVM heap' : 'Container']}
                  cursor={{ stroke: 'var(--base-05)', strokeWidth: 1, strokeDasharray: '4 4' }}
                />
                {heapMaxMb > 0 && <ReferenceLine y={heapMaxMb} stroke="var(--base-06)" strokeDasharray="4 4" ifOverflow="extendDomain" />}
                <Area type="monotone" dataKey="memUsed" stroke="var(--accent)" fill="url(#ramFill)" strokeWidth={2} dot={false} connectNulls={false} isAnimationActive={false} />
                {hasHeap && <Area type="monotone" dataKey="javaHeapUsed" stroke="var(--primary)" fill="url(#heapFill)" strokeWidth={2} dot={false} connectNulls={false} isAnimationActive={false} />}
              </AreaChart>
            </ResponsiveContainer>
          ) : emptyState}
        </ChartCard>
      </div>

      {/* Disk Limit Warning */}
      {diskUsage?.warning && diskUsage.limit > 0 && (
        <div className={`border rounded-xl p-4 flex items-center gap-3 ${
          diskUsage.warning === 'full'
            ? 'bg-(--error-ghost) border-(--error-border)'
            : diskUsage.warning === '90'
              ? 'bg-(--error-ghost) border-(--error-border)'
              : 'bg-(--warning-ghost) border-(--warning-border)'
        }`}>
          <AlertTriangle size={20} className={
            diskUsage.warning === '80' ? 'text-(--warning)' : 'text-(--error-light)'
          } />
          <div>
            <p className={`text-sm font-semibold ${
              diskUsage.warning === '80' ? 'text-(--warning)' : 'text-(--error-light)'
            }`}>
              {diskUsage.warning === 'full'
                ? 'Storage limit reached'
                : diskUsage.warning === '90'
                  ? 'Storage limit critical'
                  : 'Storage running low'}
            </p>
            <p className="text-xs text-(--base-07)">
              {Math.round((diskUsage.total / diskUsage.limit) * 100)}% used — {formatBytes(diskUsage.total)} of {formatBytes(diskUsage.limit)}
            </p>
          </div>
        </div>
      )}

      {/* Storage display.
          Three layouts pick themselves automatically based on the global
          backup mode + per-server share-quota setting:
            - backup.mode != "node-local"            → single Disk Usage bar (legacy).
            - "node-local" + !shareQuotaWithServer   → Disk Usage bar + a separate Backups bar.
            - "node-local" +  shareQuotaWithServer   → one bar with a "Backups" segment folded in.
          The combined-quota path is the one the docs warn about needing
          XFS/ZFS project quotas to actually enforce; rendering them as a
          single bar reflects the operator intent even when the cap is
          still application-level. */}

      {/* Disk Usage Section — iPhone-style stacked bar */}
      {diskUsage && (() => {
        const segmentColors = [
          'var(--color-primary)',
          'var(--color-accent)',
          'var(--color-success)',
          'var(--color-warning)',
          'var(--color-primary-light)',
          'var(--color-accent-light)',
          'var(--color-success-light)',
          'var(--color-error-light)',
        ];
        const backupColor = 'var(--color-error)';

        const sorted = diskUsage.subServers
          ? Object.entries(diskUsage.subServers).sort(([, a], [, b]) => b - a)
          : [];

        // When mode is "node-local" AND the admin folded the backup
        // budget into the server quota, render the backup bytes as
        // an extra segment of the same bar. Otherwise the backup card
        // (rendered below) handles its own bar.
        const folded = backupConfig?.mode === 'node-local'
          && backupConfig.shareQuotaWithServer
          && backupUsage
          && !backupUsage.degraded;
        const foldedBackupBytes = folded ? (backupUsage?.usedBytes ?? 0) : 0;

        const total = diskUsage.total + foldedBackupBytes;
        const capacity = diskUsage.limit > 0 ? diskUsage.limit : total;
        const freeBytes = diskUsage.limit > 0 ? Math.max(diskUsage.limit - total, 0) : 0;

        return (
          <div className="card p-5">
            <div className="flex items-center justify-between mb-4">
              <h3 className="input-label flex items-center gap-1.5">
                <HardDrive size={16} className="text-(--base-07)" />
                {folded ? 'Disk Usage (incl. backups)' : 'Disk Usage'}
              </h3>
              <span className="font-mono text-sm text-(--base-09)">
                {formatBytes(total)}
                {diskUsage.limit > 0 && <span className="text-(--base-06)"> / {formatBytes(diskUsage.limit)}</span>}
              </span>
            </div>

            {/* What differs on a filesystem without project quotas is HOW the
                limit is held, not whether it is. The disk guard still measures
                (du instead of a quota read), still stops the server at the
                limit, and Core still refuses to start it again until usage
                drops - see node/disk_guard.go. What is missing is the
                filesystem's own hard stop, so a server can overshoot between two
                scans, which run every few minutes.

                Saying "not enforced" here would be a plain lie to the owner, and
                the first version of this note said exactly that. The useful fact
                is the overshoot: it is why usage can read above the limit and
                why the server stops a few minutes later rather than instantly.

                Explicitly === false: a node too old to send the field leaves it
                undefined, and that must not read as a warning. */}
            {diskUsage.limit > 0 && diskUsage.enforceable === false && (
              <p className="flex items-start gap-1.5 mb-3 rounded-sm border border-(--warning-border) bg-(--warning-ghost) px-2 py-1.5 text-[11px] text-(--warning-light)">
                <AlertTriangle size={11} className="mt-0.5 shrink-0" />
                <span>
                  This storage cannot hold the limit itself (disk quotas need xfs or ext4), so
                  the server is stopped by a periodic check instead. Usage is measured every few
                  minutes and can briefly go over the limit before that happens.
                </span>
              </p>
            )}

            {/* Stacked bar */}
            <div className="w-full h-4 bg-(--base-04) rounded-sm overflow-hidden flex">
              {sorted.map(([name, bytes], i) => {
                const pct = capacity > 0 ? (bytes / capacity) * 100 : 0;
                if (pct < 0.3) return null;
                return (
                  <div
                    key={name}
                    title={`${name}: ${formatBytes(bytes)}`}
                    style={{
                      width: `${pct}%`,
                      backgroundColor: segmentColors[i % segmentColors.length],
                      minWidth: pct > 0 ? '2px' : 0,
                    }}
                    className="h-full transition-all duration-300"
                  />
                );
              })}
              {folded && foldedBackupBytes > 0 && (() => {
                const pct = capacity > 0 ? (foldedBackupBytes / capacity) * 100 : 0;
                if (pct < 0.3) return null;
                return (
                  <div
                    key="__backups"
                    title={`Backups: ${formatBytes(foldedBackupBytes)}`}
                    style={{ width: `${pct}%`, backgroundColor: backupColor, minWidth: '2px' }}
                    className="h-full transition-all duration-300"
                  />
                );
              })()}
            </div>

            {/* Legend */}
            <div className="flex flex-wrap gap-x-5 gap-y-2 mt-3">
              {sorted.map(([name, bytes], i) => (
                <div key={name} className="flex items-center gap-2 text-sm">
                  <span
                    className="w-2.5 h-2.5 rounded-sm shrink-0"
                    style={{ backgroundColor: segmentColors[i % segmentColors.length] }}
                  />
                  <span className="text-(--base-08)">{name}</span>
                  <span className="font-mono text-xs text-(--base-06)">{formatBytes(bytes)}</span>
                </div>
              ))}
              {folded && foldedBackupBytes > 0 && (
                <div className="flex items-center gap-2 text-sm">
                  <span className="w-2.5 h-2.5 rounded-sm shrink-0" style={{ backgroundColor: backupColor }} />
                  <span className="text-(--base-08)">Backups</span>
                  <span className="font-mono text-xs text-(--base-06)">{formatBytes(foldedBackupBytes)}</span>
                </div>
              )}
              {diskUsage.limit > 0 && freeBytes > 0 && (
                <div className="flex items-center gap-2 text-sm">
                  <span className="w-2.5 h-2.5 rounded-sm shrink-0 bg-(--base-04)" />
                  <span className="text-(--base-06)">Free</span>
                  <span className="font-mono text-xs text-(--base-06)">{formatBytes(freeBytes)}</span>
                </div>
              )}
            </div>
          </div>
        );
      })()}

      {/* Separate "Backups" card — only when mode is "node-local" and the
          quota is NOT shared with server storage. Mirrors the disk-usage
          card's layout so the two read as siblings. The quota number
          comes from the global BackupConfig; the used bytes from the
          per-server backup-usage RPC. */}
      {backupConfig?.mode === 'node-local'
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
          const pct = capacity > 0 ? (used / capacity) * 100 : 0;
          const freeBytes = limit > 0 ? Math.max(limit - used, 0) : 0;

          return (
            <div className="card p-5">
              <div className="flex items-center justify-between mb-4">
                <h3 className="input-label flex items-center gap-1.5">
                  <Archive size={16} className="text-(--base-07)" />
                  Backups
                </h3>
                <span className="font-mono text-sm text-(--base-09)">
                  {formatBytes(used)}
                  {limit > 0 && <span className="text-(--base-06)"> / {formatBytes(limit)}</span>}
                </span>
              </div>

              <div className="w-full h-4 bg-(--base-04) rounded-sm overflow-hidden flex">
                {pct > 0 && (
                  <div
                    title={`Backups: ${formatBytes(used)}`}
                    style={{ width: `${Math.min(pct, 100)}%`, backgroundColor: 'var(--color-error)', minWidth: '2px' }}
                    className="h-full transition-all duration-300"
                  />
                )}
              </div>

              <div className="flex flex-wrap gap-x-5 gap-y-2 mt-3">
                <div className="flex items-center gap-2 text-sm">
                  <span className="w-2.5 h-2.5 rounded-sm shrink-0 bg-(--error)" />
                  <span className="text-(--base-08)">
                    {backupUsage.count} {backupUsage.count === 1 ? 'archive' : 'archives'}
                  </span>
                  <span className="font-mono text-xs text-(--base-06)">{formatBytes(used)}</span>
                </div>
                {limit > 0 && freeBytes > 0 && (
                  <div className="flex items-center gap-2 text-sm">
                    <span className="w-2.5 h-2.5 rounded-sm shrink-0 bg-(--base-04)" />
                    <span className="text-(--base-06)">Free</span>
                    <span className="font-mono text-xs text-(--base-06)">{formatBytes(freeBytes)}</span>
                  </div>
                )}
              </div>
            </div>
          );
        })()}

    </div>
  );
}
