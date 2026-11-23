"use client";

import { useEffect, useState } from 'react';
import { AlertTriangle, ArrowRight } from 'lucide-react';
import Link from 'next/link';
import { useAppData } from '@/lib/AppDataContext';
import { getNodes } from '@/lib/api';
import { nodeBannerState, type BannerNode, type NodeBannerState } from '@/lib/nodeBanner';

// Mounted globally in the authenticated layout, like the billing and storage
// bars. It tells a tenant that one of THEIR machines has stopped talking to us.
//
// It has to be a banner rather than a badge on the infrastructure page: that
// page is opened when setting a machine up and then never again, so the state
// that most needs to reach the owner was on the one screen they had no reason
// to visit. The first thing they noticed was players complaining.
//
// Renders nothing while everything is connected, so it costs a small poll and
// no space in the steady state. Gated on the BYON flag - with the subsystem off
// Core answers 403 for this scope, and the request would be a guaranteed
// refusal every minute per open tab.
const POLL_MS = 60_000;

export default function NodeConnectionBanner() {
    const { featureFlags } = useAppData();
    const [state, setState] = useState<NodeBannerState | null>(null);

    useEffect(() => {
        if (!featureFlags.byon) { setState(null); return; }
        let cancelled = false;
        const load = async () => {
            const res = await getNodes('byon');
            if (cancelled) return;
            const nodes = (res.success && Array.isArray(res.nodes) ? res.nodes : []) as BannerNode[];
            // now() is read here rather than passed down, so the age is measured
            // against the moment the answer arrived and not against a render.
            setState(nodeBannerState(nodes, Date.now()));
        };
        load();
        const t = setInterval(load, POLL_MS);
        return () => { cancelled = true; clearInterval(t); };
    }, [featureFlags.byon]);

    if (!state) return null;

    // Orange for "not responding", red for "offline". The difference is whether
    // it might still come back on its own, which is exactly what decides
    // whether the reader should get up and go look at the machine.
    const down = state.tier === 'down';
    const tone = down ? 'error' : 'warning';

    return (
        <div role="status" className="app-banner shrink-0 items-center" data-tone={tone}>
            <AlertTriangle size={15} className="shrink-0" aria-hidden="true" />
            <span className="min-w-0 flex-1">{state.text}</span>
            <Link
                href="/nodes?tab=byon"
                className="shrink-0 inline-flex items-center gap-1 font-medium underline underline-offset-2
                           hover:no-underline focus-visible:outline-none focus-visible:ring-(--focus-ring) rounded-sm"
            >
                My infrastructure
                <ArrowRight size={13} aria-hidden="true" />
            </Link>
        </div>
    );
}
