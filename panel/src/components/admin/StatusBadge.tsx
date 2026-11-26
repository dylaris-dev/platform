"use client";

import React from 'react';

// Every status a server can actually be reported in. The three below the
// blank line were producing the neutral fallback, which on the admin list -
// the one place an operator scans for trouble - rendered a server held for a
// full disk exactly like one the owner had simply stopped. The Sidebar and the
// server layout already treat disk_full as an error state; this matches them.
export const STATUS_CLASSES: Record<string, string> = {
    online: 'bg-(--success-ghost) text-(--success-light)',
    offline: 'bg-(--base-03) text-(--base-06)',
    stopped: 'bg-(--base-03) text-(--base-06)',
    installing: 'bg-(--warning-ghost) text-(--warning)',
    pending_setup: 'bg-(--base-03) text-(--base-06)',
    starting: 'bg-(--warning-ghost) text-(--warning)',
    stopping: 'bg-(--warning-ghost) text-(--warning)',
    suspended: 'bg-(--error-ghost) text-(--error-light)',

    disk_full: 'bg-(--error-ghost) text-(--error-light)',
    restarting: 'bg-(--warning-ghost) text-(--warning)',
    migrating: 'bg-(--warning-ghost) text-(--warning)',
};

export function StatusBadge({ status }: { status: string }) {
    const cls = STATUS_CLASSES[status] ?? 'bg-(--base-03) text-(--base-06)';
    return (
        <span className={`inline-flex h-6 items-center rounded-full px-2.5 text-xs ${cls}`}>
            {status.replace(/_/g, ' ')}
        </span>
    );
}
