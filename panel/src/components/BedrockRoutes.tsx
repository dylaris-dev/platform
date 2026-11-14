"use client";

import { useEffect, useId, useState } from 'react';
import { Loader2 } from 'lucide-react';
import Segmented from '@/components/ui/Segmented';
import { Badge } from '@/components/ui/Badge';
import { toast } from '@/components/ui/Toast';
import type { ServerBedrock } from '@/lib/api';
import {
    addBlockedReason, bedrockTakenElsewhere, canAddBedrock, editionBlockedNote, editionOf, editionOptions,
    floodgateLink, isOwnDomainRoute, pausedNotice, saveEdition, type Edition,
} from '@/lib/bedrock';
import { useAppData } from '@/lib/AppDataContext';

/**
 * The Bedrock notes above a server's route list: what a Bedrock player
 * needs, and why Bedrock cannot be added or is paused. Hidden by the caller
 * unless showBedrock() says so.
 */
export function BedrockSummary({ bedrock }: { bedrock: ServerBedrock }) {
    const link = floodgateLink(bedrock.loader);
    const { gatewayProvider } = useAppData();
    if (gatewayProvider === 'remote') return <ManagedBedrockSummary bedrock={bedrock} link={link} />;
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

/**
 * BedrockSummary on the managed gateway. The provider holds the editions, so
 * nothing is paused here, and the node writes the provider's key: there is no
 * key to download. The platform switch only gates giving an address Bedrock.
 */
function ManagedBedrockSummary({ bedrock, link }: { bedrock: ServerBedrock; link: { label: string; href: string } }) {
    return (
        <div className="bg-(--base-02) rounded-md border border-(--base-03) px-3 py-2.5 space-y-1.5">
            <div className="text-sm font-medium text-(--base-09)">Bedrock players</div>
            <p className="text-xs text-(--base-06)">
                Bedrock players join through the managed gateway. One address per server, on the gateway
                provider&apos;s domain: own domains are not supported yet. Bedrock players (phone, tablet, Windows)
                type the address with no port; consoles cannot add custom servers.
            </p>
            {bedrock.enabled && !bedrock.available && bedrock.refusal && (
                <p className="text-xs text-(--warning-light)">Bedrock players cannot join right now. {bedrock.refusal}</p>
            )}
            {bedrock.enabled && !bedrock.allowed && (
                <p className="text-xs text-(--base-06)">
                    Giving an address Bedrock is turned off on this platform. The address that has it keeps it.
                </p>
            )}
            {!bedrock.enabled && (
                <p className="text-xs text-(--base-06)">
                    If Bedrock went off on its own, the managed gateway&apos;s side changed: switch it on again, then
                    restart the server.
                </p>
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
                in {bedrock.loader}/. The managed gateway&apos;s key is written for you on the next start, so restart
                the server after giving an address Bedrock.
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
    const { gatewayProvider } = useAppData();
    // The managed gateway allows one Bedrock address per server.
    const takenBy = gatewayProvider === 'remote' ? bedrockTakenElsewhere(bedrock, domain) : null;
    // Not offered on an own domain yet, or the server's one Bedrock address is
    // another: said here, not only as a hint on the disabled options.
    const ownDomain = gatewayProvider === 'remote' && isOwnDomainRoute(bedrock, domain);
    const blockedNote = editionBlockedNote(current, takenBy, ownDomain);
    const ownNoteId = useId();
    const [busy, setBusy] = useState(false);
    const [slow, setSlow] = useState(false);
    const [error, setError] = useState('');
    const [restartNote, setRestartNote] = useState(false);
    const [warning, setWarning] = useState('');

    // House rule: nothing under ~400 ms, then a spinner.
    useEffect(() => {
        if (!busy) { setSlow(false); return; }
        const t = setTimeout(() => setSlow(true), 400);
        return () => clearTimeout(t);
    }, [busy]);

    const pick = async (next: Edition) => {
        if (next === current || busy) return;
        setError('');
        setWarning('');
        setBusy(true);
        const res = await saveEdition(serverId, domain, next);
        setBusy(false);
        if (res.error) { setError(res.error); return; }
        // Only the managed gateway sends these: a new key, or one not yet on the node.
        setRestartNote(res.restartRequired);
        setWarning(res.warning);
        onChanged(domain, next);
        toast(next === 'java' ? `${domain}: Java only.` : next === 'both' ? `${domain}: Java and Bedrock.` : `${domain}: Bedrock only.`);
    };

    const notice = pausedNotice(current, bedrock.active);
    return (
        <div className="mt-1.5 space-y-1">
            <span className="flex flex-wrap items-center gap-2" aria-busy={busy} aria-describedby={blockedNote ? ownNoteId : undefined}>
                <Segmented
                    ariaLabel={`Players on ${domain}`}
                    value={current}
                    onChange={pick}
                    options={editionOptions(current, canAddBedrock(bedrock), addBlockedReason(bedrock), takenBy, ownDomain)}
                />
                {current !== 'java' && !bedrock.active && <Badge variant="warning">Bedrock paused</Badge>}
                {slow && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-hidden="true" />}
            </span>
            {blockedNote && <p id={ownNoteId} className="text-xs text-(--base-06)">{blockedNote}</p>}
            {notice && <p className="text-xs text-(--warning-light)">{notice}</p>}
            {restartNote && (
                <p className="text-xs text-(--warning-light)" role="status">Restart the server so it loads the new Bedrock key.</p>
            )}
            {warning && <div className="alert alert-warning text-xs" role="status">{warning}</div>}
            {error && <div className="alert alert-error text-xs" role="alert">{error}</div>}
        </div>
    );
}
