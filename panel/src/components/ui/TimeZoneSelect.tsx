"use client";

import { useMemo } from 'react';
import Select from '@/components/ui/Select';
import { timeZoneList } from '@/lib/time';

// The one zone picker: account preferences, a server's override and a
// scheduled task all choose from the same searchable list. `inheritLabel` adds
// an empty first option ("Same as the owner's account (Europe/Berlin)") for a
// value that may defer to another.
export default function TimeZoneSelect({ value, onChange, inheritLabel, id, disabled }: {
    value: string;
    onChange: (tz: string) => void;
    inheritLabel?: string;
    id?: string;
    disabled?: boolean;
}) {
    const options = useMemo(() => {
        const list = timeZoneList();
        // A stored zone this browser does not list still shows as itself.
        const zones = value && !list.includes(value) ? [value, ...list] : list;
        const opts = zones.map(z => ({ value: z, label: z.replace(/_/g, ' ') }));
        return inheritLabel !== undefined ? [{ value: '', label: inheritLabel }, ...opts] : opts;
    }, [value, inheritLabel]);
    return <Select id={id} value={value} onChange={onChange} options={options} disabled={disabled} searchable ariaLabel="Time zone" />;
}
