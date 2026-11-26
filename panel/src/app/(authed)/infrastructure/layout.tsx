"use client";

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import { useAppData } from '@/lib/AppDataContext';
import EmptyState from '@/components/ui/EmptyState';
import { ShieldAlert } from 'lucide-react';
import { InfraProvider } from '@/views/infrastructure/context';
import InfrastructureShell from '@/views/infrastructure/Shell';

/**
 * The infrastructure screen is one route per tab, so the admin check and the
 * single overview fetch live here rather than in each page. A page fetching for
 * itself would turn one request into one per tab switch.
 */
export default function InfrastructureLayout({ children }: { children: React.ReactNode }) {
    const router = useRouter();
    const { gatewayEnabled, user, ready, gatewayVisibility } = useAppData();

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
            {/* With the managed gateway the provider runs the edges, links and
                bandwidth, so this screen has none of its own to show. */}
            <InfraProvider gatewayEnabled={gatewayEnabled && gatewayVisibility.infrastructureGateway}>
                <InfrastructureShell>{children}</InfrastructureShell>
            </InfraProvider>
        </main>
    );
}
