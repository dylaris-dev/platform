"use client";

import React from 'react';
import { Server as ServerIcon, PlusCircle } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';

export default function ServersIndexPage() {
    const { servers } = useAppData();

    return (
        <div className="flex-1 flex flex-col items-center justify-center text-(--base-06) p-6">
            <ServerIcon size={64} className="mb-4 opacity-40" />
            <p className="h-section text-(--base-07) mb-2">
                {servers.length === 0 ? 'No servers yet' : 'No server selected'}
            </p>
            <p className="text-sm text-(--base-06) max-w-md text-center mb-6">
                {servers.length === 0
                    ? 'Create your first server to get started.'
                    : 'Pick a server from the sidebar on the left to manage it.'}
            </p>
            {servers.length === 0 && (
                <p className="text-xs text-(--base-05) italic flex items-center gap-1.5">
                    <PlusCircle size={12} />
                    Use &ldquo;Create&rdquo; at the bottom of the sidebar, then &ldquo;New server&rdquo;.
                </p>
            )}
        </div>
    );
}
