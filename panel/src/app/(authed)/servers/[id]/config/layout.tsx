"use client";

import React from 'react';
import { usePathname } from 'next/navigation';
import Link from 'next/link';
import { useAppData } from '@/lib/AppDataContext';
import { Settings2, Image as ImageIcon, Activity, Clock, LayoutGrid, Network as NetworkIcon, FileCode2 } from 'lucide-react';
import { useRouteId } from '@/lib/routeParams';

// Configuration sub-tab strip. Each sub-tab is a nested route under
// /servers/{id}/config/<slug>; the strip lives in this layout so every page
// inherits it without duplicating the nav code. Proxy sub-tab is conditional
// — only appears when the server is a proxy type.
//
// The parent server layout already wraps in flex-1 overflow-y-auto p-6, so
// here we offset (-mx-6 -mt-6) to pin the strip to the top of the content
// area without inheriting that padding, then leave per-page padding to each
// sub-tab page.
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
            <div className="-mx-6 -mt-6 mb-4 border-b border-(--base-03) bg-(--base-01) px-4">
                <nav className="flex gap-0.5 overflow-x-auto">
                    {subTabs.map(({ slug, label, Icon }) => {
                        const href = `/servers/${serverId}/config/${slug}`;
                        const active = pathname === href;
                        return (
                            <Link
                                key={slug}
                                href={href}
                                replace
                                className={`flex items-center gap-1.5 px-3 py-2 text-xs font-medium border-b-2 transition-colors whitespace-nowrap ${
                                    active
                                        ? 'border-(--accent) text-(--accent-light)'
                                        : 'border-transparent text-(--base-07) hover:text-(--base-09) hover:border-(--base-04)'
                                }`}
                            >
                                <Icon size={13} />
                                <span>{label}</span>
                            </Link>
                        );
                    })}
                </nav>
            </div>
            {children}
        </>
    );
}
