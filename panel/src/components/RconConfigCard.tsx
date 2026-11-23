"use client";

import Switch from '@/components/ui/Switch';
import { useCallback, useEffect, useRef, useState } from 'react';
import { Terminal, Copy, RefreshCw, EyeOff, AlertTriangle, Play, RotateCcw } from 'lucide-react';
import { getRconConfig, setRconConfig, execRcon, friendlyRconError } from '@/lib/api/rcon';
import { serverPower } from '@/lib/api';
import { toast } from '@/components/ui/Toast';
import { useUnsavedChanges } from '@/components/settings/UnsavedChanges';
import SettingsCard from '@/components/settings/SettingsCard';
import { SwitchRow } from '@/components/ui/Switch';
import Callout from '@/components/server/Callout';

// RCON enable/password card. Lives as the RCON sub-section of the Players
// tab. Generates a 24-byte hex password on first enable, supports manual
// override + regenerate, and ships a "Test command" mini-console so the
// operator can verify connectivity without leaving the tab. Enabling writes
// enable-rcon + the password into server.properties on the node (Core does
// this) so the operator never hand-edits the file. onEnabledChange lets the
// parent Players tab unlock/lock its other sections as RCON flips.

interface RconConfigCardProps {
    serverId: number;
    onEnabledChange?: (enabled: boolean) => void;
}

export default function RconConfigCard({ serverId, onEnabledChange }: RconConfigCardProps) {
    const [loaded, setLoaded] = useState(false);
    const [enabled, setEnabled] = useState(false);
    const [port, setPort] = useState(25575);
    const [hasSecret, setHasSecret] = useState(false);
    // Console noise filter. Kept as its own piece of state and saved on its own
    // because it applies live: unlike everything else on this card it never
    // touches server.properties and needs no restart.
    const [hideLogNoise, setHideLogNoise] = useState(false);
    const [savingFilter, setSavingFilter] = useState(false);
    const [revealed, setRevealed] = useState<string | null>(null);
    const [saving, setSaving] = useState(false);
    const [dirty, setDirty] = useState(false);
    const [testCmd, setTestCmd] = useState('list');
    const [testing, setTesting] = useState(false);
    const [testOutput, setTestOutput] = useState<{ ok: boolean; text: string } | null>(null);
    // Set when a Save actually rewrote server.properties (Core's
    // restartRequired) while enabled=true - the running server (if any) has
    // not picked it up yet, so we hold off treating RCON as live until an
    // explicit restart + reachability check succeed. No auto-restart (owner
    // decision) - the operator drives it via the button below.
    const [needsRestart, setNeedsRestart] = useState(false);
    const [restarting, setRestarting] = useState(false);
    // Guards the post-restart reachability poll against setting state after
    // the card has unmounted (e.g. the operator navigates away mid-poll).
    const restartPollRef = useRef<{ cancelled: boolean } | null>(null);
    useEffect(() => () => { if (restartPollRef.current) restartPollRef.current.cancelled = true; }, []);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const res = await getRconConfig(serverId);
        if (!res.success) return;
        setEnabled(res.enabled);
        setPort(res.port || 25575);
        setHasSecret(res.hasSecret);
        setHideLogNoise(res.hideLogNoise ?? false);
        setLoaded(true);
        setDirty(false);
        // A persisted restartRequired means MC has not reopened the RCON listener
        // yet: restore the banner and, exactly like the post-save flow, keep the
        // parent Players tabs locked until a restart confirms RCON is live. This
        // is what survives a page reload (the flag now comes from the DB, not
        // just local state).
        const pending = res.enabled && (res.restartRequired ?? false);
        setNeedsRestart(pending);
        onEnabledChange?.(res.enabled && !pending);
    }, [serverId, onEnabledChange]);

    useEffect(() => { refresh(); }, [refresh]);

    const handleSave = useCallback(async (opts: { regenerate?: boolean } = {}): Promise<boolean> => {
        setSaving(true);
        const res = await setRconConfig(serverId, { enabled, port, regenerate: opts.regenerate });
        setSaving(false);
        if (!res.success) {
            showToast(res.message || 'Save failed', false);
            return false;
        }
        setEnabled(res.enabled);
        setPort(res.port);
        setHasSecret(res.hasSecret);
        setDirty(false);
        if (res.password) {
            setRevealed(res.password);
            showToast('Password regenerated. Copy it now — it won\'t be shown again.', true);
        } else {
            showToast(res.message || 'Saved.', true);
        }
        if (res.enabled && res.restartRequired) {
            // server.properties changed but MC has not reopened the RCON
            // listener yet - do NOT optimistically unlock the Players tabs.
            // Wait for the operator to restart and for a reachability check
            // to actually confirm RCON is live.
            setNeedsRestart(true);
        } else {
            setNeedsRestart(false);
            onEnabledChange?.(res.enabled);
        }
        return true;
    }, [serverId, enabled, port, showToast, onEnabledChange]);

    // The port and the enable switch go through this card's own Save, like
    // every other form in the panel. The regenerate button below is an ACTION
    // and stays immediate: it mints a new password and shows it once.
    useUnsavedChanges({
        dirty,
        saving,
        save: () => handleSave(),
        discard: () => { void refresh(); },
    });

    // Saved immediately on toggle rather than through the Save button: it takes
    // effect within seconds and has no restart to wait for, so queueing it behind
    // the dirty/Save flow that exists for server.properties would only make it
    // look like it needs one.
    const handleFilterToggle = useCallback(async (next: boolean) => {
        setHideLogNoise(next);
        setSavingFilter(true);
        const res = await setRconConfig(serverId, { enabled, port, hideLogNoise: next });
        setSavingFilter(false);
        if (!res.success) {
            setHideLogNoise(!next); // roll back so the switch never lies
            showToast(res.message || 'Could not save the console filter', false);
            return;
        }
        setHideLogNoise(res.hideLogNoise ?? next);
        showToast(next ? 'RCON connection lines hidden from the console.' : 'RCON connection lines shown again.', true);
    }, [serverId, enabled, port, showToast]);

    const handleRestart = useCallback(async () => {
        setRestarting(true);
        try {
            const powerRes: any = await serverPower(serverId, 'restart');
            if (powerRes && (powerRes.success === false || powerRes.error)) {
                showToast(powerRes.error || powerRes.message || 'Restart failed', false);
                setRestarting(false);
                return;
            }
        } catch {
            showToast('Restart request failed', false);
            setRestarting(false);
            return;
        }
        // MC needs a few seconds to boot before the RCON listener is up; poll
        // reachability instead of guessing a fixed delay. Re-runs the same
        // exec path the Test command uses (no new endpoint).
        const token = { cancelled: false };
        restartPollRef.current = token;
        const maxAttempts = 12;
        const delayMs = 5000;
        for (let attempt = 0; attempt < maxAttempts; attempt++) {
            await new Promise(resolve => setTimeout(resolve, delayMs));
            if (token.cancelled) return;
            const test = await execRcon(serverId, 'list');
            if (token.cancelled) return;
            if (test.success) {
                setNeedsRestart(false);
                setRestarting(false);
                onEnabledChange?.(true);
                showToast('Server restarted. RCON is live.', true);
                return;
            }
        }
        setRestarting(false);
        showToast('Server restarted but RCON is still not reachable. Check the console.', false);
    }, [serverId, onEnabledChange, showToast]);

    const handleTest = useCallback(async () => {
        const cmd = testCmd.trim();
        if (!cmd) return;
        setTesting(true);
        setTestOutput(null);
        const res = await execRcon(serverId, cmd);
        setTesting(false);
        setTestOutput({
            ok: res.success,
            text: res.success ? (res.output || '(no output)') : friendlyRconError(res.error, 'RCON request failed'),
        });
    }, [serverId, testCmd]);

    const copy = (text: string) => {
        navigator.clipboard.writeText(text);
        showToast('Copied to clipboard.', true);
    };

    return (
        <SettingsCard
            title="RCON"
            icon={Terminal}
            form={{ dirty, saving, save: () => handleSave(), discard: () => { void refresh(); } }}
            /* Scheduled Tasks used to be named here too. They do not use RCON:
               a "say" job is pushed onto the server's stdin queue and runs with
               RCON off, so the sentence was talking people into opening a
               password-authenticated port they did not need. */
            description={<>
                Remote console. Powers live player management: the online list, kick, ban and op,
                the whitelist and the operators list. Enabling writes{' '}
                <code className="font-mono">enable-rcon=true</code> and this password into
                server.properties for you, and the server needs a restart to apply it.
            </>}
        >
            <SwitchRow
                label="RCON enabled"
                checked={enabled}
                disabled={!loaded}
                onChange={() => { setEnabled(e => !e); setDirty(true); }}
            />

            <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
                <div>
                    <label className="input-label">RCON Port</label>
                    <input
                        type="number"
                        min={1024}
                        max={65535}
                        value={port}
                        onChange={e => { setPort(parseInt(e.target.value || '25575', 10)); setDirty(true); }}
                        className="input-field input-mono w-full"
                        disabled={!loaded}
                    />
                    <p className="text-xs text-(--base-06) mt-1">Default 25575.</p>
                </div>

                <div className="sm:col-span-2">
                    <label className="input-label">Password</label>
                    <div className="flex items-center gap-2">
                        <div className="flex-1 input-field input-mono flex items-center gap-2">
                            {revealed ? (
                                <span className="truncate">{revealed}</span>
                            ) : (
                                <span className="text-(--base-06)">{hasSecret ? '••••••••••••••••••••' : '(not set)'}</span>
                            )}
                        </div>
                        {revealed && (
                            <>
                                <button onClick={() => copy(revealed)} className="btn btn-secondary btn-sm" title="Copy">
                                    <Copy size={12} />
                                </button>
                                <button onClick={() => setRevealed(null)} className="btn btn-secondary btn-sm" title="Hide">
                                    <EyeOff size={12} />
                                </button>
                            </>
                        )}
                        <button onClick={() => handleSave({ regenerate: true })} className="btn btn-secondary btn-sm" disabled={saving}>
                            <RefreshCw size={12} />
                            Regenerate
                        </button>
                    </div>
                    {!hasSecret && enabled && (
                        <p className="text-xs text-(--warning-light) mt-1 flex items-center gap-1">
                            <AlertTriangle size={11} />
                            Saving will mint a fresh random password (shown once).
                        </p>
                    )}
                </div>
            </div>

            {/* Console filter. Above the Save row on purpose: it saves itself and
                is NOT covered by the "requires a restart" note below it. */}
            <div className="flex items-start justify-between gap-3 border-t border-(--base-03) pt-3">
                <div className="min-w-0">
                    <div className="text-sm font-medium text-(--base-09)">Hide RCON lines from the console</div>
                    <p className="text-xs text-(--base-06) mt-0.5 leading-snug">
                        The panel opens an RCON connection every few seconds to read the player list, and Minecraft
                        logs a start and a shutdown line for each one. Hiding them keeps the console readable.
                        Applies within a few seconds, no restart.
                    </p>
                </div>
                <Switch checked={hideLogNoise} onChange={handleFilterToggle} ariaLabel="Hide RCON connection lines from the console" disabled={!loaded || savingFilter} className="shrink-0 mt-0.5" />
            </div>

            <div className="border-t border-(--base-03) pt-3">
                <p className="text-xs text-(--base-06)">
                    Requires a server restart to take effect.
                </p>
            </div>

            {needsRestart && (
                <Callout
                    tone="warning"
                    icon={<AlertTriangle size={16} />}
                    actions={
                        <button onClick={handleRestart} className="btn btn-secondary btn-sm" disabled={restarting}>
                            <RotateCcw size={14} className={restarting ? 'animate-spin motion-reduce:animate-none' : ''} />
                            {restarting ? 'Restarting…' : 'Restart now'}
                        </button>
                    }
                >
                    RCON enabled. Restart the server to apply.
                </Callout>
            )}

            {/* Test command — quick sanity check from inside the panel */}
            <div className="border-t border-(--base-03) pt-3">
                <label className="input-label flex items-center gap-2">
                    <Play size={11} />
                    Test command
                </label>
                <div className="flex items-center gap-2 mt-1">
                    <input
                        type="text"
                        value={testCmd}
                        onChange={e => setTestCmd(e.target.value)}
                        onKeyDown={e => { if (e.key === 'Enter') handleTest(); }}
                        placeholder="e.g. list"
                        className="input-field input-mono flex-1"
                        disabled={!enabled || !hasSecret || needsRestart}
                    />
                    <button
                        onClick={handleTest}
                        className="btn btn-secondary btn-sm"
                        disabled={testing || !enabled || !hasSecret || needsRestart}
                    >
                        {testing ? 'Running…' : 'Run'}
                    </button>
                </div>
                {testOutput && (
                    <pre className={`mt-2 p-2 rounded-md font-mono text-xs whitespace-pre-wrap border ${
                        testOutput.ok
                            ? 'bg-(--base-02) border-(--base-03) text-(--base-08)'
                            : 'bg-(--error-ghost) border-(--error)/30 text-(--error-light)'
                    }`}>{testOutput.text}</pre>
                )}
            </div>
        </SettingsCard>
    );
}
