"use client";

// The cards and small presentational pieces of the infrastructure screen.
// Split out of InfrastructureView when the page became one route per tab: the
// shell owns data and navigation, this file owns how a node, an edge or an
// error looks. Nothing here fetches.

import React, { useState, useEffect } from 'react';
import {
  Server, Globe, Network, Trash2, AlertTriangle, Users,
  Cpu, MemoryStick, ArrowDownToLine, ArrowUpFromLine,
  Link2, HardDrive, Layers, ArrowUpCircle, AlertCircle, Info,
  ChevronDown,
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
import { NO_MEMORY_LIMIT_MESSAGE } from '@/lib/memoryLimit';
import { nodeConnectivity, dotFor } from '@/lib/connectivity';
import { nodeLabel } from '@/lib/nodeLabel';
import { isAttention, type FlatServiceError, type ServiceErrorEntry } from '@/lib/serviceErrors';
import { linkMissing, LINK_MISSING_MESSAGE } from '@/lib/linkPresence';
import { formatBytes } from '@/lib/format';
import { Meter, MeterIndicator, MeterTrack } from '@ui-sdk/react/meter';
import Callout from '@/components/server/Callout';
import Tooltip from '@/components/ui/Tooltip';
import EmptyState from '@/components/ui/EmptyState';
import { RowList, Row } from '@/components/ui/RowList';

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
  /** -1 when the node did not report inodes (older node, or a filesystem without them). */
  inodesFreePercent: number;
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
  // Whether the Link carrying this node's players holds its tunnel. Absent when
  // Core could not ask or cannot trust the answer for this node.
  linkOnline?: boolean;
  portRange?: string;
  portRangeNotice?: string;
  // Non-empty when the node found one of its storage paths mounted into another
  // node too. That topology cannot work - node identity lives in the first
  // storage path - and it destroys a server on the next migration.
  sharedStorage?: SharedStorageConflict[];
  // Servers with a copy on several storage paths that the node cannot tell
  // apart. Each resolves to no directory until the stale copy is removed.
  storageAmbiguous?: { server: string; paths: string[] }[];
  // The host's Docker cannot enforce a server's RAM limit (no memory cgroup).
  noMemoryLimit?: boolean;
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

export function ProgressBar({ value, max, label, tone }: { value: number; max: number; label: string; tone?: 'warning' | 'error' }) {
  const safeMax = max > 0 ? max : 1;
  return (
    <Meter value={Math.min(Math.max(value, 0), safeMax)} min={0} max={safeMax} aria-label={label}>
      <MeterTrack className="srv-meter-track">
        <MeterIndicator className="srv-meter-fill" data-tone={tone} />
      </MeterTrack>
    </Meter>
  );
}

function toneFor(pct: number): 'warning' | 'error' | undefined {
  return pct > 90 ? 'error' : pct > 70 ? 'warning' : undefined;
}

function Stat({ icon, label, value }: { icon?: React.ReactNode; label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3 text-[13px]">
      <span className="srv-label flex items-center gap-1.5">{icon}{label}</span>
      <span className="srv-num text-(--base-08)">{value}</span>
    </div>
  );
}

function MeterRow({ icon, label, detail, value, max }: { icon: React.ReactNode; label: string; detail: React.ReactNode; value: number; max: number }) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between gap-3 text-[13px]">
        <span className="srv-label flex items-center gap-1.5">{icon}{label}</span>
        <span className="srv-num text-(--base-08)">{detail}</span>
      </div>
      <ProgressBar value={value} max={max} label={label} tone={max > 0 ? toneFor((value / max) * 100) : undefined} />
    </div>
  );
}

const LINK_OFFLINE_MESSAGE =
  'The Link for this node is not connected, so players cannot reach its servers through the gateway. ' +
  'Check the Link service on this host.';

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
  // Only meaningful while the node itself is up: an offline machine explains
  // its own servers being unreachable.
  const linkOnline = isOnline ? node.linkOnline : undefined;
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
    <div className="srv-surface p-5 flex flex-col gap-4">
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-2.5 min-w-0">
          {(() => {
            const { tier } = nodeConnectivity(node.status, node.lastSeenAt, Date.now());
            const dot = dotFor(tier, 'bg-(--success-light)');
            return <span className={`size-2 rounded-full shrink-0 ${dot}`} aria-hidden="true" />;
          })()}
          <div className="min-w-0">
            <p className="text-sm font-semibold text-(--base-09) truncate">{label}</p>
            {identity && (
              <Tooltip content={identity}>
                <p className="srv-num text-[11px] text-(--base-06) truncate">{identity}</p>
              </Tooltip>
            )}
          </div>
        </div>
        <span className={`text-xs font-medium capitalize shrink-0 ${isOnline ? 'text-(--success-light)' : 'text-(--error-light)'}`}>
          {node.status}
        </span>
      </div>

      {(() => {
        const diskCount = storageData
          ? storageData.reduce((sum, s) => sum + (s.server_uuids?.length ?? s.server_count), 0)
          : null;
        const orphaned = diskCount !== null ? Math.max(0, diskCount - serverCount) : 0;
        return (
          <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 text-[13px]">
            <div className="flex items-center gap-2 min-w-0">
              <Server size={14} className="text-(--base-06) shrink-0" aria-hidden="true" />
              <span className="text-(--base-07)">
                <span className="srv-num text-(--base-09)">{serverCount}</span> server{serverCount !== 1 ? 's' : ''}
              </span>
              {orphaned > 0 && (
                <Tooltip content="Open in Disk Analysis">
                  <button
                    type="button"
                    onClick={() => onNavigateToAdminDisk(node.id)}
                    className="srv-chip bg-(--warning-ghost)! text-(--warning-light)! cursor-pointer"
                  >
                    {orphaned} orphaned
                  </button>
                </Tooltip>
              )}
            </div>

            {/* Links: only when the Gateway module is enabled */}
            {gatewayEnabled && (
              <div className="flex items-center gap-1.5 shrink-0">
                <Link2 size={14} className="text-(--base-06)" aria-hidden="true" />
                {/* The Link's own answer where Core has one; the node's count of
                    Link containers stays at 1 while that Link reaches no edge. */}
                {linkOnline !== undefined ? (
                  <span className={linkOnline ? 'text-(--success-light)' : 'text-(--error-light)'}>
                    {linkOnline ? 'Link online' : 'Link offline'}
                  </span>
                ) : (
                  <span className="text-(--base-07)">
                    <span className="srv-num text-(--base-09)">{linkCount}</span> link{linkCount !== 1 ? 's' : ''}
                  </span>
                )}
              </div>
            )}
          </div>
        );
      })()}

      {/* Directly under the link count it explains: a node with no Link looks
          healthy everywhere else, while its servers cannot be reached. */}
      {gatewayEnabled && linkOnline === false ? (
        <Callout tone="error" icon={<AlertTriangle size={16} />}>{LINK_OFFLINE_MESSAGE}</Callout>
      ) : linkMissing(gatewayEnabled, node) && (
        <Callout tone="warning" icon={<AlertTriangle size={16} />}>{LINK_MISSING_MESSAGE}</Callout>
      )}

      {hasStats && node.cpuUsage !== undefined && (
        <MeterRow icon={<Cpu size={14} />} label="CPU" detail={`${node.cpuUsage.toFixed(1)}%`} value={node.cpuUsage} max={100} />
      )}

      {hasStats && node.ramFree !== undefined && node.ramTotal !== undefined && (
        <MeterRow
          icon={<MemoryStick size={14} />}
          label="RAM"
          detail={`${formatBytes(node.ramFree)} free / ${formatBytes(node.ramTotal)}`}
          value={node.ramTotal - node.ramFree}
          max={node.ramTotal}
        />
      )}

      {/* Shared storage: placed above everything else on the card because it
          is not a warning about degraded behaviour: the node cannot work in this
          topology at all, and the next migration destroys a server while
          reporting success. */}
      {node.sharedStorage && node.sharedStorage.length > 0 && (
        <Callout tone="error" icon={<AlertTriangle size={16} />} title={sharedStorageSummary(node.sharedStorage)}>
          <ul className="space-y-1">
            {node.sharedStorage.map((c: SharedStorageConflict) => (
              <li key={`${c.path}-${c.peerNode ?? ''}-${c.kind}`}>{sharedStorageMessage(c)}</li>
            ))}
          </ul>
          <p className="mt-1.5 text-(--base-07)">
            Give each node its own storage path. Sharing one is not supported: node
            identity is stored there, so the nodes overwrite each other.
          </p>
        </Callout>
      )}

      {node.storageAmbiguous && node.storageAmbiguous.length > 0 && (
        <Callout
          tone="error"
          icon={<AlertTriangle size={16} />}
          title={node.storageAmbiguous.length === 1
            ? '1 server cannot be resolved'
            : `${node.storageAmbiguous.length} servers cannot be resolved`}
        >
          <ul className="space-y-1 break-all">
            {node.storageAmbiguous.map((a) => (
              <li key={a.server}>{a.server}: copies on {a.paths.join(', ')}</li>
            ))}
          </ul>
          <p className="mt-1.5 text-(--base-07)">
            The node cannot tell which copy is live, so the server does not start and
            its files are not shown. Remove the stale copy on the host to settle it.
          </p>
        </Callout>
      )}

      {node.noMemoryLimit && (
        <Callout tone="warning" icon={<AlertTriangle size={16} />}>{NO_MEMORY_LIMIT_MESSAGE}</Callout>
      )}

      {/* MC host-port range: these are the ports the host firewall must open,
          so it is shown unconditionally rather than hidden behind a detail view. */}
      {node.portRange && (
        <div className="space-y-2">
          <Stat icon={<Network size={14} />} label="Port range" value={node.portRange} />
          {node.portRangeNotice && (
            <Callout tone="warning" icon={<AlertTriangle size={16} />}>{node.portRangeNotice}</Callout>
          )}
        </div>
      )}

      {!isOnline && node.lastSeenAt && (
        <p className="text-xs text-(--base-06)">Last seen {timeAgo(node.lastSeenAt)}</p>
      )}

      <div className="space-y-2 border-t border-(--base-03) pt-3">
        <div className="srv-label flex items-center gap-1.5">
          <HardDrive size={14} aria-hidden="true" />
          Storage
        </div>
        {storageData === null ? (
          <SkeletonCard height="h-10" />
        ) : storageData.length === 0 ? (
          <p className="text-[13px] text-(--base-06)">No storage info available</p>
        ) : (
          storageData.map((s, i) => {
            const pct = s.total_bytes > 0 ? (s.used_bytes / s.total_bytes) * 100 : 0;
            const cap = capacity.find((c) => c.path === s.path);
            return (
              <div key={i} className="rounded-lg bg-(--base-03) p-3 space-y-2">
                <div className="flex justify-between items-center gap-2">
                  <span className="srv-num text-xs text-(--base-08) truncate">{s.path}</span>
                  <span className="text-xs text-(--base-06) shrink-0">{s.server_count} srv</span>
                </div>
                <ProgressBar value={s.used_bytes} max={s.total_bytes} label={`Storage ${s.path}`} tone={toneFor(pct)} />
                <div className="flex justify-between srv-num text-xs text-(--base-06)">
                  <span>{formatBytes(s.used_bytes)} / {formatBytes(s.total_bytes)}</span>
                  <span>{formatBytes(s.free_bytes)} free</span>
                </div>
                {cap && <StorageCapacityNote cap={cap} />}
              </div>
            );
          })
        )}

        {/* Placement policy: only meaningful with more than one path to choose from. */}
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
    <div className="space-y-2 border-t border-(--base-04) pt-2">
      <div className="flex items-center justify-between text-xs">
        <span className="srv-label">Promised</span>
        <span className={`srv-num ${tone[cap.status]}`}>
          {label[cap.status]} &middot; {cap.projectedPercent}%
        </span>
      </div>
      <p className="text-xs leading-relaxed text-(--base-06)">
        {cap.committedGb} GB committed, {cap.unwrittenGb} GB of it not yet written.
        {' '}
        {cap.availableGb >= 0
          ? `${cap.availableGb} GB left to promise after the ${cap.headroomGb} GB buffer.`
          : `${Math.abs(cap.availableGb)} GB past the ${cap.headroomGb} GB buffer.`}
      </p>
      {!cap.quotaEnforceable && (
        // One line; the explanation and the remedy fold out, so a node card is
        // not dominated by a paragraph about project quotas.
        <Callout foldable tone="warning" icon={<AlertTriangle size={16} />} title="Disk limits are not enforced on this node">
          {/* The remedy belongs here. The warning alone was accurate and useless:
                the node already proved at startup which step is missing, and that
                detail only existed in its log. */}
            Disk limits cannot be enforced here, so they are recorded but nothing stops a server from
            exceeding them. Usage is still measured (via <code className="srv-num">du</code>).
            {' '}Project quotas need xfs mounted with <code className="srv-num">pquota</code>, or ext4 with
            the <code className="srv-num">project</code> feature (<code className="srv-num">tune2fs -O project,quota</code>,
            filesystem unmounted) and mounted with <code className="srv-num">prjquota</code>, and setting a
            limit needs the node container to hold <code className="srv-num">CAP_SYS_ADMIN</code>. The node
            logs which step is missing on startup (search its log for <code className="srv-num">quota:</code>);
            it re-checks on restart.
        </Callout>
      )}
      {cap.inodesFreePercent >= 0 && cap.inodesFreePercent < 10 && (
        <Callout tone="warning" icon={<AlertTriangle size={16} />}>
          Only {cap.inodesFreePercent}% of this filesystem&apos;s inodes are free. When they run out no server on
          it can create a file, a world save included, however much disk space is left. Look for a server
          with an unusual number of small files.
        </Callout>
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
    <div className="srv-surface p-5 flex flex-col gap-4">
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-2.5 min-w-0">
          <span className={`size-2 rounded-full shrink-0 ${isOnline ? 'bg-(--success-light)' : 'bg-(--error)'}`} aria-hidden="true" />
          <div className="min-w-0">
            <p className="text-sm font-semibold text-(--base-09) truncate">{edge.name}</p>
            <p className="srv-num text-[11px] text-(--base-06) truncate">{edge.ip}:{edge.service_port}</p>
          </div>
        </div>
        <span className={`text-xs font-medium capitalize shrink-0 ${isOnline ? 'text-(--success-light)' : 'text-(--error-light)'}`}>
          {edge.status}
        </span>
      </div>

      {(running || latest) && (
        <div className="flex items-center justify-between gap-2 text-[13px]">
          <span className="srv-label flex items-center gap-1.5">
            <Layers size={14} aria-hidden="true" /> Splice
          </span>
          {spliceBehind ? (
            <span className="srv-chip bg-(--warning-ghost)! text-(--warning-light)!">
              <ArrowUpCircle size={12} aria-hidden="true" /> <span className="srv-num">{running ? `v${running}` : 'unknown'} &rarr; v{latest}</span>
            </span>
          ) : imageMismatch ? (
            <Tooltip content={`Running image ${runImg.slice(7, 19)}, SPLICE_IMAGE resolves to ${availImg.slice(7, 19)}. The pinned tag moved; redeploy the edge stack to recreate the sidecar.`}>
              <span className="srv-chip bg-(--error-ghost)! text-(--error-light)!">
                <ArrowUpCircle size={12} aria-hidden="true" /> <span className="srv-num">v{running || latest}</span> &middot; wrong image
              </span>
            </Tooltip>
          ) : (
            <span className="srv-num text-(--base-07)">v{running || latest}</span>
          )}
        </div>
      )}

      {isOnline && stats ? (
        <div className="flex flex-col gap-3 border-t border-(--base-03) pt-3">
          <MeterRow icon={<Cpu size={14} />} label="CPU" detail={`${stats.cpu.toFixed(1)}%`} value={stats.cpu} max={100} />
          <MeterRow
            icon={<MemoryStick size={14} />}
            label="RAM"
            detail={`${formatBytes(stats.ram_used)} / ${formatBytes(stats.ram_total)}`}
            value={stats.ram_pct}
            max={100}
          />
          <Stat icon={<Users size={14} />} label="Players connected" value={stats.active_mc_streams} />
          <div className="flex items-center gap-4 flex-wrap text-xs">
            <span className="flex items-center gap-1.5 srv-num text-(--base-07)">
              <ArrowDownToLine size={14} className="text-(--base-06)" aria-label="Download" />{formatSpeed(stats.rx_speed)}
            </span>
            <span className="flex items-center gap-1.5 srv-num text-(--base-07)">
              <ArrowUpFromLine size={14} className="text-(--base-06)" aria-label="Upload" />{formatSpeed(stats.tx_speed)}
            </span>
            {hasTotal && (
              <span className="flex items-center gap-1.5 srv-num text-(--base-06) ml-auto">
                <HardDrive size={14} aria-hidden="true" />
                {formatBytes((rxBytes ?? 0) + (txBytes ?? 0))} total
              </span>
            )}
          </div>
        </div>
      ) : (
        <p className="border-t border-(--base-03) pt-3 text-[13px] text-(--base-06)">No stats available</p>
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
    <div className="srv-surface">
      <RowList label="Splice versions">
        {rows.map(r => {
          const runningList = [...r.running].sort();
          const pending = r.behind > 0;
          return (
            <Row
              key={r.region}
              leading={<Layers size={14} />}
              title={r.region}
              meta={
                <span className="srv-num">
                  running {runningList.length ? runningList.map(v => `v${v}`).join(', ') : 'unknown'}
                  {r.latest && <> &middot; latest v{r.latest}</>}
                </span>
              }
              trailing={pending ? (
                <span className="srv-chip bg-(--warning-ghost)! text-(--warning-light)!">
                  <ArrowUpCircle size={12} aria-hidden="true" /> Update available
                </span>
              ) : (
                <span className="text-(--success-light)">Up to date</span>
              )}
            />
          );
        })}
      </RowList>
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
    return <EmptyState icon={<Info size={20} />} title="No service errors reported" description="Nothing has reported a problem recently." />;
  }
  return (
    <div className="srv-surface">
      <RowList label="Service errors">
        {entries.map((e, i) => (
          <div key={`${e.ts}-${e.service}-${i}`} role="listitem" className="app-row items-start!">
            <LevelIcon level={e.level} />
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-center gap-2 text-xs">
                <span className="font-medium text-(--accent-light)">{e.service}</span>
                {e.source && <span className="text-(--base-06)">{e.source}</span>}
              </div>
              {/* break-words, not truncate: the whole value of these lines is the
                  address or name at the end of the message. */}
              <p className={`text-sm mt-0.5 break-words ${isAttention(e) ? 'text-(--base-09)' : 'text-(--base-07)'}`}>
                {e.message}
              </p>
            </div>
            <Tooltip content={e.ts || 'no timestamp'}>
              <span className="srv-num text-xs shrink-0 text-(--base-06)">
                {Number.isNaN(Date.parse(e.ts)) ? e.ts || 'no timestamp' : timeAgo(e.ts)}
              </span>
            </Tooltip>
          </div>
        ))}
      </RowList>
    </div>
  );
}

export function LevelIcon({ level }: { level: ServiceErrorEntry['level'] }) {
  const l = level?.toUpperCase();
  if (l === 'ERROR') return <AlertCircle size={16} className="shrink-0 mt-0.5 text-(--error-light)" aria-label="Error" />;
  if (l === 'WARN') return <AlertTriangle size={16} className="shrink-0 mt-0.5 text-(--warning-light)" aria-label="Warning" />;
  return <Info size={16} className="shrink-0 mt-0.5 text-(--base-06)" aria-label="Info" />;
}
