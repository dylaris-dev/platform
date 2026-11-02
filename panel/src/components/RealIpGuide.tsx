"use client";

import { ChevronRight, ExternalLink, Info } from 'lucide-react';
import { PROXY_SETUP_ROWS, PROXY_TRUSTED_RANGES, modLabel } from '@/lib/proxyProtocolGuide';

/**
 * Why the real player IP matters and what each software needs for it. A
 * disclosure, not a page: the one-line why is always visible, the setup opens
 * on demand. The row for the server's own software is marked and listed first.
 */
export default function RealIpGuide({ software, routeOnly = false }: {
    /** The server's installerType; unknown or empty marks no row. */
    software?: string;
    /** Route-only has an on/off switch instead of the Auto/Always/Off modes. */
    routeOnly?: boolean;
}) {
    const own = (software || '').trim().toLowerCase();
    const rows = [...PROXY_SETUP_ROWS].sort((a, b) => Number(b.software.includes(own)) - Number(a.software.includes(own)));

    return (
        <div className="mt-3 space-y-2">
            <div className="alert alert-info text-xs">
                <Info size={14} className="shrink-0 mt-0.5" aria-hidden="true" />
                <span>
                    Without it, every player reaches the server from the gateway&apos;s address. IP bans, alt
                    detection by IP and IP logs then see one address for everyone.
                </span>
            </div>
            <details className="group text-xs">
                <summary className="cursor-pointer select-none list-none inline-flex items-center gap-1 text-(--base-07) hover:text-(--base-09) focus-visible:outline-2 focus-visible:outline-(--accent) rounded-sm">
                    <ChevronRight size={14} className="transition-transform group-open:rotate-90 motion-reduce:transition-none" aria-hidden="true" />
                    How to set it up
                </summary>
                <div className="mt-2 space-y-3 text-(--base-06) leading-relaxed">
                    {routeOnly ? (
                        <p>
                            Set your server up first, restart it, then turn the switch on. With the switch on and a
                            server that is not set up, nobody can join; the other way round the same.
                        </p>
                    ) : (
                        <ul className="space-y-1">
                            <li><span className="text-(--base-09) font-medium">Auto</span> (recommended): the gateway sends the IP only once the server shows it expects it. Set the server up, restart, and it switches on within a minute. Nobody is locked out.</li>
                            <li><span className="text-(--base-09) font-medium">Always</span>: sent on every connection. If the server is not set up for it, nobody can join.</li>
                            <li><span className="text-(--base-09) font-medium">Off</span>: never sent.</li>
                        </ul>
                    )}

                    <ul className="divide-y divide-(--base-03) border border-(--base-03) rounded-md">
                        {rows.map(r => {
                            const mine = r.software.includes(own);
                            return (
                                <li key={r.label} className={`px-3 py-2 ${mine ? 'bg-(--accent-ghost)' : ''}`}>
                                    <div className="flex items-center gap-2">
                                        <span className="text-(--base-09) font-medium">{r.label}</span>
                                        {mine && <span className="mono-label text-(--accent-light)">This server</span>}
                                    </div>
                                    <div className="mt-0.5">{r.text}</div>
                                    {r.file && r.line && (
                                        <div className="mt-1">
                                            In {r.file}: <code className="font-mono text-(--base-09)">{r.line}</code>
                                        </div>
                                    )}
                                    {r.mods && (
                                        <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1">
                                            {r.mods.map(m => (
                                                <a key={m.name} href={m.url} target="_blank" rel="noopener noreferrer"
                                                    className="inline-flex items-center gap-1 text-(--accent-light) hover:underline">
                                                    {modLabel(m)} <ExternalLink size={12} aria-hidden="true" />
                                                </a>
                                            ))}
                                        </div>
                                    )}
                                </li>
                            );
                        })}
                    </ul>

                    <p>
                        <span className="text-(--base-09) font-medium">Mods trust the header only from listed addresses.</span>{' '}
                        The gateway connects from the server&apos;s private network, and that address changes when the
                        gateway updates. Add these ranges to the mod&apos;s trusted proxies (HAProxyCompat:{' '}
                        <code className="font-mono">trusted_proxies</code>, the others: <code className="font-mono">proxyServerIPs</code>):{' '}
                        <code className="font-mono text-(--base-09)">{PROXY_TRUSTED_RANGES.join(', ')}</code>.
                        {!routeOnly && <> For Auto, the server must also refuse connections without the header (HAProxyCompat: <code className="font-mono">require_proxy_protocol = true</code>; the others: keep these ranges out of <code className="font-mono">directAccessIPs</code>).</>}
                    </p>
                    <p>
                        <span className="text-(--base-09) font-medium">Behind Velocity or BungeeCord</span>, only the proxy the
                        gateway connects to gets this setting. Its backend servers get the IP through modern forwarding
                        and must not enable proxy-protocol themselves.
                    </p>
                    <p>
                        Restart the server after changing its config. Only use this while the server&apos;s port is
                        reachable through the gateway alone: anyone who can reach it directly could claim any IP.
                    </p>
                </div>
            </details>
        </div>
    );
}
