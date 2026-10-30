"use client";

import { useEffect, useState } from 'react';
import { Download, Loader2 } from 'lucide-react';
import Segmented from '@/components/ui/Segmented';
import { Badge } from '@/components/ui/Badge';
import { toast } from '@/components/ui/Toast';
import { downloadLinkRouteFloodgateKey, setLinkRouteBedrock, type LinkRouteBedrock as View } from '@/lib/api';
import { editionFlags, editionOptions, linkEdition, pausedNotice, type Edition } from '@/lib/bedrock';

/**
 * Java / Java + Bedrock / Bedrock for one route-only address, one block in the
 * route list like LinkRouteVoiceSwitch. Nothing writes the Floodgate key onto
 * the customer's server, so once Bedrock is on the panel hands them the key
 * and says where it goes.
 */
export default function LinkRouteBedrock({ domain, bedrock, allowed, active, onSaved }: {
    domain: string;
    bedrock: View | undefined;
    allowed: boolean;
    active: boolean;
    onSaved: (v: View) => void;
}) {
    const current = linkEdition(bedrock);
    const canAdd = allowed && !bedrock?.refusal;
    const why = !allowed ? 'Bedrock is turned off on this platform.' : bedrock?.refusal || '';
    const [busy, setBusy] = useState(false);
    const [slow, setSlow] = useState(false);
    const [error, setError] = useState('');

    // House rule: nothing under ~400 ms, then a spinner.
    useEffect(() => {
        if (!busy) { setSlow(false); return; }
        const t = setTimeout(() => setSlow(true), 400);
        return () => clearTimeout(t);
    }, [busy]);

    // Shown when Bedrock can be added, or the address already has it: an owner
    // must always be able to take it away again.
    if (!canAdd && current === 'java') return null;

    const pick = async (next: Edition) => {
        if (next === current || busy) return;
        setError('');
        setBusy(true);
        try {
            const res = await setLinkRouteBedrock(domain, editionFlags(next));
            if (res?.success === false || !res?.bedrock) {
                setError(res?.message || res?.error || 'The route could not be changed.');
                return;
            }
            onSaved({ ...res.bedrock, refusal: bedrock?.refusal });
            toast(next === 'java' ? `${domain}: Java only.` : next === 'both' ? `${domain}: Java and Bedrock.` : `${domain}: Bedrock only.`);
        } catch {
            setError('The request could not be sent.');
        } finally {
            setBusy(false);
        }
    };

    const download = async () => {
        setError('');
        try {
            await downloadLinkRouteFloodgateKey(domain);
        } catch {
            setError('The key could not be downloaded. Try again.');
        }
    };

    const notice = pausedNotice(current, active);
    return (
        <div className="mt-1.5 space-y-1.5">
            <span className="flex flex-wrap items-center gap-2" aria-busy={busy}>
                <span className="text-xs text-(--base-07)">Players</span>
                <Segmented
                    ariaLabel={`Players on ${domain}`}
                    value={current}
                    onChange={pick}
                    options={editionOptions(current, canAdd, why)}
                />
                {current !== 'java' && !active && <Badge variant="warning">Bedrock paused</Badge>}
                {slow && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-hidden="true" />}
            </span>
            {notice && <p className="text-xs text-(--warning-light)">{notice}</p>}
            {current !== 'java' && bedrock?.has_key && (
                <div className="space-y-1.5 text-xs text-(--base-06)">
                    <p>
                        Install{' '}
                        <a href="https://geysermc.org/download?project=floodgate" target="_blank" rel="noopener noreferrer"
                            className="underline underline-offset-2 hover:text-(--base-09) focus-visible:text-(--base-09)">Floodgate</a>{' '}
                        on the server (the plugin for Paper or Spigot, the mod for Fabric or NeoForge; not Geyser). Save this
                        address&apos;s key as <span className="font-mono">key.pem</span> in{' '}
                        <span className="font-mono">plugins/floodgate/</span> or <span className="font-mono">config/floodgate/</span>,
                        replacing the one Floodgate made, then restart the server.
                    </p>
                    <button type="button" onClick={download} className="btn btn-secondary inline-flex items-center gap-2 text-xs py-1">
                        <Download size={14} aria-hidden="true" />
                        Download key.pem
                    </button>
                </div>
            )}
            {error && <div className="alert alert-error text-xs" role="alert">{error}</div>}
        </div>
    );
}
