'use client';

import { useState } from 'react';
import { Check, Monitor, Server, MonitorSmartphone, Loader2, ListFilter } from 'lucide-react';
import { togglePackEnvironment, type PackEnvironment, type PackEnvironmentFilter } from '@/lib/packContent';

const controlClass = (selected: boolean) => `btn btn-sm ${selected ? 'bg-(--accent-ghost) border-(--accent-border) text-(--accent-light)' : 'btn-secondary'} disabled:opacity-50`;

export function PackEnvironmentSelector({ value, onChange, disabled, name }: {
    value: PackEnvironment;
    onChange: (next: PackEnvironment) => Promise<void>;
    disabled: boolean;
    name: string;
}) {
    const [saving, setSaving] = useState(false);
    return <div role="group" aria-label={`Install ${name} on`} className="flex items-center gap-1">
        {(['client', 'server'] as const).map(environment => {
            const selected = value === 'both' || value === environment;
            const next = togglePackEnvironment(value, environment);
            const Icon = environment === 'client' ? Monitor : Server;
            // The last selected side stays enabled-looking: dimming it made "on" read as "off".
            return <button key={environment} type="button" aria-pressed={selected} disabled={disabled || saving} aria-disabled={next === null || undefined}
                title={next === null ? 'At least one environment must remain selected' : `Include in the ${environment} download`}
                onClick={async () => {
                    if (next === null || saving || disabled) return;
                    setSaving(true);
                    try { await onChange(next); } finally { setSaving(false); }
                }} className={controlClass(selected)}>
                <Icon size={13} aria-hidden="true" /> {environment === 'client' ? 'Client' : 'Server'}
                {selected && <Check size={11} aria-hidden="true" />}
            </button>;
        })}
        {saving && <Loader2 size={12} role="status" aria-label="Saving environment" className="animate-spin" />}
    </div>;
}

export function PackEnvironmentFilter({ value, onChange }: { value: PackEnvironmentFilter; onChange: (value: PackEnvironmentFilter) => void }) {
    return <div role="group" aria-label="Filter installed content by environment" className="flex items-center flex-wrap gap-1.5">
        {([{ value: 'all', label: 'All', Icon: ListFilter }, { value: 'client', label: 'Client only', Icon: Monitor }, { value: 'server', label: 'Server only', Icon: Server }, { value: 'both', label: 'Client + Server', Icon: MonitorSmartphone }] as const).map(option => <button key={option.value} type="button"
            aria-pressed={value === option.value} onClick={() => onChange(option.value)} className={controlClass(value === option.value)}>
            <option.Icon size={13} aria-hidden="true" /> {option.label}
        </button>)}
    </div>;
}
