"use client";

import React, { useState, useEffect, useMemo, useRef } from 'react';
import {
    CreateRouteRequest,
    DomainCheckRequest,
    GatewayRoute,
    GatewayRouteOptions,
    HosterValidation,
    checkDomainAvailability,
    getGatewayRouteOptions,
} from '@/lib/api';
import { AlertCircle, Check, Globe, Loader2, Server, X } from 'lucide-react';
import { cnameTargetsFor } from '@/lib/cnameTargets';

// Availability state surfaced to the parent so it can disable submit when
// the picker is mid-check or has landed on a taken domain.
// 'own' = the domain is already routed to THIS server -- treated as
// non-blocking on submit (the parent skips the createServerRoute call).
export type DomainAvailability = 'idle' | 'checking' | 'available' | 'taken' | 'own';

interface Props {
    value: CreateRouteRequest;
    onChange: (next: CreateRouteRequest) => void;
    onAvailabilityChange?: (state: DomainAvailability) => void;
    // Routes already attached to the parent's server. When the typed
    // domain matches one of these we short-circuit the "taken" path
    // and show a friendly "you already have this one" indicator
    // instead -- the submit path treats it as a no-op for the route
    // create, but install can still proceed.
    existingRoutes?: GatewayRoute[];
    error?: string;
    portChildren?: React.ReactNode; // The port select rendered next to the domain
}

const VALIDATION_HINT: Record<HosterValidation, string> = {
    letters: 'letters only',
    alphanumeric: 'letters + numbers',
    dns: 'letters, numbers and -',
};

const VALIDATION_REGEX: Record<HosterValidation, RegExp> = {
    letters: /^[a-z]+$/,
    alphanumeric: /^[a-z0-9]+$/,
    dns: /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/,
};

// Up to apex + 2 subdomains (a.b.c.d = 4 labels max)
const CUSTOM_DOMAIN_REGEX = /^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$/;
function customDomainLabelsOk(d: string) {
    const labels = d.split('.').filter(Boolean);
    return labels.length >= 2 && labels.length <= 4;
}

/**
 * RouteDomainPicker is the user-facing domain entry for new routes.
 *
 * Renders one of three modes based on what the admin enabled:
 *   - hoster picker: subdomain text input + dropdown of base domains
 *   - custom domain: full-FQDN input + CNAME hint
 *   - legacy fallback: plain domain input (only when admin set up nothing)
 *
 * The component owns the mode toggle and writes the right shape into
 * `value` so the caller can post the request as-is.
 */
export default function RouteDomainPicker({ value, onChange, onAvailabilityChange, existingRoutes, error, portChildren }: Props) {
    const [opts, setOpts] = useState<GatewayRouteOptions | null>(null);
    const [mode, setMode] = useState<'hoster' | 'custom' | 'legacy'>('legacy');
    const [subdomain, setSubdomain] = useState('');
    const [hosterDomain, setHosterDomain] = useState('');
    const [customDomain, setCustomDomain] = useState('');

    // Live availability state. The server endpoint is cheap; we still debounce
    // ~600ms so a fast typist doesn't fire one request per keystroke. A request
    // counter guards against out-of-order responses overwriting a fresher one
    // (e.g. user types "play", check fires, user backspaces to "pla", second
    // check fires; if the first response lands last we'd otherwise show the
    // wrong result).
    const [availability, setAvailability] = useState<DomainAvailability>('idle');
    const [availabilityReason, setAvailabilityReason] = useState<string>('');
    const reqCounter = useRef(0);

    useEffect(() => {
        getGatewayRouteOptions().then(res => {
            setOpts(res);
            const hosters = res.hosterDomains || [];
            if (hosters.length > 0) {
                setMode('hoster');
                setHosterDomain(hosters[0].domain);
            } else if (res.customDomainsEnabled) {
                setMode('custom');
            } else {
                setMode('legacy');
            }
        });
    }, []);

    const activeHoster = useMemo(
        () => (opts?.hosterDomains || []).find(h => h.domain === hosterDomain),
        [opts, hosterDomain],
    );

    // Push state up whenever any of the inputs change.
    useEffect(() => {
        if (mode === 'hoster') {
            onChange({ ...value, subdomain, hosterDomain, customDomain: undefined, domain: undefined });
        } else if (mode === 'custom') {
            onChange({ ...value, customDomain, subdomain: undefined, hosterDomain: undefined, domain: undefined });
        } else {
            onChange({ ...value, domain: customDomain, subdomain: undefined, hosterDomain: undefined, customDomain: undefined });
        }
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [mode, subdomain, hosterDomain, customDomain]);

    // Effective domain the user is typing. Mirrors previewDomain below
    // and the parent's submit-path computation so all three agree on
    // the same string for own-route matching.
    const effectiveDomain = useMemo(() => {
        if (mode === 'hoster' && subdomain && hosterDomain) {
            return `${subdomain}.${hosterDomain}`.toLowerCase();
        }
        if ((mode === 'custom' || mode === 'legacy') && customDomain) {
            return customDomain.toLowerCase();
        }
        return '';
    }, [mode, subdomain, hosterDomain, customDomain]);

    // Debounced availability check. We only hit the API when the input passes
    // local format validation, so the user never sees a "checking..." spinner
    // for input we already know is malformed. If the domain is already
    // attached to this server we short-circuit to 'own' and skip the API
    // entirely -- spamming /check-domain for our own domain would be silly.
    useEffect(() => {
        let req: DomainCheckRequest | null = null;
        if (mode === 'hoster' && subdomain && hosterDomain && activeHoster) {
            const lower = subdomain.toLowerCase();
            if (VALIDATION_REGEX[activeHoster.validation].test(lower)) {
                req = { subdomain: lower, hosterDomain };
            }
        } else if (mode === 'custom' && customDomain) {
            const lower = customDomain.toLowerCase();
            if (CUSTOM_DOMAIN_REGEX.test(lower) && customDomainLabelsOk(lower)) {
                req = { customDomain: lower };
            }
        } else if (mode === 'legacy' && customDomain) {
            req = { domain: customDomain.toLowerCase() };
        }

        if (!req) {
            setAvailability('idle');
            setAvailabilityReason('');
            return;
        }

        // Own-route short-circuit. Done after the local format check so a
        // partial type ("p") doesn't accidentally match anything; only the
        // full constructed effectiveDomain compares.
        if (effectiveDomain && existingRoutes && existingRoutes.some(r => r.domain.toLowerCase() === effectiveDomain)) {
            setAvailability('own');
            setAvailabilityReason('');
            return;
        }

        setAvailability('checking');
        const myReq = ++reqCounter.current;
        const handle = setTimeout(async () => {
            try {
                const res = await checkDomainAvailability(req!);
                if (myReq !== reqCounter.current) return; // stale
                if (res.available) {
                    setAvailability('available');
                    setAvailabilityReason('');
                } else {
                    setAvailability('taken');
                    setAvailabilityReason(res.reason || 'already taken');
                }
            } catch {
                if (myReq !== reqCounter.current) return;
                // Network / 500 — don't block the user; treat as idle so the
                // server-side create is still the source of truth.
                setAvailability('idle');
                setAvailabilityReason('');
            }
        }, 600);

        return () => clearTimeout(handle);
    }, [mode, subdomain, hosterDomain, customDomain, activeHoster, effectiveDomain, existingRoutes]);

    useEffect(() => {
        onAvailabilityChange?.(availability);
    }, [availability, onAvailabilityChange]);

    if (!opts) {
        return <div className="h-10 bg-(--base-02) rounded-md animate-pulse" />;
    }

    const hosters = opts.hosterDomains || [];
    // One CNAME target per hoster base: the admin configures a single label and
    // the user picks the region they want to be routed through.
    const cnameTargets = cnameTargetsFor(opts.cnameTarget || '', hosters);
    const showHosterMode = hosters.length > 0;
    const showCustomMode = opts.customDomainsEnabled;

    // Live validation hint
    let hint: string | null = null;
    if (mode === 'hoster' && subdomain && activeHoster) {
        const ok = VALIDATION_REGEX[activeHoster.validation].test(subdomain.toLowerCase());
        if (!ok) hint = `Allowed: ${VALIDATION_HINT[activeHoster.validation]}`;
    } else if (mode === 'custom' && customDomain) {
        const lower = customDomain.toLowerCase();
        if (!CUSTOM_DOMAIN_REGEX.test(lower) || !customDomainLabelsOk(lower)) {
            hint = 'Format: yourdomain.com — up to 2 subdomain levels (e.g. play.mc.example.com)';
        } else if (hosters.some(h => lower === h.domain || lower.endsWith('.' + h.domain))) {
            hint = `${lower} is on a hoster domain — use the subdomain picker instead.`;
        }
    }

    // Compute the live preview domain from the current picker state.
    const noDomainConfigured = !showHosterMode && !showCustomMode;
    let previewDomain: string | null = null;
    if (!noDomainConfigured) {
        if (mode === 'hoster' && subdomain && hosterDomain) {
            previewDomain = `${subdomain}.${hosterDomain}`;
        } else if ((mode === 'custom' || mode === 'legacy') && customDomain) {
            previewDomain = customDomain;
        }
    }

    return (
        <div className="max-w-md space-y-2">
            {/* No-domain empty state */}
            {noDomainConfigured && (
                <div className="flex items-center gap-2 px-3 py-2.5 rounded-md bg-(--base-02) border border-(--base-03) text-sm text-(--base-05) cursor-not-allowed select-none">
                    <AlertCircle size={14} className="shrink-0 text-(--base-05)" />
                    <span>No domain configured yet — add one under Settings → Gateway.</span>
                </div>
            )}

            {/* Mode switch (only when both hoster and custom are available).
                One full-width control split down the middle rather than two
                small pills: these are the two kinds of address a route can have,
                and at pill size "Custom Domain" read as a button that does
                something rather than as the half you are not currently on. */}
            {!noDomainConfigured && showHosterMode && showCustomMode && (
                <div role="tablist" aria-label="Address type" className="grid grid-cols-2 gap-1 p-1 rounded-md bg-(--base-03) border border-(--base-04)">
                    {([
                        { id: 'hoster' as const, label: 'Subdomain', desc: 'On one of our domains', Icon: Server },
                        { id: 'custom' as const, label: 'Custom domain', desc: 'One you own, via CNAME', Icon: Globe },
                    ]).map(({ id, label, desc, Icon }) => {
                        const active = mode === id;
                        return (
                            <button
                                key={id}
                                type="button"
                                role="tab"
                                aria-selected={active}
                                onClick={() => setMode(id)}
                                className={`flex items-center gap-2 px-3 py-2 rounded-sm text-left transition-colors ${
                                    active
                                        ? 'bg-(--accent) text-white'
                                        : 'text-(--base-07) hover:text-(--base-09) hover:bg-(--base-04)/60'
                                }`}
                            >
                                <Icon size={15} className={`shrink-0 ${active ? 'text-white' : 'text-(--base-06)'}`} />
                                <span className="min-w-0">
                                    <span className="block text-xs font-medium leading-tight">{label}</span>
                                    <span className={`block text-[10px] leading-tight truncate ${active ? 'text-white/75' : 'text-(--base-06)'}`}>{desc}</span>
                                </span>
                            </button>
                        );
                    })}
                </div>
            )}

            {/* What the choice above actually decides. Both halves were already
                here and neither said what it was FOR: the region select reads as
                a list of domains, and the custom-domain tab is invisible to
                anyone who never presses it. */}
            {!noDomainConfigured && (
                <p className="text-xs text-(--base-06)">
                    {showHosterMode && (hosters.length > 1
                        ? 'The region is where your players enter our network, so it is what decides their ping — pick the one closest to them.'
                        : 'Players enter our network through the region this address belongs to; we route the traffic from there.')}
                    {showHosterMode && showCustomMode && ' '}
                    {showCustomMode && 'You can also use a domain you own: choose Custom domain and point a CNAME at us.'}
                </p>
            )}

            {/* Hoster mode */}
            {!noDomainConfigured && mode === 'hoster' && showHosterMode && (
                <div className="flex gap-2 items-stretch">
                    <input
                        type="text"
                        value={subdomain}
                        onChange={e => setSubdomain(e.target.value.toLowerCase())}
                        placeholder="play"
                        className="input-field text-sm w-28 min-w-0"
                    />
                    <span className="flex items-center text-(--base-06) text-sm font-mono px-1">.</span>
                    <select
                        value={hosterDomain}
                        onChange={e => setHosterDomain(e.target.value)}
                        aria-label="Region"
                        title="Region — where your players enter our network"
                        className="input-field text-sm w-44"
                    >
                        {hosters.map(h => (
                            <option key={h.domain} value={h.domain}>{h.domain}</option>
                        ))}
                    </select>
                    {portChildren}
                </div>
            )}

            {/* Custom mode */}
            {!noDomainConfigured && mode === 'custom' && showCustomMode && (
                <div className="space-y-2">
                    <div className="flex gap-2 items-stretch">
                        <div className="flex-1 flex items-center gap-2">
                            <Globe size={14} className="text-(--accent-light) shrink-0" />
                            <input
                                type="text"
                                value={customDomain}
                                onChange={e => setCustomDomain(e.target.value.toLowerCase())}
                                placeholder="play.example.com"
                                className="input-field flex-1 text-sm"
                            />
                        </div>
                        {portChildren}
                    </div>
                    {cnameTargets.length > 0 && (
                        <div className="flex items-start gap-2 p-2.5 rounded-md bg-(--accent)/5 border border-(--accent)/20 text-xs">
                            <AlertCircle size={12} className="text-(--accent-light) shrink-0 mt-0.5" />
                            <div className="text-(--base-07) flex flex-col gap-1.5">
                                {cnameTargets.length === 1 ? (
                                    <span>
                                        Set a CNAME record on your domain pointing to{' '}
                                        <code className="font-mono text-(--accent-light) bg-(--base-02) px-1.5 py-0.5 rounded">{cnameTargets[0]}</code>
                                        {' '}before saving — otherwise the route will not resolve.
                                    </span>
                                ) : (
                                    <>
                                        <span>
                                            Set a CNAME record on your domain before saving, pointing at the region you
                                            want to play in — otherwise the route will not resolve. Pick one:
                                        </span>
                                        <div className="flex flex-col gap-1">
                                            {cnameTargets.map(t => (
                                                <code key={t} className="font-mono text-(--accent-light) bg-(--base-02) px-1.5 py-0.5 rounded w-fit">{t}</code>
                                            ))}
                                        </div>
                                    </>
                                )}
                                <span>
                                    After saving you get a TXT record that proves the domain is yours. Add it within
                                    4 hours, or the route is removed.
                                </span>
                            </div>
                        </div>
                    )}
                </div>
            )}

            {/* Live preview */}
            {previewDomain && (
                <p className="text-xs text-(--base-06) font-mono">
                    Your domain will be: <span className="text-(--primary-light)">{previewDomain}</span>
                </p>
            )}

            {/* Availability indicator. Suppressed when a local format hint is
                already visible, since the API never gets called for invalid
                input and would just show a stale state. */}
            {!hint && availability === 'checking' && (
                <div className="flex items-center gap-1.5 text-xs text-(--base-06)">
                    <Loader2 size={12} className="animate-spin" />
                    Checking availability…
                </div>
            )}
            {!hint && availability === 'available' && (
                <div className="flex items-center gap-1.5 text-xs text-(--success-light)">
                    <Check size={12} />
                    Available
                </div>
            )}
            {!hint && availability === 'taken' && (
                <div className="flex items-center gap-1.5 text-xs text-(--error-light)">
                    <X size={12} />
                    {availabilityReason || 'Not available'}
                </div>
            )}
            {!hint && availability === 'own' && (
                <div className="flex items-center gap-1.5 text-xs text-(--accent-light)">
                    <Check size={12} />
                    You already have that one — install will skip re-creating it.
                </div>
            )}

            {hint && (
                <div className="flex items-center gap-1.5 text-xs text-(--warning-light)">
                    <AlertCircle size={12} />
                    {hint}
                </div>
            )}
            {error && (
                <div className="flex items-center gap-1.5 text-xs text-(--error-light)">
                    <AlertCircle size={12} />
                    {error}
                </div>
            )}
        </div>
    );
}
