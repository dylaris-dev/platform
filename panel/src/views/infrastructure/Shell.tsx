"use client";

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import React from 'react';
import { Server, Network, Activity, RefreshCw, Globe, Users } from 'lucide-react';
import PageHeader from '@/components/ui/PageHeader';
import { SkeletonStatGrid, SkeletonCard, SkeletonText } from '@/components/Skeleton';
import { attentionCount } from '@/lib/serviceErrors';
import { isKind } from '@/lib/nodeKind';
import { useInfra } from './context';
import { useAppData } from '@/lib/AppDataContext';

/**
 * The header, the summary row and the tab bar.
 *
 * The tabs are LINKS, not state. They used to be `useState<Tab>('nodes')`,
 * which meant no tab had a URL and every reload landed on Nodes - you could not
 * send anyone a screen, and refreshing while looking at an error lost your place.
 */

export interface InfraTab {
    slug: string;
    label: string;
    count?: number;
    /** Rendered only when true. See the note on guards below. */
    visible: boolean;
}

const BASE = '/infrastructure';

export default function InfrastructureShell({ children }: { children: React.ReactNode }) {
    const infra = useInfra();
    const { featureFlags, gatewayVisibility } = useAppData();
    const pathname = usePathname();

    const platform = isKind(infra.nodes, 'platform');
    const external = isKind(infra.nodes, 'external');
    const byon = isKind(infra.nodes, 'byon');
    const onlineNodes = infra.nodes.filter(n => n.status === 'online').length;

    // Which tabs EXIST. This decides what is drawn, and deliberately not what is
    // reachable: each page guards itself, because with real URLs a hidden tab is
    // still one typed address away. Hiding a button is presentation; the page
    // refusing is the guard.
    const tabs: InfraTab[] = [
        // The three kinds of machine, always all three. They used to appear only
        // when non-empty, which made "no external nodes" and "this platform has
        // no such thing" the same screen - and the tab an operator needs first
        // is the one for the kind they have not registered yet.
        { slug: 'nodes', label: 'Nodes', count: platform.length, visible: true },
        // With the managed gateway there is no warp to add one through, so the
        // tab only stays for machines that already exist.
        { slug: 'external', label: 'External nodes', count: external.length, visible: gatewayVisibility.warpTab || external.length > 0 },
        // BYON, not "Customer nodes": it is the word used everywhere else in
        // this product - the setting, the plan, the release notes - and one
        // screen calling it something friendlier only costs the reader the
        // connection to all of them.
        { slug: 'byon', label: 'BYON', count: byon.length, visible: featureFlags.byon || byon.length > 0 },
        // No count: the tab lists edges, warp leaders and beam relays, and the
        // only one of the three this screen knows about is the edges. A badge
        // reading 2 next to a list of six is worse than no badge.
        { slug: 'gateway', label: 'Gateway', visible: infra.gatewayDeployed },
        { slug: 'bandwidth', label: 'Bandwidth', visible: infra.gatewayEnabled },
        { slug: 'voice', label: 'Voice', visible: infra.gatewayEnabled },
        // Always shown. Whether there is anything recorded is a SERVER fact
        // (the feature flag, and whether the metrics database opened), and the
        // page says which. Hiding the tab when the panel cannot know would make
        // the feature undiscoverable for exactly the operator who has not
        // switched it on yet - the one person who needs to find it.
        { slug: 'statistics', label: 'Statistics', visible: true },
        // Only ERROR/WARN drive the count: the same streams carry INFO, and a
        // badge that is never zero is a badge nobody reads.
        { slug: 'errors', label: 'Errors', count: attentionCount(infra.errors), visible: infra.errors.length > 0 },
    ];

    if (infra.loading) {
        return (
            <div className="h-full flex flex-col gap-6 overflow-y-auto">
                <PageHeader title="Infrastructure" description="Nodes, gateway and the players on them." />
                <SkeletonStatGrid tiles={2} />
                <SkeletonText width="w-40" className="h-4" />
                <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
                    {Array.from({ length: 3 }).map((_, i) => <SkeletonCard key={i} height="h-56" />)}
                </div>
            </div>
        );
    }

    return (
        <div className="h-full flex flex-col gap-6 overflow-y-auto pb-6">
            <PageHeader
                title="Infrastructure"
                description="Nodes, gateway and the players on them."
                actions={
                    <button
                        onClick={() => infra.refresh(true)}
                        disabled={infra.refreshing}
                        className="btn btn-secondary btn-sm"
                    >
                        <RefreshCw size={14} className={infra.refreshing ? 'animate-spin' : ''} />
                        Refresh
                    </button>
                }
            />

            <div className={`grid gap-3 ${infra.gatewayDeployed ? 'grid-cols-2 md:grid-cols-5' : 'grid-cols-2 md:grid-cols-3'}`}>
                <Tile label="Nodes" value={infra.nodes.length} icon={<Network size={14} />} />
                <Tile label="Online" value={onlineNodes} sub={`/ ${infra.nodes.length}`} icon={<Activity size={14} />} />
                {infra.gatewayDeployed && <Tile label="Edges" value={infra.edges.length} icon={<Server size={14} />} />}
                {infra.gatewayDeployed && <Tile label="Routes" value={infra.routeCount} icon={<Globe size={14} />} />}
                {/* From each server's own list ping, not the edges: an edge stream is
                    also every server-list ping and resume leftover, which is how
                    this card used to count players that were not there. */}
                <Tile label="Players online" value={infra.playersOnline ?? '-'} icon={<Users size={14} />} />
            </div>

            <nav aria-label="Infrastructure sections" className="flex items-center gap-0.5 overflow-x-auto shrink-0">
                {tabs.filter(t => t.visible).map(t => {
                    const href = `${BASE}/${t.slug}`;
                    const active = pathname === href;
                    return (
                        <Link
                            key={t.slug}
                            href={href}
                            aria-current={active ? 'page' : undefined}
                            className="srv-nav-link"
                        >
                            {t.label}
                            {typeof t.count === 'number' && (
                                <span className="srv-num text-[11px] px-1.5 py-0.5 rounded-full bg-(--base-03) text-(--base-06)">
                                    {t.count}
                                </span>
                            )}
                        </Link>
                    );
                })}
            </nav>

            {children}
        </div>
    );
}

/** A quiet summary tile: label, then the number. */
function Tile({ label, value, sub, icon }: { label: string; value: string | number; sub?: string; icon: React.ReactNode }) {
    return (
        <div className="srv-surface p-4 flex flex-col gap-2">
            <span className="srv-label flex items-center gap-1.5"><span className="text-(--base-06)" aria-hidden="true">{icon}</span>{label}</span>
            <div className="flex items-baseline gap-1.5">
                <span className="srv-num text-2xl font-medium text-(--base-09) leading-none">{value}</span>
                {sub && <span className="srv-num text-sm text-(--base-06)">{sub}</span>}
            </div>
        </div>
    );
}
