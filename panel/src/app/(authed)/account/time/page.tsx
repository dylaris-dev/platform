"use client";

import { useEffect, useState } from 'react';
import { Clock } from 'lucide-react';
import TimeZoneSelect from '@/components/ui/TimeZoneSelect';
import Segmented from '@/components/ui/Segmented';
import PageHeader from '@/components/ui/PageHeader';
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
            <main className="flex-1 overflow-y-auto app-page" aria-busy="true">
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
        <main className="flex-1 overflow-y-auto app-page space-y-6 animate-content-in">
            <PageHeader
                icon={<Clock size={16} />}
                title="Time &amp; date"
                description="The zone and clock the panel shows times in."
            />

            <div className="srv-surface p-5 space-y-6 max-w-3xl">
                <div>
                    <label className="srv-label mb-1.5 block" htmlFor="account-tz">Time zone</label>
                    <TimeZoneSelect id="account-tz" value={prefs.timezone} onChange={tz => setPrefs({ ...prefs, timezone: tz })} />
                    <p className="text-xs text-(--base-06) mt-1.5">
                        Your servers use it too, unless one sets its own under Configuration &rarr; Scheduled: for
                        the console&apos;s timestamps (from the next start) and as the zone of new scheduled tasks.
                    </p>
                </div>

                <div>
                    <span className="srv-label mb-1.5 block" id="account-clock-label">Clock</span>
                    <Segmented<TimeFormat>
                        ariaLabel="Clock format"
                        value={prefs.timeFormat}
                        onChange={v => setPrefs({ ...prefs, timeFormat: v })}
                        options={FORMATS.map(f => ({ id: f.value, label: f.label }))}
                    />
                    <p className="text-xs text-(--base-06) mt-1.5">Automatic follows your browser&apos;s language.</p>
                </div>

                <div className="flex items-center justify-between gap-3 pt-4 border-t border-(--base-03)">
                    <p className="text-xs text-(--base-06)">
                        Now: <span className="font-mono tabular-nums text-(--base-08)">{sample}</span>
                    </p>
                    <button onClick={save} disabled={!dirty || saving} className="btn btn-primary btn-sm disabled:opacity-40">
                        {saving ? 'Saving...' : 'Save'}
                    </button>
                </div>
            </div>
        </main>
    );
}
