"use client";

import React, { useEffect, useState } from 'react';
import { Loader2 } from 'lucide-react';
import { getGatewayPublicTargets, setGatewayPublicTargets } from '@/lib/api/gatewayPublicTargets';

// Admin switch for one account. Only the loaded value is ever shown as on or
// off: while loading or after a failed read the control is withheld, because a
// guessed "off" invites a second, redundant enable.
export default function GatewayPublicTargetsToggle({ userId }: { userId: string }) {
    const [enabled, setEnabled] = useState<boolean | null>(null);
    const [saving, setSaving] = useState(false);
    const [error, setError] = useState('');

    useEffect(() => {
        let live = true;
        setEnabled(null);
        setError('');
        getGatewayPublicTargets(userId)
            .then(v => { if (live) setEnabled(v); })
            .catch(e => { if (live) setError(e instanceof Error ? e.message : 'Could not load the setting.'); });
        return () => { live = false; };
    }, [userId]);

    const toggle = async () => {
        if (enabled === null) return;
        setSaving(true);
        setError('');
        try {
            setEnabled(await setGatewayPublicTargets(userId, !enabled));
        } catch (e) {
            setError(e instanceof Error ? e.message : 'Could not save the setting.');
        } finally {
            setSaving(false);
        }
    };

    return (
        <div>
            <h4 className="mono-label mb-3">API Route Targets</h4>
            <div className="rounded-md bg-(--base-02) border border-(--base-03) p-3">
                <div className="flex items-center justify-between gap-3">
                    <div className="min-w-0">
                        <div className="font-medium text-sm text-(--base-09)">Allow public route targets (managed gateway API)</div>
                        <div className="text-xs text-(--base-06) mt-0.5">
                            Without this, routes made with this user&apos;s API keys may only target their own servers or private addresses. The panel is not affected.
                        </div>
                    </div>
                    {enabled === null && !error ? (
                        <Loader2 size={16} className="animate-spin text-(--base-06) shrink-0" aria-label="Loading" />
                    ) : enabled !== null && (
                        <button
                            type="button"
                            role="switch"
                            aria-checked={enabled}
                            aria-label="Allow public route targets"
                            onClick={toggle}
                            disabled={saving}
                            className={`toggle-track ${enabled ? 'toggle-track-on' : 'toggle-track-off'}`}
                        >
                            <span className={`toggle-knob ${enabled ? 'toggle-knob-on' : 'toggle-knob-off'}`} />
                        </button>
                    )}
                </div>
                {error && <p role="alert" className="text-xs text-(--error) mt-2">{error}</p>}
            </div>
        </div>
    );
}
