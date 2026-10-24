"use client";

import { useEffect, useState } from 'react';
import { Gauge } from 'lucide-react';
import SettingsCard from '@/components/settings/SettingsCard';
import { toast } from '@/components/ui/Toast';
import { getDataThrottle, setDataThrottle } from '@/lib/api/trafficLimits';

/**
 * The speed data traffic (beam + custom tabs) slows to once a tenant without
 * data overage has used this month's data allowance. Slowed, not stopped,
 * until the month resets. The minimum keeps it usable: below 20 Mbit/s a large
 * upload takes days, which is a stop in all but name.
 */
export default function DataThrottleCard() {
    const [mbit, setMbit] = useState('');
    const [saved, setSaved] = useState('');
    const [min, setMin] = useState(20);
    const [loadFailed, setLoadFailed] = useState(false);
    const [saving, setSaving] = useState(false);

    useEffect(() => {
        getDataThrottle().then(r => {
            if (!r.success || r.mbit === undefined) { setLoadFailed(true); return; }
            setMbit(String(r.mbit));
            setSaved(String(r.mbit));
            if (r.minMbit) setMin(r.minMbit);
        });
    }, []);

    const n = Number(mbit);
    const invalid = !Number.isInteger(n) || n < min ? `At least ${min} Mbit/s.` : undefined;

    return (
        <SettingsCard
            title="Data traffic throttle"
            description="When a tenant without data overage has used this month's data allowance, beam transfers and custom tabs slow to this speed until the month resets. Player traffic is never throttled."
            icon={Gauge}
            saveBlockedReason={mbit !== saved ? invalid : undefined}
            form={{
                dirty: mbit !== saved,
                saving,
                loadFailed,
                discard: () => setMbit(saved),
                save: async () => {
                    setSaving(true);
                    try {
                        const r = await setDataThrottle(n);
                        if (!r.success) { toast(r.message || 'Could not save', false); return false; }
                        setSaved(mbit);
                        toast('Saved. Throttled tenants get the new speed within a minute.');
                        return true;
                    } finally {
                        setSaving(false);
                    }
                },
            }}
        >
            <label className="input-label" htmlFor="data-throttle-mbit">Speed (Mbit/s)</label>
            <input
                id="data-throttle-mbit"
                type="number"
                min={min}
                step={1}
                value={mbit}
                onChange={e => setMbit(e.target.value)}
                className="input-field w-40"
                aria-invalid={!!invalid}
            />
            {invalid && <p className="text-xs text-(--error-light) mt-1">{invalid}</p>}
        </SettingsCard>
    );
}
