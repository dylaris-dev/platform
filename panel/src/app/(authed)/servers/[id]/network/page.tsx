"use client";

import { useRouter } from 'next/navigation';
import { useAppData } from '@/lib/AppDataContext';
import NetworkView from '@/views/NetworkView';
import { useRouteId } from '@/lib/routeParams';
import ServerDomainsCard from '@/components/server/ServerDomainsCard';
import { domainAccess } from '@/lib/serverDomainStatus';

// RCON configuration moved to the Players tab's RCON sub-section (it gates live
// player management), so the Network tab is now purely proxy/endpoint wiring.
export default function ServerNetworkPage() {
    const paramId = useRouteId('servers');
    const router = useRouter();
    const { servers, refreshServers, routingMode } = useAppData();
    const server = servers.find(s => s.id === Number(paramId));
    if (!server) return null;
    const access = domainAccess(server);
    return (
        <div className="space-y-6">
            {routingMode !== 'gateway' && access.read && (<div className="max-w-3xl mx-auto"><ServerDomainsCard key={server.id} serverId={String(server.id)} canWrite={access.write} /></div>)}
            <NetworkView
                server={server}
                allServers={servers}
                onServerSelect={(id) => router.push(`/servers/${id}`)}
                onRefreshServers={refreshServers}
            />
        </div>
    );
}
