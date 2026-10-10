"use client";

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { Server, Network, Activity, RefreshCw, Globe, Users } from 'lucide-react';
import { SkeletonStatGrid, SkeletonCard, SkeletonText } from '@/components/Skeleton';
import { attentionCount } from '@/lib/serviceErrors';
import { isKind } from '@/lib/nodeKind';
import { StatCard } from './InfraCards';
import { useInfra } from './context';

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
        { slug: 'external', label: 'External nodes', count: external.length, visible: true },
        // BYON, not "Customer nodes": it is the word used everywhere else in
        // this product - the setting, the plan, the release notes - and one
        // screen calling it something friendlier only costs the reader the
        // connection to all of them.
        { slug: 'byon', label: 'BYON', count: byon.length, visible: true },
        // No count: the tab lists edges, warp leaders and beam relays, and the
        // only one of the three this screen knows about is the edges. A badge
        // reading 2 next to a list of six is worse than no badge.
        { slug: 'gateway', label: 'Gateway', visible: infra.gatewayDeployed },
        { slug: 'bandwidth', label: 'Bandwidth', visible: infra.gatewayEnabled },
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
            <div className="h-full flex flex-col gap-4 overflow-y-auto">
                <div className="flex items-center justify-between">
                    <div className="flex items-center gap-3">
                        <div className="w-9 h-9 rounded-md bg-(--base-03) flex items-center justify-center">
                            <Server size={18} className="text-(--accent-light)" />
                        </div>
                        <h1 className="h-page">Infrastructure</h1>
                    </div>
                </div>
                <SkeletonStatGrid tiles={2} />
                <SkeletonText width="w-40" className="h-4" />
                <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
                    {Array.from({ length: 3 }).map((_, i) => <SkeletonCard key={i} height="h-56" />)}
                </div>
            </div>
        );
    }

    return (
        <div className="h-full flex flex-col gap-4 overflow-y-auto">
            <div className="flex items-center justify-between">
                <div className="flex items-center gap-3">
                    <div className="w-9 h-9 rounded-md bg-(--base-03) flex items-center justify-center">
                        <Server size={18} className="text-(--accent-light)" />
                    </div>
                    <h1 className="h-page">Infrastructure</h1>
                </div>
                <button
                    onClick={() => infra.refresh(true)}
                    disabled={infra.refreshing}
                    className="btn btn-secondary btn-sm"
                >
                    <RefreshCw size={14} className={infra.refreshing ? 'animate-spin' : ''} />
                    Refresh
                </button>
            </div>

            <div className={`grid gap-3 ${infra.gatewayDeployed ? 'grid-cols-2 md:grid-cols-5' : 'grid-cols-2 md:grid-cols-3'}`}>
                <StatCard label="Nodes" value={infra.nodes.length} icon={<Network size={16} />} />
                <StatCard label="Online" value={onlineNodes} sub={`/ ${infra.nodes.length}`} icon={<Activity size={16} />} />
                {infra.gatewayDeployed && <StatCard label="Edges" value={infra.edges.length} icon={<Server size={16} />} />}
                {infra.gatewayDeployed && <StatCard label="Routes" value={infra.routeCount} icon={<Globe size={16} />} />}
                {/* From each server's own list ping, not the edges: an edge stream is
                    also every server-list ping and resume leftover, which is how
                    this card used to count players that were not there. */}
                <StatCard label="Players online" value={infra.playersOnline ?? '-'} icon={<Users size={16} />} />
            </div>

            <nav aria-label="Infrastructure sections" className="flex items-center gap-0.5 bg-(--base-02) border border-(--base-03) rounded-lg p-1 w-fit">
                {tabs.filter(t => t.visible).map(t => {
                    const href = `${BASE}/${t.slug}`;
                    const active = pathname === href;
                    return (
                        <Link
                            key={t.slug}
                            href={href}
                            aria-current={active ? 'page' : undefined}
                            className={`flex items-center gap-2 px-4 py-1.5 rounded-md text-sm font-medium transition-all ${
                                active ? 'bg-(--accent) text-white shadow-sm' : 'text-(--base-07) hover:text-(--base-09)'
                            }`}
                        >
                            {t.label}
                            {typeof t.count === 'number' && (
                                <span className={`text-[10px] font-mono tabular-nums px-1.5 py-0.5 rounded-full ${
                                    active ? 'bg-white/20 text-white' : 'bg-(--base-03) text-(--base-06)'
                                }`}>
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
