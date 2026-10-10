"use client";

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import { useAppData } from '@/lib/AppDataContext';
import { InfraProvider } from '@/views/infrastructure/context';
import InfrastructureShell from '@/views/infrastructure/Shell';

/**
 * The infrastructure screen is one route per tab, so the admin check and the
 * single overview fetch live here rather than in each page. A page fetching for
 * itself would turn one request into one per tab switch.
 */
export default function InfrastructureLayout({ children }: { children: React.ReactNode }) {
    const router = useRouter();
    const { gatewayEnabled, user, ready } = useAppData();

    useEffect(() => {
        if (!ready) return;
        if (!user?.isAdmin) router.replace('/servers');
    }, [ready, user?.isAdmin, router]);

    if (!ready) return null;
    if (!user?.isAdmin) {
        return (
            <main className="flex-1 flex items-center justify-center text-(--error-light) font-semibold text-xl font-display">
                Access denied. Administrator rights required.
            </main>
        );
    }

    return (
        <main className="flex-1 flex flex-col overflow-hidden relative z-10 p-6">
            <InfraProvider gatewayEnabled={gatewayEnabled}>
                <InfrastructureShell>{children}</InfrastructureShell>
            </InfraProvider>
        </main>
    );
}
