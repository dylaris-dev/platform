"use client";

import { Lock } from 'lucide-react';
import EmptyState from '@/components/ui/EmptyState';

// The standard "feature paused" curtain every ticket page renders in place of
// its content when featureFlags.tickets is false. One wording across /tickets,
// /tickets/new, /tickets/inbox and /tickets/[id] so a refresh during a flip
// looks intentional rather than broken.
export default function TicketsDisabledBanner() {
    return (
        <EmptyState
            icon={<Lock size={20} />}
            title="Ticket system is disabled"
            description="The ticket system is disabled by the platform admin. Reach out to your admin if you need support."
            className="mx-auto mt-8 max-w-md"
        />
    );
}
