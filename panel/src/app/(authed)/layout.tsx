"use client";

import React, { useEffect, useState } from 'react';
import { useRouter, usePathname } from 'next/navigation';
import { AppDataProvider, useAppData } from '@/lib/AppDataContext';
import brandMark from '@/app/icon.png';
import { logout, updateProfile as apiUpdateProfile } from '@/lib/api';
import { getSetupStatus } from '@/lib/api/setup';
import Navbar from '@/components/Navbar';
import { SidebarCollapseProvider, useSidebarCollapse } from '@/lib/SidebarCollapse';
import { ModrinthLibraryProvider } from '@/components/mods/ModrinthLibrary';
import { useLayout } from '@/lib/useBreakpoint';
import NotificationsDropdown from '@/components/NotificationsDropdown';
import UpdatesBell from '@/components/UpdatesBell';
import ProfilePopup from '@/components/ProfilePopup';
import MaintenanceBanner from '@/components/MaintenanceBanner';
import BillingBanner from '@/components/BillingBanner';
import StorageBanner from '@/components/StorageBanner';
import NodeConnectionBanner from '@/components/NodeConnectionBanner';
import { ConfirmDialogRoot } from '@/components/ui/ConfirmDialog';
import { ReauthDialogRoot } from '@/components/ui/ReauthDialog';
import { ToastRoot } from '@/components/ui/Toast';
import GuardedLink, { useGuardedNav } from '@/components/GuardedLink';
import Menu, { type MenuEntry } from '@/components/ui/Menu';
import EmptyState from '@/components/ui/EmptyState';
import UploadManagerWidget from '@/components/UploadManagerWidget';
import DownloadManagerWidget, { downloadStore } from '@/components/DownloadManagerWidget';
import { DownloadTrackerContext } from '@dylaris/ui-filebrowser';
import ModInstallWidget from '@/components/ModInstallWidget';
import BeamDownloadButton from '@/components/BeamDownloadButton';
import LegalFooter from '@/components/LegalFooter';
import { UnsavedChangesProvider } from '@/components/settings/UnsavedChanges';
import { UploadManagerProvider, UploadManagerBridge } from '@/lib/uploadManager';
import { ModInstallProvider } from '@/lib/modInstallManager';
import Link from 'next/link';
import { ChevronDown, UserCog, LogOut, Wrench, Key, KeyRound, Package, Store, ShieldCheck, CloudOff, HardDrive, MoreVertical, Clock } from 'lucide-react';
import { loadTimePrefs } from '@/lib/time';
import { Skeleton, SkeletonCircle, SkeletonText } from '@/components/Skeleton';
import { hasSession, purgeLegacyTokens } from '@/lib/api/sessionState';
import { followSignOuts, markSessionSeen } from '@/lib/api/session';
import { getProfile } from '@/lib/api/auth';
import { isWails } from '@/lib/adapters/wailsBeamAdapter';
import PlayerHead from '@/components/PlayerHead';
import { Popover, PopoverContent, PopoverTrigger } from '@ui-sdk/react/popover';


function AuthedShell({ children }: { children: React.ReactNode }) {
    const { user, ready, apiUnreachable, retryBoot, featureFlags, byonEnabled, routeOnlyEnabled, servers, refreshUser } = useAppData();
    const router = useRouter();
    const pathname = usePathname();
    // The unsaved-changes dialog for the profile menu's links. A menu item
    // cannot host a GuardedLink (the menu unmounts on select, taking the dialog
    // with it), so the guard lives here and the items call into it.
    const nav = useGuardedNav();

    const [showProfilePopup, setShowProfilePopup] = useState(false);

    // The popup shows the answer itself, field by field, and stays open: Core's
    // message says what happens next (a new address waits for its confirmation),
    // and closing on a fixed "Saved!" threw exactly that away.
    const handleProfileUpdate = async (data: any) => {
        const result = await apiUpdateProfile(data);
        if (result?.success) refreshUser();
        return result;
    };

    // The API never answered the boot call. This is NOT "still loading" and it
    // is NOT an expired session - the token is untouched and probably fine. It
    // used to be indistinguishable from both: the session was thrown away and
    // the user landed on a login form that could not work either.
    if (apiUnreachable) {
        return (
            <div className="flex h-screen items-center justify-center bg-(--base-00) px-6">
                <EmptyState
                    icon={<CloudOff size={20} />}
                    title="Can't reach the API"
                    description="Your session is still valid. The panel could not reach the Dylaris API, which usually means it is restarting or the network dropped."
                    action={<button onClick={retryBoot} className="btn btn-primary">Try again</button>}
                    className="max-w-md"
                />
            </div>
        );
    }

    if (!ready || !user) {
        return <AppShellSkeleton />;
    }

    // Owner-gating signal: there is no admin-level "owner" flag on the user
    // itself, so we derive it from server ownership (each owned server
    // carries role: 'owner') in addition to platform admins.
    // Access manages SERVER grants, so it stays tied to owning a server: with no
    // server there is nothing to share, and showing it would open an empty page.
    const canAccess = user.isAdmin || servers.some(s => s.role === 'owner');
    // The page behind this holds BOTH halves of "hardware you own": machines and
    // protected addresses. So it is offered whenever EITHER exists, not just
    // when BYON does - a route-only customer on a platform with BYON off would
    // otherwise have no way in at all, now that protected addresses no longer
    // hang off the account dropdown.
    //
    // Only where BYON or route-only is usable. External nodes cannot be given to
    // a user (owner_id is set only by BYON enroll), and an admin has them under
    // Infrastructure > External nodes and Settings > Nodes, so on a self-hosted
    // install without the gateway this entry was a duplicate of those. The page
    // itself still opens for an admin by URL.
    //
    // Shown without an entitlement as well, since the page explains what is
    // missing - more use than a link that silently is not there.
    // Each product's own usable flag: the page opened on gatewayEnabled alone
    // and then showed a route-only tab its gate had closed.
    const canSeeMyInfra = byonEnabled || routeOnlyEnabled;

    const avatar = (size: string) => (
        <span className={`${size} rounded-full bg-(--accent-dim) flex items-center justify-center text-(--accent-light) font-semibold text-xs overflow-hidden shrink-0`}>
            {user.minecraftUsername ? (
                <PlayerHead name={user.minecraftUsername} fallback={user.username.charAt(0).toUpperCase()} className="w-full h-full object-cover" />
            ) : (
                user.username.charAt(0).toUpperCase()
            )}
        </span>
    );

    // Entries beside the bell: where each goes, and whether this user sees it.
    const sections = [
        // The label is hidden below xl, so the name comes from aria-label.
        { show: canSeeMyInfra, href: '/nodes', label: 'My infrastructure', icon: <HardDrive size={16} />, active: pathname?.startsWith('/nodes') },
        { show: canAccess, href: '/access', label: 'Access', icon: <ShieldCheck size={16} />, active: pathname.startsWith('/access') },
        { show: user.isAdmin, href: '/settings', label: 'Settings', icon: <Wrench size={16} />, active: pathname.startsWith('/settings') },
    ];

    const accountLink = (href: string, label: string, icon: React.ReactNode): MenuEntry => ({
        label,
        icon,
        render: <Link href={href} onClick={e => nav.intercept(e, href)} />,
    });
    const profileItems: MenuEntry[] = [
        { label: 'Edit Profile', icon: <UserCog size={16} />, onSelect: () => setShowProfilePopup(true) },
        accountLink("/account/time", 'Time & date', <Clock size={16} />),
        accountLink("/account/api-keys", 'API Keys', <Key size={16} />),
        accountLink("/account/ssh-keys", 'SSH keys', <KeyRound size={16} />),
        accountLink("/account/modrinth", 'Modrinth', <Package size={16} />),
        // Unconditional, like API Keys beside it: with own storage off the page
        // still lists existing targets for removal and says why nothing new can
        // be added.
        accountLink("/account/storage", 'Storage', <HardDrive size={16} />),
        // Connect-store entry only on the hosted build.
        ...(featureFlags.store ? [accountLink("/account/store", 'Dylaris Store', <Store size={16} />)] : []),
        { type: 'separator' },
        { label: 'Logout', icon: <LogOut size={16} />, danger: true, onSelect: () => { logout(); router.push('/login'); } },
    ];

    return (
        <SidebarCollapseProvider>
        <ModrinthLibraryProvider>
        {/* The width floor is a deliberate one, not an oversight. Below it the
            file browser, the console, the routes table and the module strip stop
            being usable at any font size, and a horizontal scrollbar is the
            honest answer - better than a layout that renders and does not work.
            It also bounds the testing surface to two bands instead of a
            continuum. Phones are not a target: they get their own app. */}
        <div className="flex flex-col h-screen min-w-(--app-min-width) bg-(--base-00) text-(--base-09) font-body overflow-hidden">
            {/* Single host for confirmDialog(). Mounted here so every authed
                screen can ask without threading a node through its own JSX. */}
            <ConfirmDialogRoot />
            {/* Single host for reauthDialog(). The admin account actions ask
                for the operator's own password, so the prompt lives beside the
                confirmation rather than inside every screen that manages a
                user. */}
            <ReauthDialogRoot />
            {/* Single host for toast(). Same reason as the dialog above: there
                were 31 copies of this state and markup, with five shapes and
                dismiss timeouts from 2800ms to 4500ms, so the same action
                reported differently depending on which screen ran it. */}
            <ToastRoot />
            {nav.dialog}
            {/* Global maintenance banner. Renders nothing when off. */}
            <MaintenanceBanner />
            {/* Non-dismissible billing banner for past_due/suspended tenants. */}
            <BillingBanner />
            {/* Storage backend reachability. Renders nothing while both are ok. */}
            <StorageBanner />
            {/* A tenant's OWN machine has stopped answering. Renders nothing while
                they are all connected, and nothing at all without BYON. */}
            <NodeConnectionBanner />
            {/* Top Navbar. The branding block carries the SAME width as the
                sidebar underneath it - that is the only reason the two columns
                line up, so it collapses with it. */}
            <div className="relative z-30 shrink-0">
                <Navbar brand={<SidebarBrand />}>
                    <UtilityCluster>
                    <BeamDownloadButton />
                    <DownloadManagerWidget />
                    <UploadManagerWidget />
                    <ModInstallWidget />
                    {/* UpdatesBell is for everyone now: an admin sees the platform notes and
                        every component, a customer sees the customer notes and their own nodes. */}
                    <UpdatesBell />
                    <NotificationsDropdown />
                    {/* NotificationsDropdown self-gates: admins see both system checks and inbox;
                        regular users see only their inbox. */}
                    {sections.filter(x => x.show).map(x => (
                        <GuardedLink
                            key={x.href}
                            href={x.href}
                            aria-label={x.label}
                            title={x.label}
                            aria-current={x.active ? 'page' : undefined}
                            className="app-icon-btn"
                        >
                            {x.icon}
                            <span className="app-label hidden xl:block">{x.label}</span>
                        </GuardedLink>
                    ))}

                    {/* User Profile Dropdown */}
                    <Menu
                        align="end"
                        className="w-60"
                        heading={
                            <div className="flex items-center gap-3 px-2.5 pb-2.5 pt-2">
                                {avatar('size-9')}
                                <div className="min-w-0">
                                    <div className="srv-label">Signed in as</div>
                                    <div className="truncate text-sm font-medium leading-tight text-(--base-09)">{user.username}</div>
                                    {user.isAdmin && <span className="srv-chip mt-1 h-5 px-2 text-[11px]">Admin</span>}
                                </div>
                            </div>
                        }
                        trigger={
                            <button type="button" aria-label="Account menu" className="app-icon-btn ml-1 gap-2 pl-1 pr-2">
                                {avatar('size-7')}
                                <span className="app-label hidden xl:block max-w-[120px] truncate text-(--base-09)">{user.username}</span>
                                <ChevronDown size={14} className="text-(--base-06)" aria-hidden="true" />
                            </button>
                        }
                        items={profileItems}
                    />
                    </UtilityCluster>
                </Navbar>
            </div>

            {/* Main content */}
            <div className="flex flex-1 overflow-hidden relative">
                {children}
            </div>
            <LegalFooter className="shrink-0 bg-(--base-01)" />

            {showProfilePopup && (
                <ProfilePopup
                    currentUser={user}
                    onClose={() => setShowProfilePopup(false)}
                    onUpdate={handleProfileUpdate}
                    onMinecraftSaved={refreshUser}
                />
            )}
        </div>
        </ModrinthLibraryProvider>
        </SidebarCollapseProvider>
    );
}

// The navbar's right-hand actions: region chip, uploads, What's new,
// notifications, My nodes, and the profile menu.
//
// In the compact band they collapse behind one trigger instead of competing
// with the module strip for the same row - which is what used to push modules
// out of reach. The plan called for moving them into the sidebar rail; the
// sidebar lives in servers/layout.tsx, one tree over, so doing that literally
// would mean lifting the sidebar into this shell. One extra click here buys the
// same room for a fraction of the risk.
//
// Above compact this renders its children directly, so nothing changes at the
// widths where there was already room.
function UtilityCluster({ children }: { children: React.ReactNode }) {
    const { layout } = useLayout();
    const [open, setOpen] = useState(false);

    if (layout !== 'compact') return <div className="flex items-center gap-1">{children}</div>;

    return (
        <Popover open={open} onOpenChange={setOpen}>
            <PopoverTrigger
                render={<button type="button" />}
                aria-label="Account and notifications"
                title="Account and notifications"
                className="app-icon-btn"
            >
                <MoreVertical size={16} />
            </PopoverTrigger>
            <PopoverContent side="bottom" align="end" sideOffset={8} aria-label="Account and notifications"
                className="ui-popup app-cluster-popup z-40 p-2 flex flex-col items-stretch gap-1 min-w-56">
                {children}
            </PopoverContent>
        </Popover>
    );
}

// The branding block. Collapses to the mark alone at rail width so it keeps the
// sidebar's footprint exactly - a full-width logo over a 56px rail is the
// misalignment this avoids.
//
// The mark is the ICON, not the letter D. Cutting the wordmark down to its first
// glyph left a bare "D" in a display face built for a whole word: at 20px it
// reads as a stray character rather than as a logo, and it is the one thing on
// screen at rail width that is supposed to say which product this is. The icon
// already exists - it is the favicon - so the narrow state now shows the same
// mark the browser tab does.
// The block takes its width from the context the sidebar publishes, so the two
// can never disagree; the negative left margin cancels the navbar's own
// padding so the block starts at the window edge like the sidebar does.
function SidebarBrand() {
    const { collapsed, width } = useSidebarCollapse();
    return (
        <div className={`${width} shrink-0 self-stretch -ml-6 mr-2 flex items-center justify-center`}>
            {collapsed ? (
                /* A plain img rather than next/image: the panel is a static
                   export, so there is no optimizer behind it, and the
                   import already yields the hashed same-origin URL the CSP
                   allows. */
                <img src={brandMark.src} alt="Dylaris" width={24} height={24} className="block select-none" draggable={false} />
            ) : (
                <h1 className="text-2xl font-logo tracking-widest select-none">
                    <span className="text-(--accent-light)">D</span>
                    <span className="text-(--base-09)">ylaris</span>
                </h1>
            )}
        </div>
    );
}

export default function AuthedLayout({ children }: { children: React.ReactNode }) {
    const router = useRouter();
    const [tokenChecked, setTokenChecked] = useState(false);

    // Auth check. Setup status is read first because in
    // Fresh-Install or Lost-Admin mode the API would either 503 every
    // /api/* route or be missing admins — sending the user to /login in
    // either case is wrong. Token presence is the second gate; full token
    // validation happens in AppDataProvider via getProfile.
    useEffect(() => {
        let cancelled = false;
        (async () => {
            const status = await getSetupStatus();
            if (cancelled) return;
            if (status.mode !== 'complete') {
                router.replace('/setup');
                return;
            }
            // Anyone upgrading has a JWT sitting in localStorage from before the
            // session moved into a cookie. It authenticates nothing any more and
            // it is exactly the thing this change removes, so it goes on the
            // first authenticated load rather than sitting there until it
            // expires.
            purgeLegacyTokens();
            // The session cookie is HttpOnly, so this asks Core's readable
            // companion flag instead. A hint, never authorization: every request
            // below is still decided by the server.
            // Inside Beam the hint lives in the webview's cookie store, which a
            // fresh webview profile does not have, while the session itself sits
            // in the app and is still good. Ask Core once there before sending
            // a signed-in user to the login form.
            if (!hasSession() && !(isWails() && await getProfile().then(() => true, () => false))) {
                const target = window.location.pathname + window.location.search;
                sessionStorage.setItem('postLoginRedirect', target);
                router.push('/login');
                return;
            }
            markSessionSeen();
            // Before the shell renders, so the first paint already shows times
            // in the account's zone and clock. A failure keeps the browser's.
            await loadTimePrefs();
            if (cancelled) return;
            setTokenChecked(true);
        })();
        return () => { cancelled = true; };
    }, []); // eslint-disable-line react-hooks/exhaustive-deps

    // A tab nobody touches makes no requests, so it never heard that its
    // session ended: it sat there until a click failed. It now follows a
    // sign-out from another tab at once, and asks Core when it is looked at
    // again. A 401 there takes the normal way to /login; an unreachable Core
    // is not a sign-out and is ignored.
    useEffect(() => {
        if (!tokenChecked) return;
        const unfollow = followSignOuts();
        let lastCheck = Date.now();
        const check = () => {
            if (document.visibilityState !== 'visible' || Date.now() - lastCheck < 60_000) return;
            lastCheck = Date.now();
            getProfile().catch(() => {});
        };
        document.addEventListener('visibilitychange', check);
        window.addEventListener('focus', check);
        return () => {
            unfollow();
            document.removeEventListener('visibilitychange', check);
            window.removeEventListener('focus', check);
        };
    }, [tokenChecked]);

    const handleUnauthenticated = () => {
        localStorage.removeItem('token');
        localStorage.removeItem('authToken');
        const target = window.location.pathname + window.location.search;
        sessionStorage.setItem('postLoginRedirect', target);
        router.push('/login');
    };

    if (!tokenChecked) {
        // Skeleton chrome that mirrors the authed shell: a top navbar with
        // logo + utility cluster, then a flex row of left sidebar + main
        // content placeholder. Avoids the spinner→full-UI snap when the
        // setup/token check resolves.
        return (
            <AppShellSkeleton />
        );
    }

    return (
        <AppDataProvider onUnauthenticated={handleUnauthenticated}>
            <UnsavedChangesProvider>
                <UploadManagerProvider>
                    <UploadManagerBridge />
                    <ModInstallProvider>
                        {/* Downloads from the file browser go to the header list,
                            so they keep running across page changes. */}
                        <DownloadTrackerContext.Provider value={downloadStore}>
                            <AuthedShell>{children}</AuthedShell>
                        </DownloadTrackerContext.Provider>
                    </ModInstallProvider>
                </UploadManagerProvider>
            </UnsavedChangesProvider>
        </AppDataProvider>
    );
}

// Skeleton chrome that mirrors the authed shell: a top navbar with logo + utility
// cluster, then a flex row of left sidebar + main content placeholder. Shown for
// BOTH waits before the shell - the token check and the first data load - so the
// screen goes skeleton -> panel once instead of skeleton -> "Loading..." -> panel.
function AppShellSkeleton() {
    return (
        <div className="flex flex-col h-screen bg-(--base-00) text-(--base-09) font-body overflow-hidden">
            {/* Navbar placeholder */}
            <div className="shrink-0 h-14 bg-(--base-01) px-4 flex items-center justify-between">
                <div className="flex items-center gap-3">
                    <SkeletonCircle size="h-8 w-8" />
                    <SkeletonText width="w-28" className="h-4" />
                </div>
                <div className="flex items-center gap-3">
                    <Skeleton className="h-7 w-20 rounded-md" />
                    <SkeletonCircle size="h-8 w-8" />
                    <SkeletonCircle size="h-8 w-8" />
                    <div className="flex items-center gap-2 pl-3">
                        <SkeletonCircle size="h-8 w-8" />
                        <SkeletonText width="w-20" className="h-3 hidden md:block" />
                    </div>
                </div>
            </div>
            {/* Body: sidebar + main area */}
            <div className="flex flex-1 overflow-hidden">
                <aside className="w-60 shrink-0 bg-(--base-01) p-4 space-y-2">
                    {Array.from({ length: 6 }).map((_, i) => (
                        <Skeleton key={i} className="h-9 w-full rounded-md" />
                    ))}
                </aside>
                <main className="flex-1 app-page space-y-4">
                    <SkeletonText width="w-48" className="h-5" />
                    <SkeletonText width="w-72" className="h-3" />
                    <div className="grid grid-cols-2 md:grid-cols-4 gap-3 pt-2">
                        {Array.from({ length: 4 }).map((_, i) => (
                            <Skeleton key={i} className="h-20 w-full rounded-(--radius-lg)" />
                        ))}
                    </div>
                    <Skeleton className="h-64 w-full rounded-(--radius-lg) mt-4" />
                </main>
            </div>
        </div>
    );
}
