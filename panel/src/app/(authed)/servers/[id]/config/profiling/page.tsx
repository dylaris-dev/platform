"use client";

import React, { useCallback, useEffect, useRef, useState } from 'react';

import { Activity, Play, Square, Trash2, ExternalLink, RefreshCw, AlertTriangle, Download, Clock } from 'lucide-react';
import { useAppData } from '@/lib/AppDataContext';
import { systemEvents } from '@/lib/systemEvents';
import { sendConsoleCommand } from '@/lib/api';
import { listInstalledMods } from '@/lib/api/modrinth';
import { listSparkProfiles, recordSparkProfile, deleteSparkProfile, SPARK_URL_RE, type SparkProfile } from '@/lib/api/spark';
import { subscribeEventSource } from '@/lib/sse';
import { useBusy } from '@/lib/useBusy';
import { toast } from '@/components/ui/Toast';
import { useRouteId } from '@/lib/routeParams';

// Spark profiler integration. UX:
//   * "Not installed" → prompts user to add `spark` from the Content tab
//   * "Idle" → "Start profile" button (duration + thread grouper)
//   * "Running" → live timer + "Stop profile" button
//
// While a profile is running we open a *second* EventSource on the console
// stream and watch for spark.lucko.me URLs. Match → record via backend.
// Re-using the existing /console/stream means no backend log-shipper work.

const DURATIONS = [
    { label: '30 seconds', sec: 30 },
    { label: '1 minute',   sec: 60 },
    { label: '5 minutes',  sec: 300 },
    { label: '15 minutes', sec: 900 },
];

interface ActiveProfile {
    startedAt: number;       // ms
    durationSec: number;     // requested timeout
}

export default function ServerConfigProfilingPage() {
    const paramId = useRouteId('servers');
    const { servers } = useAppData();
    const serverId = Number(paramId);
    const server = servers.find(s => s.id === serverId);

    const [sparkInstalled, setSparkInstalled] = useState<boolean | null>(null);
    const [stoppingProfiler, runStop] = useBusy();
    const [profiles, setProfiles] = useState<SparkProfile[]>([]);
    const [active, setActive] = useState<ActiveProfile | null>(null);
    const [duration, setDuration] = useState(60);
    const [nowTick, setNowTick] = useState(Date.now());

    const stopStreamRef = useRef<(() => void) | null>(null);

    // useCallback is load-bearing here, not tidiness: this is in the dependency
    // array of the effect that opens the console SSE stream, and a 1s ticker
    // re-renders this component for the whole run. A new identity per render
    // tore the stream down and re-opened it every second, so the single line
    // carrying the spark URL could land in a reconnect gap and the run would
    // hang until the watchdog gave up.
    const showToast = useCallback((msg: string, ok = true) => toast(msg, ok), []);

    // ----- Spark install detection (via P10 installed_mods) -----
    useEffect(() => {
        let cancelled = false;
        (async () => {
            let mods;
            try {
                mods = await listInstalledMods(serverId);
            } catch {
                // Left at null, which this page already means as "unknown": the
                // "Spark is not installed" banner is gated on === false, so a
                // failed lookup no longer asserts something it did not learn.
                return;
            }
            if (cancelled) return;
            setSparkInstalled(mods.some(m =>
                m.modrinthProjectSlug === 'spark' ||
                /^spark[-_]/.test(m.fileName.toLowerCase()),
            ));
        })();
        return () => { cancelled = true; };
    }, [serverId]);

    // ----- History -----
    const refresh = useCallback(async () => {
        const list = await listSparkProfiles(serverId);
        setProfiles(list);
    }, [serverId]);
    useEffect(() => { refresh(); }, [refresh]);

    useEffect(() => {
        const unsub = systemEvents.on('spark_profiles.changed', (evt) => {
            const sid = (evt.payload as any)?.serverId;
            if (sid === undefined || sid === serverId) refresh();
        });
        return () => { unsub(); };
    }, [serverId, refresh]);

    // ----- Live timer + auto-stop watchdog while profiling -----
    useEffect(() => {
        if (!active) return;
        const id = setInterval(() => setNowTick(Date.now()), 1000);
        return () => clearInterval(id);
    }, [active]);

    // ----- Console SSE watcher: matches spark URLs during an active profile -----
    useEffect(() => {
        if (!active) {
            stopStreamRef.current?.();
            stopStreamRef.current = null;
            return;
        }
        // Kept open across the stream's periodic reconnects: a profile can
        // run longer than one stream lives, and its URL arrives at the end.
        const stop = subscribeEventSource(`/servers/${serverId}/console/stream`, (e) => {
            const m = e.data && SPARK_URL_RE.exec(e.data);
            if (m && active) {
                const captured = m[0];
                const startedISO = new Date(active.startedAt).toISOString();
                recordSparkProfile(serverId, captured, startedISO).then(res => {
                    if (res.success) {
                        showToast('Profile URL captured.', true);
                        refresh();
                    }
                });
                // First URL ends the active run; spark may print the URL on
                // its own timeout OR when we issued `stop`.
                setActive(null);
                stop();
                stopStreamRef.current = null;
            }
        });
        stopStreamRef.current = stop;
        return () => { stop(); stopStreamRef.current = null; };
    }, [active, serverId, refresh, showToast]);

    // ----- Start / stop -----

    const handleStart = async () => {
        if (!sparkInstalled) {
            showToast('Spark not installed. Add it from the Content tab.', false);
            return;
        }
        // `spark profiler --timeout <sec>` auto-completes after the window;
        // the URL is printed to console which our watcher picks up.
        const res = await sendConsoleCommand(serverId, `spark profiler --timeout ${duration}`);
        if (res.success === false) {
            showToast(res.message || 'Failed to send command', false);
            return;
        }
        setActive({ startedAt: Date.now(), durationSec: duration });
        showToast(`Profiling for ${duration}s…`, true);
    };

    const handleStop = async () => {
        // handleStart and handleDelete both read res.success. This one did not,
        // and then claimed success in a toast regardless - so a command that
        // never reached the server told the user to wait for a URL that was
        // never going to arrive, with the run left showing as active.
        const res = await sendConsoleCommand(serverId, 'spark profiler --stop');
        if (res?.success === false) {
            showToast(res.message || 'Could not send the stop command', false);
            return;
        }
        // Keep `active` until the URL arrives; the watcher will close it.
        showToast('Stop requested — waiting for spark to finalise URL.', true);
    };

    const handleDelete = async (p: SparkProfile) => {
        const res = await deleteSparkProfile(serverId, p.id);
        if (res.success) refresh();
        else showToast(res.message || 'Delete failed', false);
    };

    const elapsed = active ? Math.max(0, Math.floor((nowTick - active.startedAt) / 1000)) : 0;
    const remaining = active ? Math.max(0, active.durationSec - elapsed) : 0;

    if (!server) return null;

    return (
        <div className="max-w-3xl space-y-4">
            <header className="flex items-start gap-3">
                <div className="w-9 h-9 rounded-md bg-(--accent-ghost) flex items-center justify-center shrink-0">
                    <Activity size={16} className="text-(--accent-light)" />
                </div>
                <div className="min-w-0 flex-1">
                    <h2 className="text-base font-display font-semibold text-(--base-09)">Spark Profiler</h2>
                    <p className="text-xs text-(--base-06) mt-0.5">
                        CPU + memory profiles via{' '}
                        <a href="https://spark.lucko.me" target="_blank" rel="noopener noreferrer" className="text-(--accent-light) inline-flex items-center gap-1">
                            spark <ExternalLink size={9} />
                        </a>
                        . Profile data is uploaded to spark.lucko.me and we save the share URL here for cross-session history.
                    </p>
                </div>
            </header>

            {/* Install status */}
            {sparkInstalled === false && (
                <section className="card p-4 flex items-start gap-3 border-(--warning)/30">
                    <AlertTriangle size={16} className="text-(--warning-light) shrink-0 mt-0.5" />
                    <div className="flex-1">
                        <div className="text-sm font-medium text-(--base-09)">Spark not installed</div>
                        <p className="text-xs text-(--base-06) mt-0.5">
                            Add <code className="font-mono">spark</code> from the Content tab — pick the
                            version that matches your server&apos;s loader.
                        </p>
                    </div>
                </section>
            )}

            {/* Controls */}
            <section className="card p-5 space-y-4">
                <h3 className="text-sm font-medium text-(--base-09)">Run a profile</h3>

                {!active ? (
                    <>
                        <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                            <div>
                                <label className="input-label">Duration</label>
                                <select
                                    value={duration}
                                    onChange={e => setDuration(parseInt(e.target.value, 10))}
                                    className="input-field w-full"
                                    disabled={sparkInstalled === false}
                                >
                                    {DURATIONS.map(d => (
                                        <option key={d.sec} value={d.sec}>{d.label}</option>
                                    ))}
                                </select>
                            </div>
                            <div className="flex items-end">
                                <button
                                    onClick={handleStart}
                                    className="btn btn-primary w-full"
                                    disabled={sparkInstalled === false}
                                >
                                    <Play size={13} />
                                    Start profile
                                </button>
                            </div>
                        </div>
                        <p className="text-xs text-(--base-06)">
                            Sends <code className="font-mono">spark profiler --timeout {duration}</code> to
                            the console; URL appears when spark finishes.
                        </p>
                    </>
                ) : (
                    <div className="flex items-center justify-between gap-4">
                        <div className="flex items-center gap-3">
                            <RefreshCw size={14} className="animate-spin text-(--accent-light)" />
                            <div>
                                <div className="text-sm text-(--base-09)">Profiling… {elapsed}s elapsed</div>
                                <div className="text-xs text-(--base-06) flex items-center gap-1">
                                    <Clock size={11} />
                                    ~{remaining}s remaining
                                </div>
                            </div>
                        </div>
                        <button onClick={() => runStop(handleStop)} disabled={stoppingProfiler} className="btn btn-secondary btn-sm disabled:opacity-40">
                            <Square size={12} />
                            Stop
                        </button>
                    </div>
                )}
            </section>

            {/* History */}
            <section className="card p-5">
                <div className="flex items-center justify-between mb-3">
                    <h3 className="text-sm font-medium text-(--base-09)">History</h3>
                    <button onClick={refresh} className="btn btn-secondary btn-sm">
                        <RefreshCw size={12} />
                    </button>
                </div>

                {profiles.length === 0 ? (
                    <p className="text-xs text-(--base-06) text-center py-6">
                        No profiles captured yet.
                    </p>
                ) : (
                    <div className="space-y-2">
                        {profiles.map(p => (
                            <article key={p.id} className="flex items-center gap-3 p-2 rounded-md border border-(--base-04)">
                                <Download size={14} className="text-(--accent-light) shrink-0" />
                                <div className="min-w-0 flex-1">
                                    <a
                                        href={p.url}
                                        target="_blank"
                                        rel="noopener noreferrer"
                                        className="text-sm text-(--accent-light) font-mono truncate block hover:underline"
                                    >
                                        {p.url}
                                    </a>
                                    <div className="text-xs text-(--base-06)">
                                        Captured {new Date(p.completedAt).toLocaleString()}
                                        {p.subServerName && <> · {p.subServerName}</>}
                                    </div>
                                </div>
                                <button onClick={() => handleDelete(p)} className="btn btn-secondary btn-sm">
                                    <Trash2 size={12} className="text-(--error-light)" />
                                </button>
                            </article>
                        ))}
                    </div>
                )}
            </section>

        </div>
    );
}
