"use client";

import { useEffect, useState } from 'react';
import { Loader2 } from 'lucide-react';
import Segmented from '@/components/ui/Segmented';
import { Badge } from '@/components/ui/Badge';
import { toast } from '@/components/ui/Toast';
import type { ServerBedrock } from '@/lib/api';
import {
    addBlockedReason, canAddBedrock, changeEdition, editionOf, editionOptions,
    floodgateLink, pausedNotice, type Edition,
} from '@/lib/bedrock';

/**
 * The Bedrock notes above a server's route list: what a Bedrock player
 * needs, and why Bedrock cannot be added or is paused. Hidden by the caller
 * unless showBedrock() says so.
 */
export function BedrockSummary({ bedrock }: { bedrock: ServerBedrock }) {
    const link = floodgateLink(bedrock.loader);
    return (
        <div className="bg-(--base-02) rounded-md border border-(--base-03) px-3 py-2.5 space-y-1.5">
            <div className="text-sm font-medium text-(--base-09)">Bedrock players</div>
            <p className="text-xs text-(--base-06)">
                Choose per route whether it takes Java, Bedrock or both. Bedrock players (phone, tablet, Windows) type
                the route&apos;s address with no port; consoles cannot add custom servers.
            </p>
            {bedrock.enabled && !bedrock.active && (
                <p className="text-xs text-(--warning-light)">
                    Bedrock is turned off on this platform. Your routes keep their setting and take Bedrock players
                    again once it is back on.
                </p>
            )}
            {bedrock.enabled && bedrock.active && !bedrock.available && (
                <p className="text-xs text-(--warning-light)">
                    No Bedrock entry is running in this region, so Bedrock players cannot join right now.
                </p>
            )}
            {!canAddBedrock(bedrock) && bedrock.enabled && (
                <p className="text-xs text-(--base-06)">New Bedrock routes: {addBlockedReason(bedrock)}</p>
            )}
            <p className="text-xs text-(--base-06)">
                Needs{' '}
                <a
                    href={link.href}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="underline underline-offset-2 hover:text-(--base-09) focus-visible:text-(--base-09)"
                >
                    {link.label}
                </a>{' '}
                in {bedrock.loader}/. Its key is written for you on the next start, so restart once after the first
                Bedrock route.
            </p>
        </div>
    );
}

/** The Java / Java + Bedrock / Bedrock choice on one existing route. */
export function RouteEdition({ serverId, domain, bedrock, onChanged }: {
    serverId: number;
    domain: string;
    bedrock: ServerBedrock;
    onChanged: (domain: string, e: Edition) => void;
}) {
    const current = editionOf(bedrock, domain);
    const [busy, setBusy] = useState(false);
    const [slow, setSlow] = useState(false);
    const [error, setError] = useState('');

    // House rule: nothing under ~400 ms, then a spinner.
    useEffect(() => {
        if (!busy) { setSlow(false); return; }
        const t = setTimeout(() => setSlow(true), 400);
        return () => clearTimeout(t);
    }, [busy]);

    const pick = async (next: Edition) => {
        if (next === current || busy) return;
        setError('');
        setBusy(true);
        const err = await changeEdition(serverId, domain, next);
        setBusy(false);
        if (err) { setError(err); return; }
        onChanged(domain, next);
        toast(next === 'java' ? `${domain}: Java only.` : next === 'both' ? `${domain}: Java and Bedrock.` : `${domain}: Bedrock only.`);
    };

    const notice = pausedNotice(current, bedrock.active);
    return (
        <div className="mt-1.5 space-y-1">
            <span className="flex flex-wrap items-center gap-2" aria-busy={busy}>
                <Segmented
                    ariaLabel={`Players on ${domain}`}
                    value={current}
                    onChange={pick}
                    options={editionOptions(current, canAddBedrock(bedrock), addBlockedReason(bedrock))}
                />
                {current !== 'java' && !bedrock.active && <Badge variant="warning">Bedrock paused</Badge>}
                {slow && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-hidden="true" />}
            </span>
            {notice && <p className="text-xs text-(--warning-light)">{notice}</p>}
            {error && <div className="alert alert-error text-xs" role="alert">{error}</div>}
        </div>
    );
}
