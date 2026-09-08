"use client";

import React, { createContext, useContext, useState, useEffect, useCallback, ReactNode } from 'react';
import {
    getProfile, getModules, getServers, getFeatureSettings, getRoutingMode, getBeamSettings,
    AppModule, Server, User, RoutingMode, FileAccessMode, BeamSettings, isGatewayRouting, isByonUsable,
} from '@/lib/api';
import { listRegions, Region } from '@/lib/api/regions';
import { API_URL, GATE_TIMEOUT_MS, getAuthHeader } from '@/lib/api/core';
import { systemEvents } from '@/lib/systemEvents';
import { getSystemFeatures, FeatureFlags } from '@/lib/api/featureFlags';
import { getMyEntitlement, Entitlement } from '@/lib/api/entitlement';

interface CoreInfo {
    region: string;
    coreId: string;
    // The DNS suffix each proxied custom tab is served under, one host per
    // tab, and whether that is configured at all. Without it a proxied tab has
    // nowhere safe to be shown and the UI says so instead of framing a URL
    // that cannot load.
    tabProxyHostSuffix: string;
    tabProxyAvailable: boolean;
}

interface AppData {
    user: User | null;
    modules: AppModule[];
    servers: Server[];
    proxiesEnabled: boolean;
    routingMode: RoutingMode;
    fileAccessMode: FileAccessMode;
    beamSettings: BeamSettings | null;
    ready: boolean;
    // The API did not answer the boot profile call at all. Distinct from
    // `!ready`, which only means the boot data is still arriving, and from
    // an expired session, which ends on the login page.
    apiUnreachable: boolean;
    retryBoot: () => void;
    refreshUser: () => Promise<void>;
    refreshModules: () => Promise<void>;
    refreshServers: () => Promise<void>;
    refreshSettings: () => Promise<void>;
    gatewayEnabled: boolean;
    /** featureFlags.byon ANDed with live gateway routing: BYON cannot work without it. */
    byonEnabled: boolean;
    libraryEnabled: boolean;
    // platform-wide feature toggles. Loaded on boot from
    // /api/system/features; refreshed when features.changed SSE fires.
    // `modpacks` defaults to true on transport failure so the UI doesn't go
    // dark when the network blips.
    featureFlags: FeatureFlags;
    refreshFeatureFlags: () => Promise<void>;
    // regions + connected-Core. Loaded once at boot and cached so
    // child components can call useAppData() without each issuing their own
    // fetch. Refreshes happen lazily on visible state changes (Settings →
    // Regions tab triggers refresh after save).
    regions: Region[];
    coreInfo: CoreInfo | null;
    refreshRegions: () => Promise<void>;
    // What this user may USE (BYON / route-only), resolved server-side from
    // their plan plus any admin grant. null until the first fetch returns;
    // treat null as "not yet known" and NOT as "no", or the UI briefly tells an
    // entitled tenant they have nothing.
    entitlement: Entitlement | null;
    refreshEntitlement: () => Promise<void>;
}

const AppDataContext = createContext<AppData | null>(null);

export function useAppData(): AppData {
    const ctx = useContext(AppDataContext);
    if (!ctx) throw new Error('useAppData must be used within AppDataProvider');
    return ctx;
}

interface AppDataProviderProps {
    children: ReactNode;
    onUnauthenticated: () => void;
}

export function AppDataProvider({ children, onUnauthenticated }: AppDataProviderProps) {
    const [user, setUser] = useState<User | null>(null);
    const [modules, setModules] = useState<AppModule[]>([]);
    const [servers, setServers] = useState<Server[]>([]);
    const [proxiesEnabled, setProxiesEnabled] = useState(true);
    const [routingMode, setRoutingMode] = useState<RoutingMode>('ip_port');
    const [fileAccessMode, setFileAccessMode] = useState<FileAccessMode>('sftp');
    const [beamSettings, setBeamSettings] = useState<BeamSettings | null>(null);
    const [regions, setRegions] = useState<Region[]>([]);
    const [coreInfo, setCoreInfo] = useState<CoreInfo | null>(null);
    // modpacks defaults to true (legacy: ships enabled, admin opts out).
    // tickets defaults to false so the nav doesn't briefly flash a Tickets
    // module on first paint before /system/features comes back.
    // modpackAuthoring defaults to FALSE for the same reason, and because
    // guessing the wider of two audiences would flash user-facing authoring
    // controls at someone who is not allowed to use them.
    // modpackStorage defaults TRUE so the create button is not disabled during
    // the first paint of a working install; the flag arrives a moment later and
    // corrects it. Guessing the other way would flash a blocked button at
    // everyone on every load.
    const [featureFlags, setFeatureFlags] = useState<FeatureFlags>({ modpacks: true, modpackAuthoring: false, tickets: false, library: false, autoMove: false, byon: false, store: false, shareLinks: false, modpackStorage: true });
    const [entitlement, setEntitlement] = useState<Entitlement | null>(null);
    const [ready, setReady] = useState(false);
    const [apiUnreachable, setApiUnreachable] = useState(false);
    const [bootAttempt, setBootAttempt] = useState(0);

    const refreshUser = useCallback(async () => {
        let profile;
        try {
            profile = await getProfile();
        } catch {
            // Unreachable, not unauthenticated. Keep the session and the last
            // known user; a refresh is not the place to end someone's session.
            return;
        }
        if (profile) setUser(profile);
        else onUnauthenticated();
    }, [onUnauthenticated]);

    const refreshModules = useCallback(async () => {
        const res = await getModules();
        if (res.success && res.modules) setModules(res.modules);
    }, []);

    const refreshServers = useCallback(async () => {
        const res = await getServers();
        if (res.success && res.servers) setServers(res.servers);
    }, []);

    const refreshRegions = useCallback(async () => {
        const res = await listRegions();
        if (res.success && Array.isArray(res.regions)) setRegions(res.regions);
    }, []);

    const refreshFeatureFlags = useCallback(async () => {
        const res = await getSystemFeatures();
        if (res.success && res.features) setFeatureFlags(res.features);
    }, []);

    // Stays null on failure rather than falling back to an all-false object:
    // "not known yet" and "you have nothing" must not render the same, or a
    // network blip tells an entitled tenant their account was downgraded.
    const refreshEntitlement = useCallback(async () => {
        const res = await getMyEntitlement();
        if (!res.success) return;
        setEntitlement({
            byon: !!res.byon,
            routeOnly: !!res.routeOnly,
            source: res.source || 'none',
            planKind: res.planKind,
            grantKind: res.grantKind,
            grantExpiresAt: res.grantExpiresAt,
        });
    }, []);

    // /api/system/core-info — public on the auth surface, returns the region
    // and ID of whichever Core this request landed on. Used by the topbar
    // chip and any region-aware UX that wants to mark "you're talking to X".
    const refreshCoreInfo = useCallback(async () => {
        try {
            const res = await fetch(`${API_URL}/system/core-info`, { headers: getAuthHeader() });
            if (!res.ok) return;
            const data = await res.json();
            if (data.success) setCoreInfo({
                region: data.region,
                coreId: data.coreId,
                tabProxyHostSuffix: data.tabProxyHostSuffix || '',
                tabProxyAvailable: !!data.tabProxyAvailable,
            });
        } catch { /* network blip — keep last known state */ }
    }, []);

    const refreshSettings = useCallback(async () => {
        const [features, routing, beam] = await Promise.all([
            getFeatureSettings(),
            getRoutingMode(),
            getBeamSettings(),
        ]);
        if (features.success && features.settings) {
            setProxiesEnabled(features.settings.proxyEnabled);
        }
        if (routing.success) {
            setRoutingMode(routing.mode || 'ip_port');
            setFileAccessMode(routing.fileMode || 'sftp');
        }
        if (beam.success && beam.settings) setBeamSettings(beam.settings);
    }, []);

    // Initial load. `ready` gates the ENTIRE authed app - until it flips, the
    // layout renders a bare "Loading..." - so everything awaited here has to be
    // bounded and every failure has to end somewhere real.
    useEffect(() => {
        const init = async () => {
            let profile;
            try {
                profile = await getProfile();
            } catch {
                // The API could not be reached. Do NOT call onUnauthenticated:
                // it clears both tokens, and reproduced on the testbed that
                // meant a Core restart logged out every open tab holding a
                // perfectly valid token. Say so instead and offer a retry.
                setApiUnreachable(true);
                return;
            }
            if (!profile) { onUnauthenticated(); return; }
            setUser(profile);
            // allSettled, not all: refreshSettings awaits three calls without a
            // catch of its own, so one rejection used to reject init itself and
            // setReady(true) was never reached - the app sat in "Loading..."
            // forever over a single failed slice. Each refresher already leaves
            // its own slice untouched on failure.
            //
            // The deadline is for the other half of that: a slice that hangs
            // rather than fails is neither settled nor rejected, and fetch has
            // no default timeout. Rendering with one stale slice beats not
            // rendering at all; the SSE subscriptions refresh them anyway.
            await Promise.race([
                Promise.allSettled([refreshModules(), refreshServers(), refreshSettings(), refreshRegions(), refreshCoreInfo(), refreshFeatureFlags(), refreshEntitlement()]),
                new Promise(resolve => setTimeout(resolve, GATE_TIMEOUT_MS)),
            ]);
            setReady(true);
        };
        init();
    }, [bootAttempt]); // eslint-disable-line react-hooks/exhaustive-deps

    // subscribe to /api/system/events SSE for live config refresh.
    // Replaces the old 5s setInterval(refreshServers) poll: status flips and
    // CRUD mutations on any Core publish servers.changed / regions.changed /
    // modules.changed / features.changed / maintenance.changed, and each
    // listener re-fetches just its own slice. systemEvents reconnects on
    // its own with exponential backoff if the stream drops.
    useEffect(() => {
        if (!ready) return;
        systemEvents.start();
        const unsubs = [
            systemEvents.on('servers.changed', () => { refreshServers(); }),
            systemEvents.on('regions.changed', () => { refreshRegions(); }),
            systemEvents.on('modules.changed', () => { refreshModules(); }),
            systemEvents.on('features.changed', () => { refreshSettings(); refreshFeatureFlags(); refreshEntitlement(); }),
            // A grant/revoke publishes users.changed; without this the tenant's own
            // "+" menu stays stale until they reload.
            systemEvents.on('users.changed', () => { refreshEntitlement(); }),
            systemEvents.on('modpack_settings.changed', () => { refreshFeatureFlags(); }),
            // maintenance state is consumed by MaintenanceBanner via its own
            // fetcher; we re-broadcast through window event so it (and any
            // other isolated subscriber) can react without threading a
            // refresher through context.
            systemEvents.on('maintenance.changed', () => {
                if (typeof window !== 'undefined') {
                    window.dispatchEvent(new CustomEvent('dylaris:maintenance.changed'));
                }
            }),
        ];
        return () => {
            unsubs.forEach(u => u());
            systemEvents.stop();
        };
    }, [ready, refreshServers, refreshRegions, refreshModules, refreshSettings, refreshFeatureFlags]);

    // The routing mode is the single source of truth for "is the gateway
    // routing traffic": Core gates every gateway write on exactly this
    // (handlers.gatewayEnabled reads routing_mode), so the panel must derive
    // it the same way. Deriving it from a separate feature flag let the two
    // sides disagree in both directions — a visible routes UI whose writes
    // 503, or a live gateway with its whole UI hidden.
    const gatewayEnabled = isGatewayRouting(routingMode);

    // BYON is tenancy over the gateway, so the raw admin flag is intent, not
    // capability: an external node FORCES gateway routing and beam locally
    // (NODE_EXTERNAL), so with routing on ip_port there is no way for a tenant
    // node to reach anything. The flag alone therefore let an operator with no
    // gateway switch on a whole tenant surface that cannot work - enrolment
    // screens for machines that can never join.
    //
    // Same shape as autoMove, whose flag is documented as "the panel ANDs this
    // with the live routing mode". featureFlags.byon stays the raw value so the
    // Features toggle keeps rendering the operator's actual setting.
    const byonEnabled = isByonUsable(featureFlags.byon, routingMode);

    // The FLAG, not the module row. The row hides a navbar entry; the flag is
    // what Core actually gates the library routes on, so a screen that offers
    // library files has to ask the same question Core will answer.
    const libraryEnabled = featureFlags.library;

    // Retry clears the error first, or a second failure would look like a
    // button that does nothing.
    const retryBoot = useCallback(() => {
        setApiUnreachable(false);
        setBootAttempt(n => n + 1);
    }, []);

    const value: AppData = {
        user, modules, servers,
        proxiesEnabled, routingMode, fileAccessMode, beamSettings,
        ready,
        apiUnreachable,
        retryBoot,
        refreshUser, refreshModules, refreshServers, refreshSettings,
        gatewayEnabled, byonEnabled, libraryEnabled,
        featureFlags, refreshFeatureFlags,
        regions, coreInfo, refreshRegions,
        entitlement, refreshEntitlement,
    };

    return <AppDataContext.Provider value={value}>{children}</AppDataContext.Provider>;
}
