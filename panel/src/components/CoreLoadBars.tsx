"use client";

import { useEffect, useState } from 'react';
import { getServerCoreLoad, type ServerCoreLoad } from '@/lib/api';
import { Meter, MeterIndicator, MeterTrack } from '@ui-sdk/react/meter';
import Tooltip from '@/components/ui/Tooltip';

// Same cadence as the node's sampler; asking faster only repeats a sample.
const POLL_MS = 2000;

/**
 * The cores a pinned server runs on, one bar each. Core answers with no cores
 * unless the numbers mean something to this reader: a member sees them only
 * while no other server can run on those cores, an admin always, with the
 * shared ones marked. Renders nothing when there is nothing to show.
 */
export default function CoreLoadBars({ serverId, active, cpuLimit }: { serverId: number; active: boolean; cpuLimit?: number }) {
  const [data, setData] = useState<ServerCoreLoad | null>(null);

  useEffect(() => {
    // Another server's bars are not this one's: start empty.
    setData(null);
    if (!active) return;
    let cancelled = false;
    let inFlight = false;
    let misses = 0;
    const tick = async () => {
      if (document.visibilityState !== 'visible' || inFlight) return;
      inFlight = true;
      try {
        const res = await getServerCoreLoad(serverId);
        if (cancelled) return;
        if (Array.isArray(res?.cores)) { setData(res); misses = 0; }
        else if (++misses >= 3) setData(null);
      } catch {
        // One failed poll keeps the last reading; a run of them is no reading.
        if (!cancelled && ++misses >= 3) setData(null);
      } finally {
        inFlight = false;
      }
    };
    tick();
    const t = setInterval(tick, POLL_MS);
    return () => { cancelled = true; clearInterval(t); };
  }, [serverId, active]);

  if (!active || !data || data.cores.length === 0) return null;
  const anyShared = data.cores.some(c => c.shared);

  return (
    <div className="srv-surface px-4 py-3">
      <div className="flex items-center justify-between gap-3 pb-2">
        <span className="srv-label">
          {/* The bars are where the server MAY run (its pin), the limit is how
              much CPU time it gets across them: 4 cores of time on 8 pinned
              cores is 4, spread over 8. */}
          {cpuLimit && cpuLimit > 0 && cpuLimit < data.cores.length
            ? `CPU limit ${cpuLimit} of ${data.cores.length} pinned cores`
            : 'CPU cores'}
        </span>
        {anyShared && (
          <span className="text-[11px] text-(--warning-light)">
            Marked cores are shared: their load includes other servers.
          </span>
        )}
      </div>
      <ul className="grid gap-x-4 gap-y-2" style={{ gridTemplateColumns: 'repeat(auto-fill, minmax(140px, 1fr))' }}>
        {data.cores.map(c => (
          <li key={c.index} className="flex flex-col gap-1">
            <div className="flex items-center justify-between text-xs">
              {c.shared ? (
                <Tooltip content="Other servers can run on this core, so this load includes theirs.">
                  <span className="text-(--warning-light)">
                    {c.id !== undefined ? `Core ${c.id}` : `Core ${c.index}`} &middot; shared
                  </span>
                </Tooltip>
              ) : (
                <span className="text-(--base-07)">{c.id !== undefined ? `Core ${c.id}` : `Core ${c.index}`}</span>
              )}
              <span className="srv-num text-(--base-08)">{c.load.toFixed(0)}%</span>
            </div>
            <Meter value={Math.min(100, Math.max(0, Math.round(c.load)))} min={0} max={100} aria-label={`Core ${c.id ?? c.index} load`}>
              <MeterTrack className="srv-meter-track">
                <MeterIndicator className="srv-meter-fill" data-tone={c.shared ? 'warning' : undefined} />
              </MeterTrack>
            </Meter>
          </li>
        ))}
      </ul>
    </div>
  );
}
