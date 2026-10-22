"use client";

import React, { useEffect, useState } from 'react';
import { Loader2 } from 'lucide-react';
import Switch from '@/components/ui/Switch';
import CopyText from '@/components/ui/CopyText';
import { confirmDialog } from '@/components/ui/ConfirmDialog';
import { getServerVoice, setServerVoice, serverPower, type ServerVoice } from '@/lib/api';
import { showVoiceSwitch, voiceAddress, voiceErrorMessage } from '@/lib/voiceChat';

const MOD_LABEL: Record<string, string> = { svc: 'Simple Voice Chat', plasmo: 'Plasmo Voice' };

/**
 * The per-server voice chat switch, laid out like ProxyProtocolSwitch.
 *
 * Renders nothing until Core says this server may turn voice on, except while
 * voice is already on: an owner must always be able to turn it off. A change
 * reaches the mod only on restart, so after either direction the owner is
 * offered one; declining keeps the note on screen.
 */
export default function VoiceChatSwitch({ serverId }: { serverId: number }) {
    const [voice, setVoice] = useState<ServerVoice | null>(null);
    const [busy, setBusy] = useState(false);
    const [error, setError] = useState('');
    const [pendingRestart, setPendingRestart] = useState(false);

    useEffect(() => {
        let cancelled = false;
        // A failed read (gateway off, no permission) shows nothing rather than
        // a switch whose state is a guess.
        getServerVoice(serverId)
            .then(res => { if (!cancelled && res?.success !== false && res?.voice) setVoice(res.voice); })
            .catch(() => {});
        return () => { cancelled = true; };
    }, [serverId]);

    // pendingRestart keeps it up after turning voice off on a server that may
    // not turn it on again, so the restart note does not vanish with it.
    if (!voice || !showVoiceSwitch(voice.allowed, voice.enabled || pendingRestart)) return null;

    const offerRestart = async () => {
        const ok = await confirmDialog({
            title: 'Restart now?',
            message: 'Players are disconnected.',
            confirmLabel: 'Restart',
            destructive: true,
        });
        if (!ok) return;
        setBusy(true);
        try {
            const res = await serverPower(serverId, 'restart');
            if (res?.success === false) {
                setError(res.message || res.error || 'The server could not be restarted.');
                return;
            }
            setPendingRestart(false);
        } catch {
            setError('The request could not be sent.');
        } finally {
            setBusy(false);
        }
    };

    const toggle = async (next: boolean) => {
        setError('');
        setBusy(true);
        let restart = false;
        try {
            // fetchAPI resolves on 4xx/5xx with {success:false}; the 409
            // refusals carry a code with their own sentence.
            const res = await setServerVoice(serverId, next);
            if (res?.success === false || !res?.voice) {
                setError(voiceErrorMessage(res?.code, res?.message || res?.error || 'Voice chat could not be changed.'));
                return;
            }
            setVoice(res.voice);
            restart = !!res.restartRequired;
            if (restart) setPendingRestart(true);
        } catch {
            setError('The request could not be sent.');
            return;
        } finally {
            setBusy(false);
        }
        if (restart) await offerRestart();
    };

    return (
        <div className="bg-(--base-02) rounded-md border border-(--base-03) px-3 py-2.5">
            <div className="flex items-center gap-3 justify-between">
                <div className="min-w-0">
                    <div className="text-sm font-medium text-(--base-09)">Voice chat</div>
                    <div className="text-xs text-(--base-06) mt-0.5">
                        Gives Simple Voice Chat or Plasmo Voice a public address through the gateway. The mod must be installed.
                    </div>
                </div>
                <span className="flex items-center gap-2 shrink-0" aria-busy={busy}>
                    {busy && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-hidden="true" />}
                    <Switch
                        checked={voice.enabled}
                        onChange={toggle}
                        disabled={busy}
                        ariaLabel="Voice chat"
                    />
                </span>
            </div>
            {voice.enabled && voice.host && voice.port > 0 && (
                <div className="flex flex-wrap items-center gap-2 mt-2">
                    <span className="mono-label">Voice address</span>
                    <CopyText value={voiceAddress(voice)} className="text-xs font-mono text-(--base-08)" />
                    {MOD_LABEL[voice.mod] && <span className="text-xs text-(--base-06)">{MOD_LABEL[voice.mod]}</span>}
                </div>
            )}
            {pendingRestart && (
                <p className="text-xs text-(--warning-light) mt-2">
                    Applies on next restart.{' '}
                    <button
                        type="button"
                        onClick={offerRestart}
                        disabled={busy}
                        className="underline underline-offset-2 hover:text-(--base-09) focus-visible:text-(--base-09) disabled:opacity-40"
                    >
                        Restart now
                    </button>
                </p>
            )}
            {error && <div className="alert alert-error text-xs mt-2" role="alert">{error}</div>}
        </div>
    );
}
