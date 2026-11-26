"use client";

import React, { useEffect, useState } from 'react';
import { Wrench, Clock } from 'lucide-react';
import { getMaintenance, MaintenanceState } from '@/lib/api';

// MaintenanceBanner is mounted globally in the authenticated layout. It polls
// the public /api/maintenance endpoint every 30s as a safety net and also
// reacts to the `dylaris:maintenance.changed` window event fired by
// AppDataContext when the SSE stream signals an admin flip — so the banner
// appears within ~1s of the toggle instead of waiting up to 30s.
// Renders nothing when maintenance is off — zero footprint in steady state.
export default function MaintenanceBanner() {
    const [state, setState] = useState<MaintenanceState | null>(null);

    useEffect(() => {
        let cancelled = false;
        const fetchState = async () => {
            try {
                const res = await getMaintenance();
                if (!cancelled && res.success && res.state) {
                    setState(res.state);
                }
            } catch { /* network blip — keep last known state */ }
        };
        fetchState();
        const id = setInterval(fetchState, 30_000);
        const onPush = () => { fetchState(); };
        window.addEventListener('dylaris:maintenance.changed', onPush);
        return () => {
            cancelled = true;
            clearInterval(id);
            window.removeEventListener('dylaris:maintenance.changed', onPush);
        };
    }, []);

    if (!state || !state.active || state.blockLevel === 'off') return null;

    const countdown = state.expectedEnd ? formatCountdown(state.expectedEnd) : '';

    return (
        <div className="app-banner shrink-0" data-tone="warning">
            <div className="flex w-full flex-col sm:flex-row sm:items-center gap-2 sm:gap-4">
                <div className="flex items-start gap-2 min-w-0 flex-1">
                    <Wrench size={16} className="shrink-0 mt-0.5" />
                    <div className="min-w-0">
                        <p className="text-sm font-medium">{state.title || 'Maintenance in progress'}</p>
                        {state.message && (
                            <p className="text-xs text-(--base-07) mt-0.5">{state.message}</p>
                        )}
                    </div>
                </div>
                {countdown && (
                    <div className="srv-num flex items-center gap-1.5 text-xs shrink-0 sm:ml-auto">
                        <Clock size={12} />
                        <span>{countdown}</span>
                    </div>
                )}
            </div>
        </div>
    );
}

function formatCountdown(iso: string): string {
    try {
        const target = new Date(iso).getTime();
        const now = Date.now();
        const diff = target - now;
        if (diff <= 0) return 'Window passed';
        const mins = Math.floor(diff / 60000);
        const hours = Math.floor(mins / 60);
        if (hours >= 24) {
            const days = Math.floor(hours / 24);
            return `~${days}d ${hours % 24}h remaining`;
        }
        if (hours > 0) return `~${hours}h ${mins % 60}m remaining`;
        return `~${mins}m remaining`;
    } catch { return ''; }
}
