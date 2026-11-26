"use client";

import React from 'react';
import { usePathname } from 'next/navigation';
import Link from 'next/link';
import { useAppData } from '@/lib/AppDataContext';
import { Settings2, Image as ImageIcon, Activity, Clock, LayoutGrid, Network as NetworkIcon, FileCode2 } from 'lucide-react';
import '@/styles/server-config.css';
import { useRouteId } from '@/lib/routeParams';

// Configuration sub-tab strip. Each sub-tab is a nested route under
// /servers/{id}/config/<slug>; the strip lives in this layout so every page
// inherits it. The Proxy sub-tab only appears for proxy servers. The parent
// shell supplies the page padding.
export default function ServerConfigLayout({ children }: { children: React.ReactNode }) {
    const paramId = useRouteId('servers');
    const pathname = usePathname();
    const { servers, featureFlags } = useAppData();

    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);
    const isProxy = server?.serverType === 'proxy';

    const subTabs: { slug: string; label: string; Icon: React.ComponentType<{ size?: number }> }[] = [
        { slug: 'properties', label: 'Properties', Icon: Settings2 },
        { slug: 'display',    label: 'Display',    Icon: ImageIcon },
        { slug: 'profiling',  label: 'Profiling',  Icon: Activity },
        { slug: 'scheduled',  label: 'Scheduled',  Icon: Clock },
        { slug: 'tabs',       label: 'Tabs',       Icon: LayoutGrid },
        ...(isProxy ? [{ slug: 'proxy', label: 'Proxy', Icon: NetworkIcon }] : []),
        // Beta, offered per the admin's Features setting (admins only, or everyone).
        ...(featureFlags?.modConfigs ? [{ slug: 'mods', label: 'Mod configs', Icon: FileCode2 }] : []),
    ];

    return (
        <>
            <nav aria-label="Configuration sections" className="srv-config-nav">
                {subTabs.map(({ slug, label, Icon }) => {
                    const href = `/servers/${serverId}/config/${slug}`;
                    return (
                        <Link
                            key={slug}
                            href={href}
                            replace
                            aria-current={pathname === href ? 'page' : undefined}
                            className="srv-config-link"
                        >
                            <Icon size={14} />
                            <span>{label}</span>
                        </Link>
                    );
                })}
            </nav>
            {children}
        </>
    );
}
