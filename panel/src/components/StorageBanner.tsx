"use client";

import React, { useEffect, useState } from 'react';
import { HardDrive, Clock } from 'lucide-react';
import { systemEvents } from '@/lib/systemEvents';
import { getStorageConnection } from '@/lib/api/storageConnection';
import {
    applyStorageConnectionEvent,
    normaliseStorageConnection,
    selectStorageBanner,
    INITIAL_STORAGE_CONNECTION,
    type StorageConnectionState,
} from '@/lib/storageConnection';
import { fmtTime } from '@/lib/time';

// StorageBanner is mounted globally in the authenticated layout. It warns that
// a storage backend is degraded, which matters BEFORE an upload starts: the
// three uploads a user is most likely to run (library, modpack build content,
// ticket attachments) call their API functions directly rather than going
// through UploadManagerWidget, so a per-upload indicator would miss all three.
// Renders nothing while both backends are ok - zero footprint in steady state.
//
// It subscribes to the SSE event directly (rather than via the
// `dylaris:*` window re-broadcast AppDataContext uses for maintenance) because
// this payload CARRIES the new state. Re-broadcasting would mean either
// throwing that payload away and re-fetching, or re-encoding it through an
// untyped CustomEvent detail, for no gain.
export default function StorageBanner() {
    const [state, setState] = useState<StorageConnectionState>(INITIAL_STORAGE_CONNECTION);

    useEffect(() => {
        let cancelled = false;

        const fetchState = async () => {
            try {
                const res = await getStorageConnection();
                if (!cancelled && res.success) {
                    setState(normaliseStorageConnection(res));
                }
            } catch { /* network blip - keep last known state */ }
        };

        fetchState();

        // Not redundant with the SSE subscription below: the stream has no
        // replay and its `hello` frame carries no state, so a client that
        // connects mid-outage sees nothing until the next transition - which
        // may well be the recovery. The poll bounds that blind window to 30s.
        const id = setInterval(fetchState, 30_000);

        const unsub = systemEvents.on('storage.connection.changed', (evt) => {
            if (cancelled) return;
            setState(prev => applyStorageConnectionEvent(prev, evt.payload));
        });

        return () => {
            cancelled = true;
            clearInterval(id);
            unsub();
        };
    }, []);

    const view = selectStorageBanner(state);
    if (!view) return null;

    // Resolved before the chip is rendered: `since` is untrusted wire data, and
    // a stamp that will not parse must drop the whole chip rather than leave a
    // clock icon standing beside an empty string.
    const sinceLabel = view.since ? formatSince(view.since) : '';
    const error = view.severity === 'error';
    const tone = error
        ? 'bg-(--error-ghost) border-(--error)/30 text-(--error-light)'
        : 'bg-(--warning-ghost) border-(--warning)/30 text-(--warning-light)';

    return (
        // role="status" (polite), not role="alert": this is a standing
        // condition that persists on screen, not a transient response to
        // something the user just did. Assertive would cut across whatever a
        // screen reader is currently reading, and would do it again on every
        // flap between the two backends.
        <div role="status" className={`w-full shrink-0 border-b px-4 py-2.5 ${tone}`}>
            <div className="max-w-7xl mx-auto flex flex-col sm:flex-row sm:items-center gap-2 sm:gap-4">
                <div className="flex items-start gap-2 min-w-0 flex-1">
                    <HardDrive size={16} className="shrink-0 mt-0.5" aria-hidden="true" />
                    <div className="min-w-0">
                        <p className="text-sm font-medium">{view.title}</p>
                        <p className="text-xs text-(--base-08) mt-0.5">{view.message}</p>
                    </div>
                </div>
                {sinceLabel && (
                    <div className="flex items-center gap-1.5 text-xs font-mono shrink-0 sm:ml-auto">
                        <Clock size={12} aria-hidden="true" />
                        <span>{sinceLabel}</span>
                    </div>
                )}
            </div>
        </div>
    );
}

// Reports when the outage started, which is an observed fact. It deliberately
// does not estimate when it will end.
function formatSince(iso: string): string {
    const t = new Date(iso).getTime();
    if (Number.isNaN(t)) return '';
    return `since ${fmtTime(t)}`;
}
