"use client";

import { useState, useEffect } from 'react';
import { AreaChart, Area, XAxis, YAxis, Tooltip, ResponsiveContainer, CartesianGrid } from 'recharts';
import {
  Server, ServerStats, DiskUsage, BackupConfig, BackupUsage,
  getStatsHistory, getDiskUsage, getBackupConfig, getBackupUsage,
} from '@/lib/api';
import { subscribeEventSource } from '@/lib/sse';
import { latestValue } from '@/lib/statsSeries';

import { Cpu, MemoryStick, AlertTriangle, HardDrive, Archive } from 'lucide-react';

function formatBytes(bytes: number): string {
  if (bytes >= 1073741824) return `${(bytes / 1073741824).toFixed(1)} GB`;
  if (bytes >= 1048576) return `${(bytes / 1048576).toFixed(0)} MB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${bytes} B`;
}

function formatTime(ts: number): string {
  const d = new Date(ts * 1000);
  return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}:${d.getSeconds().toString().padStart(2, '0')}`;
}

function formatTimeShort(ts: number): string {
  const d = new Date(ts * 1000);
  return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}`;
}

interface ChartCardProps {
  icon: React.ReactNode;
  label: string;
  currentValue: string;
  accentColor: string;
  children: React.ReactNode;
}

function ChartCard({ icon, label, currentValue, accentColor, children }: ChartCardProps) {
  return (
    <div className="rounded-xl bg-(--base-01) border border-(--base-03) overflow-hidden">
      <div className="flex items-center justify-between px-4 pt-3.5 pb-1">
        <div className="flex items-center gap-2">
          <div className="w-7 h-7 rounded-md bg-(--base-03) flex items-center justify-center">
            {icon}
          </div>
          <span className="input-label text-[11px]">{label}</span>
        </div>
        <span
          className="font-display text-xl font-bold tabular-nums"
          style={{ color: accentColor }}
        >
          {currentValue}
        </span>
      </div>
      <div className="h-44 px-1 pb-2">
        {children}
      </div>
    </div>
  );
}

interface OverviewViewProps {
  server: Server;
}

export default function OverviewView({ server }: OverviewViewProps) {
  const [mode, setMode] = useState<'live' | 'history'>('live');
  const [liveData, setLiveData] = useState<ServerStats[]>([]);
  const [historyData, setHistoryData] = useState<ServerStats[]>([]);
  const [historyRange, setHistoryRange] = useState('24h');
  const [diskUsage, setDiskUsage] = useState<DiskUsage | null>(null);
  const [backupConfig, setBackupConfig] = useState<BackupConfig | null>(null);
  const [backupUsage, setBackupUsage] = useState<BackupUsage | null>(null);

  useEffect(() => {
    setLiveData([]);

    return subscribeEventSource(`/servers/${server.id}/stats/stream`, (e) => {
      try {
        const data = JSON.parse(e.data) as ServerStats;
        setLiveData(prev => [...prev.slice(-59), data]);
      } catch { /* ignore */ }
    });
  }, [server.id]);

  useEffect(() => {
    if (mode !== 'history') return;
    getStatsHistory(server.id, historyRange).then((res: { points?: ServerStats[] }) => {
      setHistoryData(res.points || []);
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

  const chartData = mode === 'live' ? liveData : historyData;
  const timeFormatter = mode === 'live' ? formatTime : formatTimeShort;

  const isOffline = server.status === 'stopped' || server.status === 'offline' || server.status === 'pending_setup';

  const latestCpu = chartData.length > 0 ? (chartData[chartData.length - 1].cpu ?? 0) : 0;
  // Prefer JVM heap (post-GC, fluctuates with the GC cycle) over the
  // container-level metric: with Xms=Xmx the container always reads
  // near the limit, hiding the actual live usage. Fall back to memUsed
  // when no GC has happened yet (early startup) or for Java 8 servers.
  const ramKey: keyof ServerStats = chartData.some(d => typeof d.javaHeapUsed === 'number' && d.javaHeapUsed > 0)
    ? 'javaHeapUsed'
    : 'memUsed';
  // The LAST sample that carries a value, not simply the last sample: the heap
  // field is omitted on any tick where no fresh GC reading was available, so
  // reading the final point alone showed a live server as using 0 MB.
  const latestRamMb = latestValue(chartData, ramKey);

  const tooltipStyle = {
    backgroundColor: 'var(--base-02)',
    border: '1px solid var(--base-04)',
    borderRadius: 'var(--radius-md)',
    fontSize: '12px',
    color: 'var(--base-09)',
    boxShadow: 'var(--shadow-md)',
    padding: '8px 12px',
  };

  const emptyState = (
    <div className="h-full flex items-center justify-center text-(--base-06) text-sm font-mono">
      {isOffline ? 'Server offline' : 'Waiting for data...'}
    </div>
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
          currentValue={chartData.length > 0 ? `${latestCpu.toFixed(1)}%` : '—'}
          accentColor="var(--primary-light)"
        >
          {chartData.length > 1 ? (
            <ResponsiveContainer width="100%" height="100%">
              <AreaChart data={chartData} margin={{ top: 8, right: 8, bottom: 0, left: -12 }}>
                <defs>
                  <linearGradient id="cpuFill" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="var(--primary)" stopOpacity={0.2} />
                    <stop offset="100%" stopColor="var(--primary)" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid stroke="var(--base-03)" strokeDasharray="3 3" vertical={false} />
                <XAxis dataKey="ts" tickFormatter={timeFormatter} tick={{ fontSize: 10, fill: 'var(--base-05)' }} axisLine={false} tickLine={false} />
                <YAxis domain={[0, 100]} tick={{ fontSize: 10, fill: 'var(--base-05)' }} axisLine={false} tickLine={false} unit="%" />
                <Tooltip
                  contentStyle={tooltipStyle}
                  labelFormatter={(v) => formatTime(v as number)}
                  formatter={(v) => [`${Number(v).toFixed(1)}%`, 'CPU']}
                  cursor={{ stroke: 'var(--base-05)', strokeWidth: 1, strokeDasharray: '4 4' }}
                />
                <Area type="monotone" dataKey="cpu" stroke="var(--primary)" fill="url(#cpuFill)" strokeWidth={2} dot={false} isAnimationActive={false} />
              </AreaChart>
            </ResponsiveContainer>
          ) : emptyState}
        </ChartCard>

        {/* RAM Chart */}
        <ChartCard
          icon={<MemoryStick size={14} className="text-(--base-06)" />}
          label={ramKey === 'javaHeapUsed' ? 'JVM Heap' : 'RAM'}
          currentValue={chartData.length > 0 ? (latestRamMb >= 1024 ? `${(latestRamMb / 1024).toFixed(1)} GB` : `${Math.round(latestRamMb)} MB`) : '—'}
          accentColor="var(--accent-light)"
        >
          {chartData.length > 1 ? (
            <ResponsiveContainer width="100%" height="100%">
              <AreaChart data={chartData} margin={{ top: 8, right: 8, bottom: 0, left: -12 }}>
                <defs>
                  <linearGradient id="ramFill" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="var(--accent)" stopOpacity={0.2} />
                    <stop offset="100%" stopColor="var(--accent)" stopOpacity={0} />
                  </linearGradient>
                </defs>
                <CartesianGrid stroke="var(--base-03)" strokeDasharray="3 3" vertical={false} />
                <XAxis dataKey="ts" tickFormatter={timeFormatter} tick={{ fontSize: 10, fill: 'var(--base-05)' }} axisLine={false} tickLine={false} />
                <YAxis domain={[0, server.memory]} tick={{ fontSize: 10, fill: 'var(--base-05)' }} axisLine={false} tickLine={false} tickFormatter={(v: number) => v >= 1024 ? `${(v / 1024).toFixed(1)}G` : `${v}M`} />
                <Tooltip
                  contentStyle={tooltipStyle}
                  labelFormatter={(v) => formatTime(v as number)}
                  formatter={(v) => { const n = Number(v); return [`${n >= 1024 ? (n / 1024).toFixed(1) + ' GB' : n + ' MB'}`, 'RAM']; }}
                  cursor={{ stroke: 'var(--base-05)', strokeWidth: 1, strokeDasharray: '4 4' }}
                />
                <Area type="monotone" dataKey={ramKey} stroke="var(--accent)" fill="url(#ramFill)" strokeWidth={2} dot={false} isAnimationActive={false} />
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
