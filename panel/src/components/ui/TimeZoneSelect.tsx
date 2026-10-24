"use client";

import { useMemo } from 'react';
import { timeZoneList } from '@/lib/time';

// The one zone picker: account preferences, a server's override and a
// scheduled task all choose from the same list. `inheritLabel` adds an empty
// first option ("Same as my account (Europe/Berlin)") for a value that may
// defer to another.
export default function TimeZoneSelect({ value, onChange, inheritLabel, id, disabled }: {
    value: string;
    onChange: (tz: string) => void;
    inheritLabel?: string;
    id?: string;
    disabled?: boolean;
}) {
    const zones = useMemo(() => {
        const list = timeZoneList();
        // A stored zone this browser does not list still shows as itself.
        return value && !list.includes(value) ? [value, ...list] : list;
    }, [value]);
    return (
        <select id={id} value={value} onChange={e => onChange(e.target.value)} disabled={disabled} className="input-field w-full">
            {inheritLabel !== undefined && <option value="">{inheritLabel}</option>}
            {zones.map(z => <option key={z} value={z}>{z.replace(/_/g, ' ')}</option>)}
        </select>
    );
}
