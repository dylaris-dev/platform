"use client";

import { useRouter } from 'next/navigation';
import { useAppData } from '@/lib/AppDataContext';
import SetupView from '@/views/SetupView';
import { useRouteId } from '@/lib/routeParams';

export default function ServerSetupPage() {
    const paramId = useRouteId('servers');
    const router = useRouter();
    const { servers, libraryEnabled, refreshServers } = useAppData();
    const server = servers.find(s => s.id === Number(paramId));
    if (!server) return null;
    return (
        <SetupView
            server={server}
            libraryEnabled={libraryEnabled}
            onSetupComplete={refreshServers}
            // After the refresh, so the console tab is no longer greyed out as
            // "pending setup" when it opens.
            onInstalled={async () => { await refreshServers(); router.push(`/servers/${server.id}/console`); }}
        />
    );
}
