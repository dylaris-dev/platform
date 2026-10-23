"use client";

import { useEffect, useState } from 'react';
import { getServerCoreLoad, type ServerCoreLoad } from '@/lib/api';

// Same cadence as the node's sampler; asking faster only repeats a sample.
const POLL_MS = 2000;

/**
 * The cores a pinned server runs on, one bar each. Core answers with no cores
 * unless the numbers mean something to this reader: a member sees them only
 * while no other server can run on those cores, an admin always, with the
 * shared ones marked. Renders nothing when there is nothing to show.
 */
export default function CoreLoadBars({ serverId, active }: { serverId: number; active: boolean }) {
  const [data, setData] = useState<ServerCoreLoad | null>(null);

  useEffect(() => {
    if (!active) { setData(null); return; }
    let cancelled = false;
    const tick = async () => {
      if (document.visibilityState !== 'visible') return;
      try {
        const res = await getServerCoreLoad(serverId);
        if (!cancelled && Array.isArray(res?.cores)) setData(res);
      } catch { /* keep the last reading; the next tick tries again */ }
    };
    tick();
    const t = setInterval(tick, POLL_MS);
    return () => { cancelled = true; clearInterval(t); };
  }, [serverId, active]);

  if (!data || data.cores.length === 0) return null;
  const anyShared = data.cores.some(c => c.shared);

  return (
    <div className="rounded-xl bg-(--base-01) border border-(--base-03) px-4 py-3">
      <div className="flex items-center justify-between gap-3 pb-2">
        <span className="input-label text-[11px]">CPU cores</span>
        {anyShared && (
          <span className="text-[11px] text-(--warning-light)">
            Marked cores are shared: their load includes other servers.
          </span>
        )}
      </div>
      <ul className="grid gap-x-4 gap-y-2" style={{ gridTemplateColumns: 'repeat(auto-fill, minmax(140px, 1fr))' }}>
        {data.cores.map(c => (
          <li
            key={c.index}
            className="flex flex-col gap-1"
            title={c.shared ? 'Other servers can run on this core, so this load includes theirs.' : undefined}
          >
            <div className="flex items-center justify-between text-[11px]">
              <span className={c.shared ? 'text-(--warning-light)' : 'text-(--base-07)'}>
                {c.id !== undefined ? `Core ${c.id}` : `Core ${c.index}`}{c.shared ? ' · shared' : ''}
              </span>
              <span className="font-mono tabular-nums text-(--base-08)">{c.load.toFixed(0)}%</span>
            </div>
            <div
              className="h-1.5 rounded-full bg-(--base-03) overflow-hidden"
              role="meter"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={Math.round(c.load)}
              aria-label={`Core ${c.id ?? c.index} load`}
            >
              <div
                className={`h-full rounded-full transition-[width] duration-500 motion-reduce:transition-none ${c.shared ? 'bg-(--warning)' : 'bg-(--primary)'}`}
                style={{ width: `${Math.min(100, Math.max(0, c.load))}%` }}
              />
            </div>
          </li>
        ))}
      </ul>
    </div>
  );
}
