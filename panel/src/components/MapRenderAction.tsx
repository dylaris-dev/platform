"use client";

import { useState } from 'react';
import { Map as MapIcon } from 'lucide-react';
import { getFileContent, sendConsoleCommand } from '@/lib/api';
import { parseProperties } from '@/lib/properties-codec';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { toast } from '@/components/ui/Toast';
import { mapRenderCommand } from '@/lib/mapRender';

/**
 * "Render whole map" for a web map tab. A map plugin renders what the game
 * touches; a world that was generated before it (or pregenerated with Chunky)
 * stays blank until a full render runs once. Shown only for the ports the
 * common map plugins listen on; the command goes through the console, so the
 * reader needs console access for it to do anything.
 */
export default function MapRenderAction({ serverId, serverUuid, subServer, port }: {
    serverId: number;
    serverUuid: string;
    subServer: string;
    port: number;
}) {
    const [busy, setBusy] = useState(false);
    const kind = port === 8100 ? 'bluemap' : port === 8080 ? 'squaremap' : null;
    if (!kind) return null;

    const run = async () => {
        if (!await confirmDialog({
            title: 'Render the whole map?',
            message: kind === 'bluemap'
                ? 'Runs "bluemap update" in the console: BlueMap renders every chunk it has not rendered yet. On a big world this takes a while and uses CPU; players can stay online.'
                : 'Runs "map fullrender" in the console: squaremap (or Pl3xMap) renders every chunk already generated. On a big world this takes a while and uses CPU; players can stay online.',
            confirmLabel: 'Render',
        })) return;
        setBusy(true);
        try {
            let world = 'world';
            if (kind === 'squaremap' && subServer) {
                const res = await getFileContent(`${subServer}/server.properties`, serverUuid).catch(() => null);
                const text = res && res.success !== false ? (res.content ?? res.text ?? '') : '';
                world = parseProperties(text).values['level-name'] || 'world';
            }
            const res = await sendConsoleCommand(serverId, mapRenderCommand(kind, world));
            if (res?.success === false) toast(res.message || 'Could not send the command', false);
            else toast('Render started. The map fills in as it goes; the console shows the progress.', true);
        } finally {
            setBusy(false);
        }
    };

    return (
        <button type="button" onClick={run} disabled={busy} className="btn btn-secondary btn-sm" title="Render every generated chunk once">
            <MapIcon size={12} aria-hidden="true" /> {busy ? 'Starting...' : 'Render whole map'}
        </button>
    );
}
