"use client";

import React, { useEffect, useState } from 'react';
import { useRouter, usePathname } from 'next/navigation';
import { AppDataProvider, useAppData } from '@/lib/AppDataContext';
import brandMark from '@/app/icon.png';
import { logout, updateProfile as apiUpdateProfile } from '@/lib/api';
import { getSetupStatus } from '@/lib/api/setup';
import Navbar from '@/components/Navbar';
import { SidebarCollapseProvider, useSidebarCollapse } from '@/lib/SidebarCollapse';
import { useLayout } from '@/lib/useBreakpoint';
import NotificationsDropdown from '@/components/NotificationsDropdown';
import UpdatesBell from '@/components/UpdatesBell';
import ProfilePopup from '@/components/ProfilePopup';
import MaintenanceBanner from '@/components/MaintenanceBanner';
import BillingBanner from '@/components/BillingBanner';
import StorageBanner from '@/components/StorageBanner';
import NodeConnectionBanner from '@/components/NodeConnectionBanner';
import { ConfirmDialogRoot } from '@/components/ui/ConfirmDialog';
import { ToastRoot } from '@/components/ui/Toast';
import GuardedLink from '@/components/GuardedLink';
import UploadManagerWidget from '@/components/UploadManagerWidget';
import BeamDownloadButton from '@/components/BeamDownloadButton';
import { UnsavedChangesProvider } from '@/components/settings/UnsavedChanges';
import { UploadManagerProvider, UploadManagerBridge } from '@/lib/uploadManager';
import { ChevronDown, UserCog, LogOut, Wrench, Key, Package, Store, ShieldCheck, CloudOff, HardDrive, MoreVertical } from 'lucide-react';
import { Skeleton, SkeletonCircle, SkeletonText } from '@/components/Skeleton';
import { hasSession, purgeLegacyTokens } from '@/lib/api/sessionState';

// The player head shown beside the username. Encoded because the name is stored
// user input: without it a value carrying a slash would address a different
// path on the avatar host.
function avatarURL(minecraftUsername: string): string {
    return `https://cravatar.eu/helmavatar/${encodeURIComponent(minecraftUsername)}/64.png`;
}

function AuthedShell({ children }: { children: React.ReactNode }) {
    const { user, ready, apiUnreachable, retryBoot, featureFlags, gatewayEnabled, servers } = useAppData();
    const router = useRouter();
    const pathname = usePathname();

    const [isProfileDropdownOpen, setIsProfileDropdownOpen] = useState(false);
    const [showProfilePopup, setShowProfilePopup] = useState(false);
    const [popupError, setPopupError] = useState('');
    const [popupSuccess, setPopupSuccess] = useState('');

    // Click outside closes the dropdown
    useEffect(() => {
        const handleClickOutside = (e: MouseEvent) => {
            const target = e.target as HTMLElement;
            if (!target.closest('.profile-dropdown-container')) setIsProfileDropdownOpen(false);
        };
        document.addEventListener('click', handleClickOutside);
        return () => document.removeEventListener('click', handleClickOutside);
    }, []);

    const handleProfileUpdate = async (data: any) => {
        setPopupError(''); setPopupSuccess('');
        if (data.newPassword === 'passwords-do-not-match') { setPopupError('Passwords do not match.'); return; }
        const result = await apiUpdateProfile(data);
        if (result.success) {
            setPopupSuccess('Saved!');
            setTimeout(() => { setShowProfilePopup(false); setPopupSuccess(''); }, 1500);
        } else {
            setPopupError(result.message || 'An error occurred.');
        }
    };

    // The API never answered the boot call. This is NOT "still loading" and it
    // is NOT an expired session - the token is untouched and probably fine. It
    // used to be indistinguishable from both: the session was thrown away and
    // the user landed on a login form that could not work either.
    if (apiUnreachable) {
        return (
            <div className="flex h-screen items-center justify-center bg-(--base-00) px-6">
                <div className="max-w-md text-center">
                    <CloudOff size={32} className="mx-auto mb-4 text-(--base-06)" aria-hidden="true" />
                    <h1 className="text-lg font-medium text-(--base-09) mb-2">Can&apos;t reach the API</h1>
                    <p className="text-sm text-(--base-07) mb-6">
                        Your session is still valid. The panel could not reach the Dylaris
                        API, which usually means it is restarting or the network dropped.
                    </p>
                    <button onClick={retryBoot} className="btn btn-primary">Try again</button>
                </div>
            </div>
        );
    }

    if (!ready || !user) {
        return (
            <div className="flex h-screen items-center justify-center bg-(--base-00) text-(--base-07)">
                Loading...
            </div>
        );
    }

    const settingsActive = pathname.startsWith('/settings');
    const accessActive = pathname.startsWith('/access');
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
    // Admins ALWAYS, and not only for that reason: the page's third tab is the
    // operator's own external machines, which exist whether or not either
    // feature is on. Gating an admin on featureFlags.byon || gatewayEnabled left
    // that tab reachable only by typing the URL.
    //
    // Shown without an entitlement as well, since the page explains what is
    // missing - more use than a link that silently is not there.
    const canSeeMyInfra = user.isAdmin || featureFlags.byon || gatewayEnabled;

    return (
        <SidebarCollapseProvider>
        {/* min-w-[1024px] is a deliberate floor, not an oversight. Below it the
            file browser, the console, the routes table and the module strip stop
            being usable at any font size, and a horizontal scrollbar is the
            honest answer - better than a layout that renders and does not work.
            It also bounds the testing surface to two bands instead of a
            continuum. Phones are not a target: they get their own app. */}
        <div className="flex flex-col h-screen min-w-[1024px] bg-(--base-00) text-(--base-09) font-body overflow-hidden">
            {/* Single host for confirmDialog(). Mounted here so every authed
                screen can ask without threading a node through its own JSX. */}
            <ConfirmDialogRoot />
            {/* Single host for toast(). Same reason as the dialog above: there
                were 31 copies of this state and markup, with five shapes and
                dismiss timeouts from 2800ms to 4500ms, so the same action
                reported differently depending on which screen ran it. */}
            <ToastRoot />
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
                    <UploadManagerWidget />
                    {/* UpdatesBell is for everyone now: an admin sees the platform notes and
                        every component, a customer sees the customer notes and their own nodes. */}
                    <UpdatesBell />
                    <NotificationsDropdown />
                    {/* NotificationsDropdown self-gates: admins see both system checks and inbox;
                        regular users see only their inbox. */}
                    {canSeeMyInfra && (
                        <GuardedLink
                            href="/nodes"
                            className={`flex items-center space-x-2 px-3 py-1.5 rounded-md transition-colors font-medium border mr-2 ${
                                pathname?.startsWith('/nodes')
                                    ? 'bg-(--accent-ghost) text-(--accent-light) border-(--accent-border)'
                                    : 'text-(--base-07) hover:bg-(--base-04)/50 hover:text-(--base-09) border-transparent'
                            }`}
                        >
                            <HardDrive size={20} />
                            <span className="text-sm hidden md:block">My infrastructure</span>
                        </GuardedLink>
                    )}

                    {canAccess && (
                        <GuardedLink
                            href="/access"
                            className={`flex items-center space-x-2 px-3 py-1.5 rounded-md transition-colors font-medium border mr-2 ${
                                accessActive
                                    ? 'bg-(--accent-ghost) text-(--accent-light) border-(--accent-border)'
                                    : 'text-(--base-07) hover:bg-(--base-04)/50 hover:text-(--base-09) border-transparent'
                            }`}
                        >
                            <ShieldCheck size={20} />
                            <span className="text-sm hidden md:block">Access</span>
                        </GuardedLink>
                    )}

                    {user.isAdmin && (
                        <GuardedLink
                            href="/settings"
                            className={`flex items-center space-x-2 px-3 py-1.5 rounded-md transition-colors font-medium border mr-2 ${
                                settingsActive
                                    ? 'bg-(--accent-ghost) text-(--accent-light) border-(--accent-border)'
                                    : 'text-(--base-07) hover:bg-(--base-04)/50 hover:text-(--base-09) border-transparent'
                            }`}
                        >
                            <Wrench size={20} />
                            <span className="text-sm hidden md:block">Settings</span>
                        </GuardedLink>
                    )}

                    {/* User Profile Dropdown */}
                    <div className="relative profile-dropdown-container border-l border-(--base-03) pl-3">
                        <button
                            onClick={() => setIsProfileDropdownOpen(!isProfileDropdownOpen)}
                            className={`flex items-center gap-2.5 pl-1.5 pr-2.5 py-1.5 rounded-md transition-colors border border-transparent ${isProfileDropdownOpen ? 'bg-(--base-03)' : 'hover:bg-(--base-03)'}`}
                        >
                            <div className="w-8 h-8 rounded-full bg-(--accent-dim) flex items-center justify-center text-(--accent-light) font-semibold text-sm overflow-hidden border border-(--base-04) shrink-0">
                                {user.minecraftUsername ? (
                                    // 64, never 32: cravatar treats 32 as its default size and answers
                                    // that one request with a 308 to a plain-http URL, which the browser
                                    // then refuses as mixed content. So the head rendered everywhere the
                                    // panel asked for any other size and broke only here. One size for
                                    // both avatars, downscaled by CSS, which is sharper on HiDPI anyway.
                                    <img src={avatarURL(user.minecraftUsername)} alt="" className="w-full h-full object-cover" />
                                ) : (
                                    user.username.charAt(0).toUpperCase()
                                )}
                            </div>
                            <span className="font-medium hidden md:block text-sm max-w-[120px] truncate text-(--base-09)">{user.username}</span>
                            <ChevronDown size={18} className="text-(--base-06) transition-transform duration-200 shrink-0" style={{ transform: isProfileDropdownOpen ? 'rotate(180deg)' : 'rotate(0deg)' }} />
                        </button>

                        {isProfileDropdownOpen && (
                            <div className="dropdown-menu right-0 mt-3 w-60 animate-fade-in origin-top-right">
                                <div className="flex items-center gap-3 px-3 py-3 border-b border-(--base-03) mb-1.5">
                                    <div className="w-9 h-9 rounded-full bg-(--accent-dim) flex items-center justify-center text-(--accent-light) font-semibold text-sm overflow-hidden border border-(--base-04) shrink-0">
                                        {user.minecraftUsername ? (
                                            <img src={avatarURL(user.minecraftUsername)} alt="" className="w-full h-full object-cover" />
                                        ) : (
                                            user.username.charAt(0).toUpperCase()
                                        )}
                                    </div>
                                    <div className="min-w-0">
                                        <div className="dropdown-label">Signed in as</div>
                                        <div className="font-medium truncate text-(--base-09) leading-tight">{user.username}</div>
                                        {user.isAdmin && (
                                            <div className="mt-1"><span className="badge badge-accent">Admin</span></div>
                                        )}
                                    </div>
                                </div>
                                <button onClick={() => { setIsProfileDropdownOpen(false); setShowProfilePopup(true); }} className="dropdown-item">
                                    <UserCog size={20} className="mr-3" /> Edit Profile
                                </button>
                                <GuardedLink
                                    href="/account/api-keys"
                                    className="dropdown-item"
                                >
                                    <Key size={20} className="mr-3" /> API Keys
                                </GuardedLink>
                                <GuardedLink
                                    href="/account/modrinth"
                                    className="dropdown-item"
                                >
                                    <Package size={20} className="mr-3" /> Modrinth
                                </GuardedLink>
                                {/* Unconditional, like API Keys beside it: the route is
                                    capability-gated at Core and the page says so plainly
                                    when the grant is missing. The panel holds no list of
                                    the caller's own capabilities to hide it by. */}
                                <GuardedLink
                                    href="/account/backup-storage"
                                    className="dropdown-item"
                                >
                                    <HardDrive size={20} className="mr-3" /> Backup storage
                                </GuardedLink>
                                {/* Connect-store entry only on the hosted build. */}
                                {featureFlags.store && (
                                    <GuardedLink
                                        href="/account/store"
                                        className="dropdown-item"
                                    >
                                        <Store size={20} className="mr-3" /> Dylaris Store
                                    </GuardedLink>
                                )}
                                <button onClick={() => { setIsProfileDropdownOpen(false); logout(); router.push('/login'); }} className="dropdown-item text-(--error) hover:bg-(--error-ghost) mt-1">
                                    <LogOut size={20} className="mr-3" /> Logout
                                </button>
                            </div>
                        )}
                    </div>
                    </UtilityCluster>
                </Navbar>
            </div>

            {/* Main content */}
            <div className="flex flex-1 overflow-hidden relative">
                {children}
            </div>

            {showProfilePopup && (
                <ProfilePopup
                    currentUser={user}
                    onClose={() => setShowProfilePopup(false)}
                    onUpdate={handleProfileUpdate}
                    error={popupError}
                    success={popupSuccess}
                />
            )}
        </div>
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

    useEffect(() => {
        if (!open) return;
        const close = (e: MouseEvent) => {
            if (!(e.target as HTMLElement).closest('.utility-cluster')) setOpen(false);
        };
        document.addEventListener('mousedown', close);
        return () => document.removeEventListener('mousedown', close);
    }, [open]);

    if (layout !== 'compact') return <>{children}</>;

    return (
        <div className="relative utility-cluster">
            <button
                type="button"
                onClick={() => setOpen(o => !o)}
                aria-haspopup="menu"
                aria-expanded={open}
                aria-label="Account and notifications"
                title="Account and notifications"
                className="p-1.5 rounded-md text-(--base-07) hover:bg-(--base-04)/50 hover:text-(--base-09) transition-colors"
            >
                <MoreVertical size={20} />
            </button>
            {open && (
                <div className="absolute right-0 top-full mt-2 z-40 rounded-md border border-(--base-03) bg-(--base-01) shadow-lg p-3 flex flex-col items-stretch gap-2 min-w-56">
                    {children}
                </div>
            )}
        </div>
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
// Its divider has to land exactly on the sidebar's own edge, in BOTH states.
//
// Two things stopped it. The block took its width from a hard-coded class per
// branch instead of the one the context publishes for exactly this - which is
// how the two could disagree at all. And it sat INSIDE the navbar's px-6, so its
// border fell 24px to the right of the sidebar edge underneath it, while the
// sidebar starts at 0. The negative left margin cancels that padding; the
// negative vertical margins plus self-stretch take the divider the full height
// of the bar, which items-center otherwise crops to the height of the logo.
function SidebarBrand() {
    const { collapsed, width } = useSidebarCollapse();
    return (
        <div
            className={`${width} shrink-0 self-stretch -my-2.5 -ml-6 mr-6 flex items-center justify-center border-r border-(--base-03)`}
        >
            {collapsed ? (
                <div className="p-1 rounded-md bg-(--accent-dim) border border-(--accent-border) inline-flex items-center">
                    {/* A plain img rather than next/image: the panel is a static
                        export, so there is no optimizer behind it, and the
                        import already yields the hashed same-origin URL the CSP
                        allows. */}
                    <img src={brandMark.src} alt="Dylaris" width={22} height={22} className="block select-none" draggable={false} />
                </div>
            ) : (
                <div className="px-3.5 py-1 rounded-md bg-(--accent-dim) border border-(--accent-border) inline-flex items-center">
                    <h1 className="text-2xl font-logo tracking-widest select-none">
                        <span className="text-(--accent-light)">D</span>
                        <span className="text-(--base-09)">ylaris</span>
                    </h1>
                </div>
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
            if (!hasSession()) {
                const target = window.location.pathname + window.location.search;
                sessionStorage.setItem('postLoginRedirect', target);
                router.push('/login');
                return;
            }
            setTokenChecked(true);
        })();
        return () => { cancelled = true; };
    }, []); // eslint-disable-line react-hooks/exhaustive-deps

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
            <div className="flex flex-col h-screen bg-(--base-00) text-(--base-09) font-body overflow-hidden">
                {/* Navbar placeholder */}
                <div className="shrink-0 h-14 border-b border-(--base-03) px-4 flex items-center justify-between">
                    <div className="flex items-center gap-3">
                        <SkeletonCircle size="h-8 w-8" />
                        <SkeletonText width="w-28" className="h-4" />
                    </div>
                    <div className="flex items-center gap-3">
                        <Skeleton className="h-7 w-20 rounded-md" />
                        <SkeletonCircle size="h-8 w-8" />
                        <SkeletonCircle size="h-8 w-8" />
                        <div className="flex items-center gap-2 pl-3 border-l border-(--base-03)">
                            <SkeletonCircle size="h-8 w-8" />
                            <SkeletonText width="w-20" className="h-3 hidden md:block" />
                        </div>
                    </div>
                </div>
                {/* Body: sidebar + main area */}
                <div className="flex flex-1 overflow-hidden">
                    <aside className="w-60 shrink-0 border-r border-(--base-03) p-4 space-y-2">
                        {Array.from({ length: 6 }).map((_, i) => (
                            <Skeleton key={i} className="h-9 w-full rounded-md" />
                        ))}
                    </aside>
                    <main className="flex-1 p-6 space-y-4">
                        <SkeletonText width="w-48" className="h-5" />
                        <SkeletonText width="w-72" className="h-3" />
                        <div className="grid grid-cols-2 md:grid-cols-4 gap-3 pt-2">
                            {Array.from({ length: 4 }).map((_, i) => (
                                <Skeleton key={i} className="h-20 w-full rounded-(--radius-md)" />
                            ))}
                        </div>
                        <Skeleton className="h-64 w-full rounded-(--radius-md) mt-4" />
                    </main>
                </div>
            </div>
        );
    }

    return (
        <AppDataProvider onUnauthenticated={handleUnauthenticated}>
            <UnsavedChangesProvider>
                <UploadManagerProvider>
                    <UploadManagerBridge />
                    <AuthedShell>{children}</AuthedShell>
                </UploadManagerProvider>
            </UnsavedChangesProvider>
        </AppDataProvider>
    );
}
