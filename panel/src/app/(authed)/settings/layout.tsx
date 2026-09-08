"use client";

import React, { useEffect, useState, useCallback } from 'react';
import Link from 'next/link';
import { usePathname, useRouter } from 'next/navigation';
import { useAppData } from '@/lib/AppDataContext';
import {
    useUnsavedChangesState,
    UnsavedDialog,
} from '@/components/settings/UnsavedChanges';
import SettingsSearch from '@/components/settings/SettingsSearch';
import { leavesPage } from '@/lib/navGuard';

interface SettingsTab {
    slug: string;
    label: string;
    // `always: true` bypasses the per-module toggle filter; module-gated tabs
    // carry a `module` name that must be enabled for them to appear.
    always: boolean;
    module?: string;
    // Hidden unless the hosted store is wired (STORE_URL + STORE_SHARED_KEY).
    requiresStore?: boolean;
    // Hidden unless the gateway is actually routing (routing_mode gateway|both).
    // For tabs that configure the gateway data plane and control nothing when it
    // is off: their settings are stored but no component reads them.
    requiresGateway?: boolean;
}

interface SettingsGroup {
    group: string;
    // Group-level feature gate: when set, the whole group (header + items) is
    // hidden unless the named feature flag is on.
    requiresByon?: boolean;
    tabs: SettingsTab[];
}

// Per-tab gate, for tabs inside an otherwise-visible group. requiresStore means
// the tab is meaningless without the hosted store: plans map onto Stripe
// products and billing acts on subscriptions, so on a self-host build they would
// be controls with nothing behind them.

// Grouped settings nav. Slugs/routes are unchanged from the previous flat list;
// only the presentation moved to a left vertical grouped sidebar. The BYON group
// is hidden unless feature_byon_enabled is on (selfhost installs never see it).
const TAB_GROUPS: SettingsGroup[] = [
    {
        group: 'General',
        tabs: [
            { slug: 'status', label: 'Status', always: true },
            { slug: 'modules', label: 'Modules', always: true },
            { slug: 'features', label: 'Features', always: true },
            { slug: 'maintenance', label: 'Maintenance', always: true },
            // Under General rather than under User settings: six places send
            // mail and only two of them are about accounts - a billing warning
            // is not a user setting.
            { slug: 'mailing', label: 'Mailing', always: true },
            { slug: 'database', label: 'Database', always: true },
        ],
    },
    {
        group: 'Access',
        tabs: [
            { slug: 'users', label: 'User settings', always: true },
            { slug: 'roles', label: 'Roles', always: true },
        ],
    },
    {
        group: 'Infrastructure',
        tabs: [
            { slug: 'regions', label: 'Regions', always: true },
            { slug: 'nodes', label: 'Nodes', always: true },
            // Gateway configures the player-traffic data plane: hoster domains,
            // route limits, reserved names. With routing on ip_port nothing reads
            // any of it. It stays reachable by URL so an operator can turn routing
            // ON from the routing-mode control it also owns.
            { slug: 'gateway', label: 'Gateway', always: true },
            // Warp is the overlay external nodes join. It belongs to the gateway
            // subsystem and is deployed with it, so without routing there is no
            // overlay for it to configure.
            { slug: 'warp', label: 'Warp', always: true, requiresGateway: true },
            { slug: 'beam', label: 'Beam', always: true },
        ],
    },
    {
        group: 'Storage',
        tabs: [
            { slug: 'core-storage', label: 'Core Storage', always: true },
            { slug: 'storage-connections', label: 'Storage Connections', always: true },
            { slug: 'storage-migration', label: 'Storage Migration', always: true },
            { slug: 'backups', label: 'Server Backups', always: true },
            { slug: 'platform-backups', label: 'Platform Backups', always: true },
        ],
    },
    {
        group: 'Servers & Content',
        tabs: [
            { slug: 'servers', label: 'Servers', always: true },
            { slug: 'modpacks', label: 'Modpacks', always: true },
            { slug: 'filemanager', label: 'File Manager', always: true },
        ],
    },
    {
        group: 'Support',
        tabs: [
            { slug: 'ticket-categories', label: 'Ticket Categories', always: true },
            { slug: 'canned-responses', label: 'Canned Responses', always: true },
            { slug: 'tickets', label: 'Ticket Settings', always: true },
            { slug: 'ticket-db', label: 'Ticket DB', always: true },
        ],
    },
    {
        group: 'BYON',
        requiresByon: true,
        tabs: [
            // Traffic metering is useful on any BYON install, store or not.
            { slug: 'usage', label: 'Usage', always: true },
            // What that metering is measured against. Beside Usage on purpose:
            // the number and the limit it is judged by are one question, and
            // they used to live in two different products.
            { slug: 'traffic-limits', label: 'Traffic limits', always: true },
            // Both act on Stripe subscriptions: hidden without the store.
            { slug: 'billing', label: 'Billing', always: true, requiresStore: true },
        ],
    },
];

// ---------------------------------------------------------------------------
// Inner layout - sits inside the provider so it can read context
// ---------------------------------------------------------------------------

interface PendingNav { href: string }

function SettingsLayoutInner({
    children,
    groups,
}: {
    children: React.ReactNode;
    groups: SettingsGroup[];
}) {
    const router = useRouter();
    const pathname = usePathname();
    const registration = useUnsavedChangesState();

    // Pending navigation while the confirm dialog is open.
    const [pendingNav, setPendingNav] = useState<PendingNav | null>(null);
    const [dialogSaving, setDialogSaving] = useState(false);

    const dirty = registration?.dirty ?? false;

    // Intercept tab clicks when dirty.
    const handleTabClick = useCallback(
        (e: React.MouseEvent<HTMLAnchorElement>, href: string) => {
            // Highlighting uses the prefix; leaving does not. /settings/tickets
            // is the lit-up nav item while /settings/tickets/deletion-log is
            // showing, and clicking it does move off that page.
            if (!dirty || !leavesPage(pathname, href)) return; // let it navigate normally
            e.preventDefault();
            setPendingNav({ href });
        },
        [dirty, pathname],
    );

    const closeDialog = () => {
        setPendingNav(null);
        setDialogSaving(false);
    };

    const handleDialogSave = async () => {
        if (!registration || !pendingNav) return;
        setDialogSaving(true);
        let ok = false;
        try {
            ok = await registration.save();
        } catch {
            ok = false;
        } finally {
            setDialogSaving(false);
        }
        // Same rule as the other two guards: a refused save keeps the operator
        // where their edits are.
        if (!ok) return;
        const href = pendingNav.href;
        closeDialog();
        router.replace(href);
    };

    const handleDialogDiscard = () => {
        if (!registration || !pendingNav) return;
        registration.discard();
        const href = pendingNav.href;
        closeDialog();
        router.replace(href);
    };

    return (
        <>
            <div className="flex-1 flex gap-6 min-h-0 overflow-hidden">
                {/* Left vertical grouped settings sidebar */}
                <nav className="w-52 shrink-0 overflow-y-auto border-r border-(--base-03) pr-3 flex flex-col gap-7 pt-1">
                    {/* Over the page list rather than in it: what people look
                        for are individual settings, and the page name is the
                        one thing they do not know. */}
                    <SettingsSearch />
                    {groups.map(group => (
                        <div key={group.group} className="flex flex-col gap-1">
                            <span className="mono-label px-3">{group.group}</span>
                            {group.tabs.map(tab => {
                                const href = `/settings/${tab.slug}`;
                                const isActive =
                                    pathname === href || pathname.startsWith(href + '/');
                                return (
                                    <Link
                                        key={tab.slug}
                                        href={href}
                                        replace
                                        onClick={e => handleTabClick(e, href)}
                                        className={`px-3 py-2 rounded-r-md text-sm font-medium transition-colors text-left border-l-[3px] ${
                                            isActive
                                                ? 'border-(--accent) bg-(--accent)/10 text-(--accent-light)'
                                                : 'border-transparent text-(--base-07) hover:text-(--base-09) hover:bg-(--base-03)'
                                        }`}
                                    >
                                        {tab.label}
                                    </Link>
                                );
                            })}
                        </div>
                    ))}
                </nav>

                {/* Content column. Saving belongs to the cards inside it:
                    see components/settings/SettingsCard.tsx for why a card is
                    the only place a save control lives. */}
                <div className="flex-1 min-w-0 flex flex-col min-h-0">
                    <div className="flex-1 overflow-y-auto">
                        {children}
                    </div>
                </div>
            </div>

            {/* Tab-switch confirm dialog */}
            {pendingNav && registration && (
                <UnsavedDialog
                    onSave={handleDialogSave}
                    onDiscard={handleDialogDiscard}
                    onCancel={closeDialog}
                    saving={dialogSaving}
                />
            )}
        </>
    );
}

// ---------------------------------------------------------------------------
// Root layout export
// ---------------------------------------------------------------------------

export default function SettingsLayout({ children }: { children: React.ReactNode }) {
    const router = useRouter();
    const { user, modules, ready, featureFlags, gatewayEnabled, byonEnabled } = useAppData();

    // Admin-only gate
    useEffect(() => {
        if (!ready) return;
        if (!user?.isAdmin) router.replace('/servers');
    }, [ready, user?.isAdmin, router]);

    if (!ready) return null;
    if (!user?.isAdmin) {
        return (
            <main className="flex-1 flex items-center justify-center text-(--error) font-semibold text-xl font-display">
                Access denied. Administrator rights required.
            </main>
        );
    }

    // Drop feature-gated groups (BYON only when the flag is on), then filter each
    // group's tabs by the per-module toggle, and finally drop groups left empty.
    const visibleGroups: SettingsGroup[] = TAB_GROUPS
        .filter(g => !g.requiresByon || byonEnabled)
        .map(g => ({
            ...g,
            tabs: g.tabs.filter(tab => {
                if (tab.requiresStore && !featureFlags.store) return false;
                if (tab.requiresGateway && !gatewayEnabled) return false;
                if (tab.always) return true;
                return modules.some(m => m.name === tab.module && m.isEnabled);
            }),
        }))
        .filter(g => g.tabs.length > 0);

    return (
        <main className="flex-1 flex flex-col overflow-hidden relative z-10 p-6">
            <h1 className="h-page mb-6">System Settings</h1>

            {/* UnsavedChangesProvider is mounted globally in (authed)/layout.tsx
                so its beforeunload + GuardedLink coverage extends beyond the
                Settings module too. We just consume it here. */}
            <SettingsLayoutInner groups={visibleGroups}>
                {children}
            </SettingsLayoutInner>
        </main>
    );
}
