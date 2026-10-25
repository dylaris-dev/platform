"use client";

import React, { useState } from 'react';
import { Loader2 } from 'lucide-react';
import Switch from '@/components/ui/Switch';
import CopyText from '@/components/ui/CopyText';
import { setLinkRouteVoice, type LinkRouteVoice } from '@/lib/api';
import { linkRouteVoiceConfig, showVoiceSwitch, validVoicePort, voiceErrorMessage } from '@/lib/voiceChat';

const DEFAULT_VOICE_PORT = 24454;

/**
 * Voice chat for one route-only route, one line in the route list like
 * ProxyProtocolSwitch. Core cannot write the voice mod's config on the
 * customer's machine, so once it is on the panel shows the lines to set.
 */
export default function LinkRouteVoiceSwitch({ domain, voice, allowed, onSaved }: {
    domain: string;
    voice: LinkRouteVoice | undefined;
    allowed: boolean;
    onSaved: (voice: LinkRouteVoice) => void;
}) {
    const enabled = !!voice?.enabled;
    const [targetPort, setTargetPort] = useState(voice?.target_port || DEFAULT_VOICE_PORT);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');

    if (!showVoiceSwitch(allowed, enabled)) return null;

    const toggle = async (next: boolean) => {
        setError('');
        if (next && !validVoicePort(targetPort)) {
            setError('The voice port must be between 1 and 65535.');
            return;
        }
        setBusy(true);
        try {
            const res = await setLinkRouteVoice(domain, next, targetPort);
            if (res?.success === false || !res?.voice) {
                setError(voiceErrorMessage(res?.code, res?.message || res?.error || 'Voice chat could not be changed.'));
                return;
            }
            onSaved(res.voice);
        } catch {
            setError('The request could not be sent.');
        } finally {
            setBusy(false);
        }
    };

    const cfg = enabled && voice?.port ? linkRouteVoiceConfig(domain, voice.port, voice.target_port || DEFAULT_VOICE_PORT) : null;

    return (
        <div className="mt-1">
            <div className="flex flex-wrap items-center gap-3">
                <span className="text-xs text-(--base-07)">Voice chat</span>
                <span className="flex items-center gap-2" aria-busy={busy}>
                    {busy && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-hidden="true" />}
                    <Switch checked={enabled} onChange={toggle} disabled={busy} ariaLabel={`Voice chat for ${domain}`} />
                </span>
                {!enabled && (
                    <label className="flex items-center gap-1.5 text-xs text-(--base-06)">
                        Mod port
                        <input
                            type="number"
                            min={1}
                            max={65535}
                            value={targetPort}
                            onChange={e => setTargetPort(Number(e.target.value))}
                            disabled={busy}
                            className="input-field text-xs w-20 py-0.5 text-center"
                            aria-label={`Voice mod port for ${domain}`}
                        />
                    </label>
                )}
            </div>
            {cfg && (
                <div className="mt-2 space-y-1.5 text-xs text-(--base-06)">
                    <p>Set this in your voice mod&apos;s config, then restart the server. Your link forwards to port {voice?.target_port || DEFAULT_VOICE_PORT} on the route&apos;s target host.</p>
                    <div className="flex flex-wrap items-start gap-2">
                        <span className="mono-label shrink-0">Simple Voice Chat</span>
                        <CopyText value={cfg.svc} className="font-mono text-(--base-08) whitespace-pre" />
                    </div>
                    <div className="flex flex-wrap items-start gap-2">
                        <span className="mono-label shrink-0">Plasmo Voice</span>
                        <CopyText value={cfg.plasmo} className="font-mono text-(--base-08) whitespace-pre" />
                    </div>
                </div>
            )}
            {error && <div className="alert alert-error text-xs mt-2" role="alert">{error}</div>}
        </div>
    );
}
