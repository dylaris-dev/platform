"use client";

import React, { useEffect } from 'react';
import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { useAppData } from '@/lib/AppDataContext';
import PageHeader from '@/components/ui/PageHeader';
import EmptyState from '@/components/ui/EmptyState';
import { ShieldAlert } from 'lucide-react';

// Routes are gateway records only; direct-mode server domains live on each
// server's Network tab. Without gateway routing the tab led to an empty hint, so
// it is hidden. The page keeps its own explanation for a bookmarked link.
const TABS = [
    { slug: 'servers', label: 'Servers' },
    { slug: 'users', label: 'Users' },
    { slug: 'routes', label: 'Routes', gatewayOnly: true },
    { slug: 'disk', label: 'Disk Analysis' },
];

export default function AdminLayout({ children }: { children: React.ReactNode }) {
    const pathname = usePathname();
    const router = useRouter();
    const { user, ready, gatewayEnabled } = useAppData();

    useEffect(() => {
        if (!ready) return;
        if (!user?.isAdmin) router.replace('/servers');
    }, [ready, user?.isAdmin, router]);

    if (!ready) return null;
    if (!user?.isAdmin) {
        return (
            <main className="flex-1 flex items-center justify-center p-6">
                <EmptyState tone="error" icon={<ShieldAlert size={20} />} title="Access denied" description="Administrator rights required." />
            </main>
        );
    }

    return (
        <main className="app-page flex-1 flex flex-col overflow-hidden relative z-10">
            <PageHeader title="Admin" description="Servers, user accounts, gateway routes and storage diagnostics" className="mb-4" />

            <nav aria-label="Admin sections" className="flex gap-0.5 mb-6 shrink-0">
                {TABS.filter(t => !t.gatewayOnly || gatewayEnabled).map(t => {
                    const href = `/admin/${t.slug}`;
                    const isActive = pathname === href || pathname.startsWith(href + '/');
                    return (
                        <Link
                            key={t.slug}
                            href={href}
                            replace
                            aria-current={isActive ? 'page' : undefined}
                            className="srv-nav-link"
                        >
                            {t.label}
                        </Link>
                    );
                })}
            </nav>

            <div className="flex-1 min-h-0 overflow-auto">
                {children}
            </div>
        </main>
    );
}
