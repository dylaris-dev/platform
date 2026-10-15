'use client';

import HelpTip from '@/components/ui/HelpTip';
import { formatMB } from '@/lib/format';

/** What the two memory figures mean; shown next to both of them. */
export default function MemoryHelp({ bookedMb, limitMb }: { bookedMb: number; limitMb: number }) {
    const headroom = limitMb - bookedMb;
    return (
        <HelpTip label="About memory">
            <p><strong>Heap</strong> is what the Minecraft server holds after its last garbage collection, out of the most Java may use (-Xmx). This is the number to watch.</p>
            <p className="mt-2"><strong>Process</strong> is the whole Java process, out of the container limit
                {bookedMb > 0 && limitMb > 0 ? <> ({formatMB(bookedMb)} booked{headroom > 0 ? <> + {formatMB(headroom)} headroom</> : null})</> : null}.
                It stays close to the limit because Java reserves its heap up front, so a high process figure alone is normal.</p>
            <p className="mt-2">Heap is not shown on Java 8, and only appears after the first garbage collection.</p>
        </HelpTip>
    );
}
