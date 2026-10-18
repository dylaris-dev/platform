"use client";

import React, { useState } from 'react';
import { Loader2 } from 'lucide-react';
import Switch from '@/components/ui/Switch';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import {
    proxyProtocolGuide, proxyProtocolEnableMessage, PROXY_PROTOCOL_DISABLE, PROXY_PROTOCOL_VANILLA,
} from '@/lib/proxyProtocolGuide';

type SaveResult = { success?: boolean; message?: string; error?: string } | null | undefined;

/**
 * The real-player-IP switch, for one server (RoutesModal) or one route-only
 * route (RouteOnlyPanel). Both directions confirm first: ON breaks a server
 * that is not set up for the header, OFF breaks one that still expects it.
 */
export default function ProxyProtocolSwitch({ enabled, software, viaLink, save, onSaved, compact = false }: {
    enabled: boolean;
    /** The server software (installerType). Empty or unknown shows the generic guide. */
    software?: string;
    /** Traffic reaches the server through the customer's own link (BYON, route-only). */
    viaLink: boolean;
    save: (enabled: boolean) => Promise<SaveResult>;
    onSaved: (enabled: boolean) => void;
    /** One line for a list row, without the description. */
    compact?: boolean;
}) {
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');
    const guide = proxyProtocolGuide(software ?? '');
    // Still switchable OFF when it is on, so a server whose software changed
    // after the switch was set is never stuck with it.
    const unsupported = !guide.supported && !enabled;

    const toggle = async (next: boolean) => {
        setError('');
        const ok = await confirmDialog(next
            ? {
                title: 'Pass the real player IP?',
                message: proxyProtocolEnableMessage(software ?? '', viaLink),
                confirmLabel: 'Turn on',
                destructive: false,
            }
            : {
                title: 'Stop passing the player IP?',
                message: PROXY_PROTOCOL_DISABLE,
                confirmLabel: 'Turn off',
                destructive: false,
            });
        if (!ok) return;
        setBusy(true);
        try {
            // fetchAPI resolves on 4xx/5xx with {success:false}; a 409 (setting
            // off), 503 (no gateway routing) or 403 arrives here, not in catch.
            const res = await save(next);
            if (res?.success === false) {
                setError(res.message || res.error || 'The setting could not be changed.');
                return;
            }
            onSaved(next);
        } catch {
            setError('The request could not be sent.');
        } finally {
            setBusy(false);
        }
    };

    return (
        <div className={compact ? 'mt-1' : ''}>
            <div className={`flex items-center gap-3 ${compact ? '' : 'justify-between'}`}>
                {compact ? (
                    <span className="text-xs text-(--base-07)">Real player IP</span>
                ) : (
                    <div className="min-w-0">
                        <div className="text-sm font-medium text-(--base-09)">Real player IP</div>
                        <div className="text-xs text-(--base-06) mt-0.5">
                            {unsupported
                                ? PROXY_PROTOCOL_VANILLA
                                : 'Sends each player\'s real address to the server (PROXY protocol). The server must be set up to expect it.'}
                        </div>
                    </div>
                )}
                <span className="flex items-center gap-2 shrink-0" title={unsupported ? PROXY_PROTOCOL_VANILLA : undefined} aria-busy={busy}>
                    {busy && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-hidden="true" />}
                    <Switch
                        checked={enabled}
                        onChange={toggle}
                        disabled={busy || unsupported}
                        ariaLabel="Real player IP"
                    />
                </span>
            </div>
            {compact && unsupported && <p className="text-xs text-(--base-06) mt-1">{PROXY_PROTOCOL_VANILLA}</p>}
            {error && <div className="alert alert-error text-xs mt-2" role="alert">{error}</div>}
        </div>
    );
}
