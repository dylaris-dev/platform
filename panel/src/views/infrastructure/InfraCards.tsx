"use client";

// The cards and small presentational pieces of the infrastructure screen.
// Split out of InfrastructureView when the page became one route per tab: the
// shell owns data and navigation, this file owns how a node, an edge or an
// error looks. Nothing here fetches.

import React, { useState, useEffect } from 'react';
import {
  Server, Globe, Network, Trash2, AlertTriangle, Users,
  Cpu, MemoryStick, ArrowDownToLine, ArrowUpFromLine, Shield,
  Link2, HardDrive, Layers, ArrowUpCircle, AlertCircle, Info
} from 'lucide-react';
import { getNodeServers, setNodeStoragePlacement, GatewayEdge, GatewayLink, EdgeStats, API_URL } from '@/lib/api';
import StoragePlacement from '@/components/StoragePlacement';
import { SkeletonCard } from '@/components/Skeleton';
import type { StoragePlacement as StoragePlacementConfig } from '@/lib/api';
import { spliceImageMismatch } from '@/lib/spliceDrift';
import {
  sharedStorageMessage,
  sharedStorageSummary,
  type SharedStorageConflict,
} from '@/lib/sharedStorage';
import { timeAgo } from '@/lib/time';
import { nodeConnectivity, dotFor } from '@/lib/connectivity';
import { nodeLabel } from '@/lib/nodeLabel';
import { isAttention, type FlatServiceError, type ServiceErrorEntry } from '@/lib/serviceErrors';

export interface StorageInfo {
  path: string;
  total_bytes: number;
  free_bytes: number;
  used_bytes: number;
  server_count: number;
  server_uuids?: string[];
}

export interface DiskPathStatus {
  path: string;
  totalGb: number;
  freeGb: number;
  usedGb: number;
  committedGb: number;
  unwrittenGb: number;
  availableGb: number;
  headroomGb: number;
  projectedPercent: number;
  status: 'unknown' | 'ok' | 'warning' | 'critical' | 'breached';
  quotaEnforceable: boolean;
}

async function fetchNodeStorage(nodeId: number): Promise<{ storage: StorageInfo[]; placement: StoragePlacementConfig; capacity: DiskPathStatus[] }> {
  const fallback = {
    storage: [] as StorageInfo[],
    placement: { mode: 'auto' as const, order: [] as string[] },
    capacity: [] as DiskPathStatus[],
  };
  try {
    const res = await fetch(`${API_URL}/nodes/${nodeId}/storage`, {
    });
    const data = await res.json();
    if (!data.success) return fallback;
    return {
      storage: data.storage ?? [],
      placement: data.placement ?? fallback.placement,
      capacity: data.capacity ?? [],
    };
  } catch { return fallback; }
}

export interface NodeInfo {
  id: number;
  name: string;
  // Sent by Core already (models.Node is serialised straight into the overview);
  // the panel simply never declared them, which is why one list held every kind
  // of machine. Classified through lib/nodeKind, never inline.
  ownerId?: string;
  tags?: string;
  // Admin-editable human label, defaulted to the hostname the node reported at
  // enroll. `name`/`token` are the Core-minted identity, not a label.
  displayName?: string;
  token?: string;
  address: string;
  status: string;
  lastSeenAt?: string;
  serverCount?: number;
  privateIps?: string[];
  cpuUsage?: number;
  ramFree?: number;
  ramTotal?: number;
  linkCount?: number;
  portRange?: string;
  portRangeNotice?: string;
  // Non-empty when the node found one of its storage paths mounted into another
  // node too. That topology cannot work - node identity lives in the first
  // storage path - and it destroys a server on the next migration.
  sharedStorage?: SharedStorageConflict[];
}

export interface ServerInfo {
  id: number;
  uuid: string;
  name: string;
  status: string;
}

export interface InfrastructureData {
  edges: GatewayEdge[];
  links: GatewayLink[];
  nodes: NodeInfo[];
  routeCount: number;
  onlineLinks: number;
  onlineEdges: number;
  errors: FlatServiceError[];
  customers?: CustomerSummary;
}

/**
 * What tenants run: BYON nodes, their links, and their warp overlay peers.
 *
 * A total and how many are up. Nothing else, and that is the whole design: these
 * machines belong to customers, sit in places nobody here can reach, and get
 * switched off for ordinary reasons. Attaching a severity to "3 of 4 up" would
 * put a warning on this platform for somebody else's laptop being closed.
 *
 * `online` is optional because absent and zero are different answers. Warp
 * liveness comes from a gauge the leaders only publish once the gateway is
 * updated; a confident 0/12 for a fleet nobody measured reads as a total outage.
 */
export interface CustomerCounts {
  total: number;
  online?: number | null;
}

export interface CustomerSummary {
  nodes: CustomerCounts;
  links: CustomerCounts;
  warps: CustomerCounts;
}

type Tab = 'nodes' | 'edges' | 'routes' | 'bandwidth' | 'errors';

export function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return `${(bytes / Math.pow(k, i)).toFixed(1)} ${sizes[i]}`;
}

/**
 * A node's network speed, which arrives in BYTES per second (`rx_speed`).
 *
 * The parameter is named for what it is: feeding this a bits-per-second figure
 * renders an eighth of the truth with a byte unit on it. The gateway screens
 * work in bits and format with formatBitsPerSec instead.
 */
export function formatSpeed(bytesPerSec: number): string {
  return `${formatBytes(bytesPerSec)}/s`;
}

export function ProgressBar({ value, max, color = 'accent' }: { value: number; max: number; color?: 'accent' | 'primary' | 'success' }) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0;
  const colorClass = color === 'success' ? 'bg-(--success-light)' : color === 'primary' ? 'bg-(--primary)' : 'bg-(--accent)';
  return (
    <div className="h-1.5 rounded-full bg-(--base-03) overflow-hidden">
      <div className={`h-full rounded-full transition-all duration-500 ${colorClass}`} style={{ width: `${pct}%` }} />
    </div>
  );
}

export function StatCard({ label, value, sub, icon }: { label: string; value: string | number; sub?: string; icon: React.ReactNode }) {
  return (
    <div className="card p-4 flex flex-col gap-1.5">
      <span className="mono-label">{label}</span>
      <div className="flex items-baseline gap-1.5">
        <div className="text-(--accent-light) flex items-center" style={{ marginBottom: 1 }}>{icon}</div>
        <span className="font-display text-2xl font-bold text-(--base-09) tabular-nums leading-none">{value}</span>
        {sub && <span className="text-sm font-normal text-(--base-05)">{sub}</span>}
      </div>
    </div>
  );
}

export function NodeCard({
  node,
  gatewayEnabled,
  onNavigateToAdminDisk,
}: {
  node: NodeInfo;
  gatewayEnabled: boolean;
  onNavigateToAdminDisk: (nodeId: number) => void;
}) {
  const isOnline = node.status === 'online';
  const label = nodeLabel(node);
  // The token IS the node identity now (a Core-minted UUID), so it is shown as
  // such under the name rather than standing in for it.
  const identity = node.token && node.token !== label ? node.token : '';
  const serverCount = node.serverCount ?? 0;
  const linkCount = node.linkCount ?? 0;
  const hasStats = (node.ramTotal ?? 0) > 0;

  const [storageData, setStorageData] = useState<StorageInfo[] | null>(null);
  const [placement, setPlacement] = useState<StoragePlacementConfig | null>(null);
  const [capacity, setCapacity] = useState<DiskPathStatus[]>([]);

  useEffect(() => {
    fetchNodeStorage(node.id).then(({ storage, placement, capacity }) => {
      setStorageData(storage);
      setPlacement(placement);
      setCapacity(capacity);
    });
  }, [node.id]);

  return (
    <div className="card p-4 flex flex-col gap-3">
      {/* Header */}
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-2.5 min-w-0">
          {(() => {
            const { tier } = nodeConnectivity(node.status, node.lastSeenAt, Date.now());
            const dot = dotFor(tier, 'bg-(--success-light) shadow-[0_0_6px_var(--success-light)]');
            return <div className={`w-2 h-2 rounded-full shrink-0 ${dot}`} />;
          })()}
          <div className="min-w-0">
            <p className="text-sm font-semibold text-(--base-09) truncate">{label}</p>
            {identity && (
              <p className="mono-label text-(--base-05) truncate" title={identity}>{identity}</p>
            )}
          </div>
        </div>
        <div className="flex items-center gap-2 shrink-0">
          <span className={`mono-label ${isOnline ? 'text-(--success-light)' : 'text-(--error)'}`}>
            {node.status}
          </span>
        </div>
      </div>

      {/* Info row */}
      {(() => {
        const diskCount = storageData
          ? storageData.reduce((sum, s) => sum + (s.server_uuids?.length ?? s.server_count), 0)
          : null;
        const orphaned = diskCount !== null ? Math.max(0, diskCount - serverCount) : 0;
        return (
          <div className="flex items-center justify-between gap-2 pt-2 border-t border-(--base-03)">
            {/* Server count + orphaned button */}
            <div className="flex items-center gap-2 min-w-0">
              <Server size={11} className="text-(--base-05) shrink-0" />
              <span className="text-xs font-mono text-(--base-07)">
                <span className="text-(--base-09) font-semibold">{serverCount}</span>
                <span className="text-(--base-06)"> server{serverCount !== 1 ? 's' : ''}</span>
              </span>
              {orphaned > 0 && (
                <button
                  onClick={() => onNavigateToAdminDisk(node.id)}
                  className="px-1.5 py-0.5 rounded border border-(--warning-border) bg-(--warning-ghost) text-(--warning) text-[10px] font-mono hover:bg-(--warning-border)/30 transition-colors cursor-pointer"
                  title="Open in Disk Analysis"
                >
                  {orphaned} orphaned
                </button>
              )}
            </div>

            {/* Links — only when Gateway module is enabled */}
            {gatewayEnabled && (
              <div className="flex items-center gap-1.5 shrink-0">
                <Link2 size={11} className="text-(--base-05)" />
                <span className="text-xs font-mono text-(--base-07)">
                  <span className="text-(--base-09) font-semibold">{linkCount}</span>
                  <span className="text-(--base-06)"> link{linkCount !== 1 ? 's' : ''}</span>
                </span>
              </div>
            )}
          </div>
        );
      })()}

      {/* CPU */}
      {hasStats && node.cpuUsage !== undefined && (
        <div>
          <div className="flex items-center justify-between mb-1">
            <span className="flex items-center gap-1.5 mono-label">
              <Cpu size={10} /> CPU
            </span>
            <span className="text-[10px] font-mono text-(--base-07) tabular-nums">{node.cpuUsage.toFixed(1)}%</span>
          </div>
          <ProgressBar value={node.cpuUsage} max={100} color="accent" />
        </div>
      )}

      {/* RAM */}
      {hasStats && node.ramFree !== undefined && node.ramTotal !== undefined && (
        <div>
          <div className="flex items-center justify-between mb-1">
            <span className="flex items-center gap-1.5 mono-label">
              <MemoryStick size={10} /> RAM
            </span>
            <span className="text-[10px] font-mono text-(--base-07) tabular-nums">
              {formatBytes(node.ramFree)} free / {formatBytes(node.ramTotal)}
            </span>
          </div>
          <ProgressBar value={node.ramTotal - node.ramFree} max={node.ramTotal} color="primary" />
        </div>
      )}

      {/* Shared storage — placed above everything else on the card because it
          is not a warning about degraded behaviour: the node cannot work in this
          topology at all, and the next migration destroys a server while
          reporting success. */}
      {node.sharedStorage && node.sharedStorage.length > 0 && (
        <div className="rounded-sm border border-(--error-border) bg-(--error-ghost) px-2 py-2 space-y-1.5">
          <p className="flex items-start gap-1.5 text-[10px] font-mono font-semibold uppercase tracking-[0.08em] text-(--error-light)">
            <AlertTriangle size={11} className="mt-px shrink-0" />
            <span>{sharedStorageSummary(node.sharedStorage)}</span>
          </p>
          <ul className="space-y-1">
            {node.sharedStorage.map((c: SharedStorageConflict) => (
              <li key={`${c.path}-${c.peerNode ?? ''}-${c.kind}`} className="text-[10px] text-(--error-light) leading-snug">
                {sharedStorageMessage(c)}
              </li>
            ))}
          </ul>
          <p className="text-[10px] text-(--base-06) leading-snug">
            Give each node its own storage path. Sharing one is not supported: node
            identity is stored there, so the nodes overwrite each other.
          </p>
        </div>
      )}

      {/* MC host-port range — these are the ports the host firewall must open,
          so it is shown unconditionally rather than hidden behind a detail view. */}
      {node.portRange && (
        <div>
          <div className="flex items-center justify-between">
            <span className="flex items-center gap-1.5 mono-label">
              <Network size={10} /> Port range
            </span>
            <span className="text-[10px] font-mono text-(--base-07) tabular-nums">{node.portRange}</span>
          </div>
          {node.portRangeNotice && (
            <p className="mt-1.5 flex items-start gap-1.5 rounded-sm border border-(--warning-border) bg-(--warning-ghost) px-2 py-1.5 text-[10px] font-mono text-(--warning-light)">
              <AlertTriangle size={10} className="mt-0.5 shrink-0" />
              <span>{node.portRangeNotice}</span>
            </p>
          )}
        </div>
      )}

      {/* Last seen */}
      {!isOnline && node.lastSeenAt && (
        <p className="text-[10px] text-(--base-05) font-mono">Last seen {timeAgo(node.lastSeenAt)}</p>
      )}

      {/* Storage */}
      <div className="border-t border-(--base-03) pt-2 space-y-1.5">
        <div className="flex items-center gap-1.5 mono-label">
          <HardDrive size={10} />
          Storage
        </div>
        {storageData === null ? (
          <SkeletonCard height="h-10" />
        ) : storageData.length === 0 ? (
          <p className="text-[10px] font-mono text-(--base-05) italic">No storage info available</p>
        ) : (
          storageData.map((s, i) => {
            const pct = s.total_bytes > 0 ? (s.used_bytes / s.total_bytes) * 100 : 0;
            const barColor = pct > 90 ? 'bg-(--error-light)' : pct > 70 ? 'bg-(--warning)' : 'bg-(--accent)';
            const cap = capacity.find((c) => c.path === s.path);
            return (
              <div key={i} className="bg-(--base-02) rounded p-2 border border-(--base-03)">
                <div className="flex justify-between items-center mb-1">
                  <span className="font-mono text-[10px] text-(--base-07) truncate">{s.path}</span>
                  <span className="text-[10px] text-(--base-06) ml-2 shrink-0">{s.server_count} srv</span>
                </div>
                <div className="h-1 rounded-full bg-(--base-03) overflow-hidden">
                  <div className={`h-full rounded-full ${barColor}`} style={{ width: `${Math.min(pct, 100)}%` }} />
                </div>
                <div className="flex justify-between mt-1">
                  <span className="text-[10px] font-mono text-(--base-06)">{formatBytes(s.used_bytes)} / {formatBytes(s.total_bytes)}</span>
                  <span className="text-[10px] font-mono text-(--base-06)">{formatBytes(s.free_bytes)} free</span>
                </div>
                {cap && <StorageCapacityNote cap={cap} />}
              </div>
            );
          })
        )}

        {/* Placement policy — only meaningful with more than one path to choose from. */}
        {placement && storageData && storageData.length > 1 && (
          <StoragePlacement
            paths={storageData.map((s) => s.path)}
            placement={placement}
            save={(next) => setNodeStoragePlacement(node.id, next)}
            onSaved={setPlacement}
          />
        )}
      </div>
    </div>
  );
}

/**
 * StorageCapacityNote shows what free space alone cannot: how much of a path is
 * already PROMISED to the servers on it. A path can read as 10% full and still
 * have nothing left to give, because every server is entitled to grow into its
 * disk limit.
 */
export function StorageCapacityNote({ cap }: { cap: DiskPathStatus }) {
  if (cap.status === 'unknown') return null;

  const tone: Record<string, string> = {
    ok: 'text-(--base-06)',
    warning: 'text-(--warning-light)',
    critical: 'text-(--warning-light)',
    breached: 'text-(--error-light)',
  };
  const label: Record<string, string> = {
    ok: 'OK',
    warning: 'Filling up',
    critical: 'Nearly full',
    breached: 'Over-promised',
  };

  return (
    <div className="mt-1.5 space-y-1 border-t border-(--base-03) pt-1.5">
      <div className="flex items-center justify-between">
        <span className="mono-label">Promised</span>
        <span className={`font-mono text-[10px] tabular-nums ${tone[cap.status]}`}>
          {label[cap.status]} &middot; {cap.projectedPercent}%
        </span>
      </div>
      <p className="font-mono text-[10px] leading-relaxed text-(--base-05)">
        {cap.committedGb} GB committed, {cap.unwrittenGb} GB of it not yet written.
        {' '}
        {cap.availableGb >= 0
          ? `${cap.availableGb} GB left to promise after the ${cap.headroomGb} GB buffer.`
          : `${Math.abs(cap.availableGb)} GB past the ${cap.headroomGb} GB buffer.`}
      </p>
      {!cap.quotaEnforceable && (
        <p className="flex items-start gap-1.5 rounded-sm border border-(--warning-border) bg-(--warning-ghost) px-2 py-1.5 text-[10px] font-mono text-(--warning-light)">
          <AlertTriangle size={10} className="mt-0.5 shrink-0" />
          {/* The remedy belongs here. The warning alone was accurate and useless:
              the node already proved at startup which step is missing, and that
              detail only existed in its log. */}
          <span>
            Disk limits cannot be enforced here, so they are recorded but nothing stops a server from
            exceeding them. Usage is still measured (via <code className="font-mono">du</code>).
            {' '}Project quotas need xfs mounted with <code className="font-mono">pquota</code>, or ext4 with
            the <code className="font-mono">project</code> feature (<code className="font-mono">tune2fs -O project,quota</code>,
            filesystem unmounted) and mounted with <code className="font-mono">prjquota</code>. The node
            logs which of the two is missing on startup (search its log for <code className="font-mono">quota:</code>);
            it re-checks on restart.
          </span>
        </p>
      )}
    </div>
  );
}

export function EdgeCard({ edge }: { edge: GatewayEdge }) {
  const isOnline = edge.status === 'online';
  const stats = edge.stats;
  const rxBytes = (stats as any)?.rx_bytes as number | undefined;
  const txBytes = (stats as any)?.tx_bytes as number | undefined;
  const hasTotal = (rxBytes ?? 0) > 0 || (txBytes ?? 0) > 0;
  const running = edge.splice_version || '';
  const latest = edge.splice_version_latest || '';
  // A pinned splice is "behind" whenever the latest available version is known and
  // this edge's running version differs (an empty running version = pre-versioning
  // splice, also treated as behind so the operator schedules a bump).
  const spliceBehind = !!latest && running !== latest;
  // A DIFFERENT fault from being behind, and it needs both halves known: the
  // running splice is not the image the pin names, at the same version string.
  // That is a tag that moved, and the version comparison above cannot see it.
  const runImg = edge.splice_image_running || '';
  const availImg = edge.splice_image_available || '';
  const imageMismatch = spliceImageMismatch(runImg, availImg);

  return (
    <div className="card p-4 flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-2.5 min-w-0">
          <div className={`w-2 h-2 rounded-full shrink-0 ${isOnline ? 'bg-(--success-light) shadow-[0_0_6px_var(--success-light)]' : 'bg-(--error)'}`} />
          <div className="min-w-0">
            <p className="text-sm font-semibold text-(--base-09) truncate">{edge.name}</p>
            <p className="text-[10px] font-mono text-(--base-05) truncate">{edge.ip}:{edge.service_port}</p>
          </div>
        </div>
        <span className={`mono-label shrink-0 ${isOnline ? 'text-(--success-light)' : 'text-(--error)'}`}>
          {edge.status}
        </span>
      </div>

      {(running || latest) && (
        <div className="flex items-center justify-between gap-2">
          <span className="flex items-center gap-1.5 mono-label text-(--base-06)">
            <Layers size={10} /> Splice
          </span>
          {spliceBehind ? (
            <span className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded bg-(--warning)/10 mono-label text-(--warning-light)">
              <ArrowUpCircle size={9} /> {running ? `v${running}` : 'unknown'} &rarr; v{latest}
            </span>
          ) : imageMismatch ? (
            <span
              className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded bg-(--danger)/10 mono-label text-(--danger-light)"
              title={`Running image ${runImg.slice(7, 19)}, SPLICE_IMAGE resolves to ${availImg.slice(7, 19)}. The pinned tag moved; redeploy the edge stack to recreate the sidecar.`}
            >
              <ArrowUpCircle size={9} /> v{running || latest} &middot; wrong image
            </span>
          ) : (
            <span className="text-[10px] font-mono text-(--base-07) tabular-nums">v{running || latest}</span>
          )}
        </div>
      )}

      {isOnline && stats ? (
        <div className="flex flex-col gap-2.5 pt-1 border-t border-(--base-03)">
          <div>
            <div className="flex items-center justify-between mb-1">
              <span className="flex items-center gap-1.5 mono-label">
                <Cpu size={10} /> CPU
              </span>
              <span className="text-[10px] font-mono text-(--base-07) tabular-nums">{stats.cpu.toFixed(1)}%</span>
            </div>
            <ProgressBar value={stats.cpu} max={100} color="accent" />
          </div>
          <div>
            <div className="flex items-center justify-between mb-1">
              <span className="flex items-center gap-1.5 mono-label">
                <MemoryStick size={10} /> RAM
              </span>
              <span className="text-[10px] font-mono text-(--base-07) tabular-nums">
                {formatBytes(stats.ram_used)} / {formatBytes(stats.ram_total)}
              </span>
            </div>
            <ProgressBar value={stats.ram_pct} max={100} color="primary" />
          </div>
          <div className="flex items-center gap-5 pt-0.5">
            <div className="flex items-center gap-1.5">
              <Users size={11} className="text-(--accent-light)" />
              <span className="text-[11px] font-mono text-(--base-07)">
                <span className="text-(--base-09) font-semibold tabular-nums">{stats.active_mc_streams}</span> players connected
              </span>
            </div>
          </div>
          <div className="flex items-center gap-4 flex-wrap">
            <div className="flex items-center gap-1.5">
              <ArrowDownToLine size={10} className="text-(--success-light)" />
              <span className="text-[10px] font-mono text-(--base-07) tabular-nums">{formatSpeed(stats.rx_speed)}</span>
            </div>
            <div className="flex items-center gap-1.5">
              <ArrowUpFromLine size={10} className="text-(--accent-light)" />
              <span className="text-[10px] font-mono text-(--base-07) tabular-nums">{formatSpeed(stats.tx_speed)}</span>
            </div>
            {hasTotal && (
              <div className="flex items-center gap-1 ml-auto">
                <HardDrive size={10} className="text-(--base-05)" />
                <span className="text-[10px] font-mono text-(--base-06) tabular-nums">
                  {formatBytes((rxBytes ?? 0) + (txBytes ?? 0))} total
                </span>
              </div>
            )}
          </div>
          {stats.xdp_enabled && (
            <div className="flex items-center gap-2 flex-wrap">
              <span className="inline-flex items-center gap-1 px-1.5 py-0.5 rounded bg-(--accent)/10 mono-label text-(--accent-light)">
                <Shield size={9} /> XDP
              </span>
              <span className="text-[10px] font-mono text-(--base-06) tabular-nums">
                {stats.xdp_dropped_blocked + stats.xdp_dropped_ratelimit} dropped
              </span>
              {stats.xdp_blocked_ips > 0 && (
                <span className="text-[10px] font-mono text-(--error-light) tabular-nums">
                  {stats.xdp_blocked_ips} blocked IPs
                </span>
              )}
            </div>
          )}
        </div>
      ) : (
        <div className="pt-1 border-t border-(--base-03)">
          <p className="text-[10px] font-mono text-(--base-05) italic">No stats available</p>
        </div>
      )}
    </div>
  );
}

// SpliceVersionSummary groups online edges by region and reports the running
// splice version(s) vs the latest available one, flagging a pending bump. The
// splice sidecar is version-pinned per regional stack (SPLICE_IMAGE), so a region
// can lag the latest edge image until the owner deliberately moves the pin.
export function SpliceVersionSummary({ edges }: { edges: GatewayEdge[] }) {
  const online = edges.filter(e => e.status === 'online');

  const byRegion = new Map<string, { region: string; running: Set<string>; latest: string; behind: number }>();
  for (const e of online) {
    const region = e.region || 'default';
    let info = byRegion.get(region);
    if (!info) { info = { region, running: new Set(), latest: '', behind: 0 }; byRegion.set(region, info); }
    if (e.splice_version_latest && !info.latest) info.latest = e.splice_version_latest;
    if (e.splice_version) info.running.add(e.splice_version);
    if (e.splice_version_latest && e.splice_version !== e.splice_version_latest) info.behind++;
  }

  const rows = [...byRegion.values()]
    .filter(r => r.latest || r.running.size > 0)
    .sort((a, b) => a.region.localeCompare(b.region));
  if (rows.length === 0) return null;

  return (
    <div className="card p-4 flex flex-col gap-3 mb-3">
      <div className="flex items-center gap-2">
        <Layers size={14} className="text-(--accent-light)" />
        <h3 className="text-sm font-semibold text-(--base-09)">Splice versions</h3>
      </div>
      <div className="flex flex-col divide-y divide-(--base-03)">
        {rows.map(r => {
          const runningList = [...r.running].sort();
          const pending = r.behind > 0;
          return (
            <div key={r.region} className="flex items-center justify-between gap-3 py-2 first:pt-0 last:pb-0">
              <div className="flex items-center gap-2 min-w-0">
                <span className="mono-label text-(--base-06) shrink-0">{r.region}</span>
                <span className="text-[11px] font-mono text-(--base-07) truncate">
                  running {runningList.length ? runningList.map(v => `v${v}`).join(', ') : 'unknown'}
                  {r.latest && <span className="text-(--base-05)"> &middot; latest v{r.latest}</span>}
                </span>
              </div>
              {pending ? (
                <span className="inline-flex items-center gap-1 shrink-0 px-1.5 py-0.5 rounded bg-(--warning)/10 mono-label text-(--warning-light)">
                  <ArrowUpCircle size={9} /> update available
                </span>
              ) : (
                <span className="mono-label shrink-0 text-(--success-light)">up to date</span>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

// The service error streams, one list, newest first.
//
// Deliberately not grouped per service: the question is "what is broken", and
// which component noticed is a property of the line, not a heading to scroll
// past. The producing service is shown because it is often the ONLY hint about
// where to look - a link reporting `failed to connect to 127.0.0.1:25550` is a
// problem on the customer's machine, and the edge above it is perfectly healthy.
export function ServiceErrorList({ entries }: { entries: FlatServiceError[] }) {
  if (entries.length === 0) {
    return (
      <div className="card p-8 text-center text-(--base-06) text-sm">
        No service errors reported
      </div>
    );
  }
  return (
    <div className="card divide-y divide-(--base-03)">
      {entries.map((e, i) => (
        <div key={`${e.ts}-${e.service}-${i}`} className="flex items-start gap-3 p-3">
          <LevelIcon level={e.level} />
          <div className="min-w-0 flex-1">
            <div className="flex flex-wrap items-center gap-2">
              <span className="mono-label text-(--accent-light)">{e.service}</span>
              {e.source && <span className="mono-label text-(--base-06)">{e.source}</span>}
            </div>
            {/* break-words, not truncate: the whole value of these lines is the
                address or name at the end of the message. */}
            <p className={`text-sm mt-0.5 break-words ${isAttention(e) ? 'text-(--base-09)' : 'text-(--base-07)'}`}>
              {e.message}
            </p>
          </div>
          <span className="mono-label shrink-0 text-(--base-06)" title={e.ts}>
            {Number.isNaN(Date.parse(e.ts)) ? e.ts || 'no timestamp' : timeAgo(e.ts)}
          </span>
        </div>
      ))}
    </div>
  );
}

export function LevelIcon({ level }: { level: ServiceErrorEntry['level'] }) {
  const l = level?.toUpperCase();
  if (l === 'ERROR') return <AlertCircle size={15} className="shrink-0 mt-0.5 text-(--error)" aria-label="Error" />;
  if (l === 'WARN') return <AlertTriangle size={15} className="shrink-0 mt-0.5 text-(--warning-light)" aria-label="Warning" />;
  return <Info size={15} className="shrink-0 mt-0.5 text-(--base-06)" aria-label="Info" />;
}
