"use client";

import React from 'react';
import { Server as ServerIcon } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import PageHeader from '@/components/ui/PageHeader';
import EmptyState from '@/components/ui/EmptyState';

export default function ServersIndexPage() {
    const { servers } = useAppData();
    const none = servers.length === 0;

    return (
        <div className="app-page flex-1 overflow-y-auto space-y-6 animate-content-in">
            <PageHeader
                title="Servers"
                description={none ? 'Everything you run shows up here and in the sidebar.' : `${servers.length} ${servers.length === 1 ? 'server' : 'servers'}. Pick one in the sidebar to manage it.`}
            />
            <EmptyState
                icon={<ServerIcon size={20} />}
                title={none ? 'No servers yet' : 'No server selected'}
                description={none
                    ? 'Open "Create" at the bottom of the sidebar to see what you can set up.'
                    : 'Pick a server from the sidebar on the left to manage it.'}
            />
        </div>
    );
}
