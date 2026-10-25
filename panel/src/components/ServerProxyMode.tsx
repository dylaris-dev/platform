"use client";

import { useCallback, useEffect, useState } from 'react';
import { Loader2 } from 'lucide-react';
import Segmented from '@/components/ui/Segmented';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { getServerProxyMode, setServerProxyMode, type ProxyMode, type ServerProxyState } from '@/lib/api';
import { proxyProtocolEnableMessage, proxyModeStatus, PROXY_PROTOCOL_DISABLE } from '@/lib/proxyProtocolGuide';

const OPTIONS: { id: ProxyMode; label: string; hint: string }[] = [
    { id: 'auto', label: 'Auto', hint: 'Forward the player IP once the server shows it expects it' },
    { id: 'on', label: 'Always', hint: 'Always forward; the server must be set up for it' },
    { id: 'off', label: 'Off', hint: 'Never forward the player IP' },
];

/**
 * Real player IP for one managed server. Auto (the default) forwards only once
 * the server's own status ping shows it expects a PROXY header, so installing a
 * plugin or mod and restarting is all an owner does, and a server that cannot
 * read the header is never locked out.
 */
export default function ServerProxyMode({ serverId, software, viaLink, canWrite, onSaved }: {
    serverId: number;
    software?: string;
    viaLink: boolean;
    canWrite: boolean;
    onSaved?: () => void;
}) {
    const [state, setState] = useState<ServerProxyState | null>(null);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');

    const load = useCallback(async () => {
        const res = await getServerProxyMode(serverId).catch(() => null);
        if (res?.success && res.mode) {
            setState({ mode: res.mode, active: !!res.active, detected: res.detected ?? 'unknown' });
        }
    }, [serverId]);

    useEffect(() => { void load(); }, [load]);

    if (!state) return null;

    const change = async (next: ProxyMode) => {
        if (next === state.mode) return;
        setError('');
        if (next === 'on' && !(await confirmDialog({
            title: 'Always pass the real player IP?',
            message: proxyProtocolEnableMessage(software ?? '', viaLink),
            confirmLabel: 'Always on',
            destructive: false,
        }))) return;
        if (next === 'off' && state.active && !(await confirmDialog({
            title: 'Stop passing the player IP?',
            message: PROXY_PROTOCOL_DISABLE,
            confirmLabel: 'Turn off',
            destructive: true,
        }))) return;
        setBusy(true);
        try {
            const res = await setServerProxyMode(serverId, next);
            if (res?.success === false) {
                setError(res.message || res.error || 'The setting could not be changed.');
                return;
            }
            await load();
            onSaved?.();
        } catch {
            setError('The request could not be sent.');
        } finally {
            setBusy(false);
        }
    };

    return (
        <div>
            <div className="flex items-center justify-between gap-3 flex-wrap">
                <div className="min-w-0">
                    <div className="text-sm font-medium text-(--base-09)">Real player IP</div>
                    <div className="text-xs text-(--base-06) mt-0.5">{proxyModeStatus(state.mode, state.active, state.detected)}</div>
                </div>
                <span className="flex items-center gap-2 shrink-0" aria-busy={busy}>
                    {busy && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-hidden="true" />}
                    <Segmented
                        options={OPTIONS.map(o => ({ ...o, disabled: busy || !canWrite }))}
                        value={state.mode}
                        onChange={id => void change(id)}
                        ariaLabel="Real player IP"
                    />
                </span>
            </div>
            {error && <div className="alert alert-error text-xs mt-2" role="alert">{error}</div>}
        </div>
    );
}
