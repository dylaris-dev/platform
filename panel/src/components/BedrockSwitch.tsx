"use client";

import React, { useEffect, useState } from 'react';
import { Loader2 } from 'lucide-react';
import Switch from '@/components/ui/Switch';
import CopyText from '@/components/ui/CopyText';
import { toast } from '@/components/ui/Toast';
import { getServerBedrock, type ServerBedrock } from '@/lib/api';
import { changeBedrock, floodgateLink, showBedrockSwitch } from '@/lib/bedrock';

/**
 * The per-server Bedrock switch, laid out like VoiceChatSwitch.
 *
 * Renders nothing unless Core says Bedrock may be turned on here, except
 * while it is already on: an owner must always be able to turn it off. The
 * Floodgate key is written on the next start, so enabling shows a restart hint.
 */
export default function BedrockSwitch({ serverId }: { serverId: number }) {
    const [bedrock, setBedrock] = useState<ServerBedrock | null>(null);
    const [busy, setBusy] = useState(false);
    const [slow, setSlow] = useState(false);
    const [error, setError] = useState('');
    const [pendingRestart, setPendingRestart] = useState(false);

    useEffect(() => {
        let cancelled = false;
        // A failed read shows nothing rather than a switch whose state is a guess.
        getServerBedrock(serverId)
            .then(res => { if (!cancelled && res?.success !== false && res?.bedrock) setBedrock(res.bedrock); })
            .catch(() => {});
        return () => { cancelled = true; };
    }, [serverId]);

    // House rule: nothing under ~400 ms, then a spinner.
    useEffect(() => {
        if (!busy) { setSlow(false); return; }
        const t = setTimeout(() => setSlow(true), 400);
        return () => clearTimeout(t);
    }, [busy]);

    if (!bedrock || !showBedrockSwitch(bedrock)) return null;

    const toggle = async (next: boolean) => {
        setError('');
        setBusy(true);
        const err = await changeBedrock(serverId, next);
        setBusy(false);
        if (err) { setError(err); return; }
        setBedrock({ ...bedrock, enabled: next });
        setPendingRestart(next);
        toast(next ? 'Bedrock enabled.' : 'Bedrock disabled.');
    };

    const link = floodgateLink(bedrock.loader);

    return (
        <div className="bg-(--base-02) rounded-md border border-(--base-03) px-3 py-2.5">
            <div className="flex items-center gap-3 justify-between">
                <div className="min-w-0">
                    <div className="text-sm font-medium text-(--base-09)">Bedrock players</div>
                    <div className="text-xs text-(--base-06) mt-0.5">
                        Lets Bedrock players (phone, tablet, Windows) join through the gateway. Consoles cannot add custom servers.
                    </div>
                </div>
                <span className="flex items-center gap-2 shrink-0" aria-busy={busy}>
                    {slow && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-hidden="true" />}
                    <Switch
                        checked={bedrock.enabled}
                        onChange={toggle}
                        disabled={busy}
                        ariaLabel="Bedrock players"
                    />
                </span>
            </div>
            {bedrock.enabled && !bedrock.available && (
                <p className="text-xs text-(--warning-light) mt-2">
                    No Bedrock entry is running in this region, so Bedrock players cannot join right now.
                </p>
            )}
            {bedrock.enabled && (
                <div className="text-xs text-(--base-06) mt-2 space-y-1.5">
                    <p>
                        Bedrock players join with{' '}
                        <CopyText value={bedrock.address} className="text-xs font-mono text-(--base-08)" />
                        , no port. Install Floodgate on this server; the key is written for you on the next start.
                    </p>
                    <p>
                        <a
                            href={link.href}
                            target="_blank"
                            rel="noopener noreferrer"
                            className="underline underline-offset-2 hover:text-(--base-09) focus-visible:text-(--base-09)"
                        >
                            {link.label}
                        </a>
                    </p>
                </div>
            )}
            {pendingRestart && (
                <p className="text-xs text-(--warning-light) mt-2">Applies on next restart. Restart the server to finish.</p>
            )}
            {error && <div className="alert alert-error text-xs mt-2" role="alert">{error}</div>}
        </div>
    );
}
