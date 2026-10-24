"use client";

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import { useAppData } from '@/lib/AppDataContext';
import { useRouteId } from '@/lib/routeParams';

export default function ServerIndexPage() {
    const paramId = useRouteId('servers');
    const router = useRouter();
    const { servers, ready } = useAppData();

    useEffect(() => {
        if (!ready) return;
        const id = Number(paramId);
        const srv = servers.find(s => s.id === id);
        if (!srv) return; // Layout will show "not found"
        const target = srv.status === 'pending_setup' ? 'setup' : 'overview';
        router.replace(`/servers/${id}/${target}`);
    }, [ready, paramId, servers, router]);

    // Only on screen for the moment the redirect takes; the server header and
    // tabs above are already there, so a word in the middle would just flash.
    return <div className="h-full" aria-busy="true" />;
}
