"use client";

import { useEffect, useState } from 'react';
import { Clock } from 'lucide-react';
import TimeZoneSelect from '@/components/ui/TimeZoneSelect';
import { SkeletonCard } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';
import { useBusy } from '@/lib/useBusy';
import { browserTimeZone, getMyTimePrefs, prefOptions, saveMyTimePrefs, type TimeFormat, type TimePrefs } from '@/lib/time';

const FORMATS: { value: TimeFormat; label: string }[] = [
    { value: 'auto', label: 'Automatic' },
    { value: '24h', label: '24-hour' },
    { value: '12h', label: '12-hour (AM/PM)' },
];

// Time zone and clock format for this account. The zone is what the panel
// shows times in and what servers inherit unless they set their own, which
// decides the console's timestamps and the default zone of new scheduled tasks.
export default function AccountTimePage() {
    const [prefs, setPrefs] = useState<TimePrefs | null>(null);
    const [saved, setSaved] = useState<TimePrefs | null>(null);
    const [saving, runSave] = useBusy();

    useEffect(() => {
        getMyTimePrefs().then(p => {
            const v = p ? { timezone: p.timezone || browserTimeZone(), timeFormat: p.timeFormat || 'auto' } : null;
            setPrefs(v);
            setSaved(v);
        });
    }, []);

    if (!prefs) {
        return (
            <main className="flex-1 overflow-y-auto p-6 max-w-3xl" aria-busy="true">
                <SkeletonCard />
            </main>
        );
    }

    const dirty = !saved || saved.timezone !== prefs.timezone || saved.timeFormat !== prefs.timeFormat;
    const sample = new Date().toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short', ...prefOptions(prefs) });

    const save = () => runSave(async () => {
        const res = await saveMyTimePrefs(prefs);
        if (res.success) {
            setSaved(prefs);
            toast('Saved.', true);
        } else {
            toast(res.message || 'Could not save', false);
        }
    });

    return (
        <main className="flex-1 overflow-y-auto p-6 max-w-3xl animate-content-in">
            <header className="flex items-center gap-3 mb-4">
                <Clock size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09)">Time &amp; date</h1>
            </header>

            <div className="card p-5 space-y-5">
                <div>
                    <label className="input-label" htmlFor="account-tz">Time zone</label>
                    <TimeZoneSelect id="account-tz" value={prefs.timezone} onChange={tz => setPrefs({ ...prefs, timezone: tz })} />
                    <p className="text-xs text-(--base-06) mt-1">
                        Your servers use it too, unless one sets its own under Configuration &rarr; Scheduled: for
                        the console&apos;s timestamps (from the next start) and as the zone of new scheduled tasks.
                    </p>
                </div>

                <fieldset>
                    <legend className="input-label">Clock</legend>
                    <div className="grid grid-cols-1 sm:grid-cols-3 gap-2 mt-1">
                        {FORMATS.map(f => (
                            <button
                                key={f.value}
                                type="button"
                                aria-pressed={prefs.timeFormat === f.value}
                                onClick={() => setPrefs({ ...prefs, timeFormat: f.value })}
                                className={`px-3 py-2 rounded-md border text-sm text-left transition-colors focus-visible:outline-2 focus-visible:outline-(--accent) ${
                                    prefs.timeFormat === f.value
                                        ? 'border-(--accent) bg-(--accent-ghost) text-(--accent-light)'
                                        : 'border-(--base-04) text-(--base-07) hover:bg-(--base-03)'
                                }`}
                            >
                                {f.label}
                            </button>
                        ))}
                    </div>
                    <p className="text-xs text-(--base-06) mt-1">Automatic follows your browser&apos;s language.</p>
                </fieldset>

                <div className="flex items-center justify-between gap-3 border-t border-(--base-03) pt-4">
                    <p className="text-xs text-(--base-06)">
                        Now: <span className="font-mono text-(--base-08)">{sample}</span>
                    </p>
                    <button onClick={save} disabled={!dirty || saving} className="btn btn-primary btn-sm disabled:opacity-40">
                        {saving ? 'Saving...' : 'Save'}
                    </button>
                </div>
            </div>
        </main>
    );
}
