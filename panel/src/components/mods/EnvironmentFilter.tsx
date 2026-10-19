"use client";

import { AlertTriangle, Monitor, MonitorSmartphone, Server as ServerIcon, type LucideIcon } from 'lucide-react';
import type { ReactNode } from 'react';
import type { EnvFilter, ServerBucketFilter } from '@/lib/modSide';

// The category buttons' style, so a sidebar reads as one list of controls.
function toggleClass(on: boolean, inline: boolean): string {
    return `flex items-center gap-2 px-2.5 py-1.5 rounded-md border text-[13px] transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--accent-border) active:bg-(--base-04) disabled:opacity-50 disabled:pointer-events-none ${
        inline ? '' : 'w-full'
    } ${
        on
            ? 'bg-(--accent-ghost) border-(--accent-border) text-(--accent-light)'
            : 'bg-(--base-02) border-(--base-03) text-(--base-07) hover:bg-(--base-03) hover:border-(--base-04) hover:text-(--base-09)'
    }`;
}

function ToggleGroup({ inline, caption, children }: { inline: boolean; caption?: string; children: ReactNode }) {
    return (
        <div role="group" aria-label="Environment" className={inline ? 'flex items-center gap-1.5' : ''}>
            <span className={inline ? 'text-xs text-(--base-06)' : 'input-label'}>Environment</span>
            <div className={inline ? 'flex gap-1.5' : 'mt-2 flex flex-col gap-1.5'}>{children}</div>
            {caption && <p className="mt-1.5 text-[11px] text-(--base-06)">{caption}</p>}
        </div>
    );
}

// Modrinth's Environment filter: Client and Server, each on or off, neither on
// = no filter. Used where a pack is built for players, not for one server.
export function EnvironmentFilter({ value, onChange, inline = false }: {
    value: EnvFilter;
    onChange: (next: EnvFilter) => void;
    inline?: boolean;
}) {
    const options = [
        { key: 'client' as const, label: 'Client', Icon: Monitor },
        { key: 'server' as const, label: 'Server', Icon: ServerIcon },
    ];
    return (
        <ToggleGroup inline={inline}>
            {options.map(({ key, label, Icon }) => {
                const on = value[key];
                return (
                    <button
                        key={key}
                        type="button"
                        aria-pressed={on}
                        onClick={() => onChange({ ...value, [key]: !on })}
                        className={toggleClass(on, inline)}
                    >
                        <Icon size={14} aria-hidden="true" className="shrink-0" />
                        {label}
                    </button>
                );
            })}
        </ToggleGroup>
    );
}

const SERVER_OPTIONS: { key: Exclude<ServerBucketFilter, null>; label: string; meaning: string; Icon: LucideIcon }[] = [
    { key: 'server_only', label: 'Server only', meaning: "Players don't need these.", Icon: ServerIcon },
    { key: 'client_server', label: 'Client + Server', meaning: 'Players must install these too.', Icon: MonitorSmartphone },
];

const NO_BUTTON_CAPTION = 'Client-only mods are hidden: they do nothing on a server.';

// The server Content tab's filter: one of two buttons or none, clicking the
// selected one again clears it. Inline, the caption moves into the titles to
// keep the header row one line.
export function ServerSideFilter({ value, onChange, inline = false }: {
    value: ServerBucketFilter;
    onChange: (next: ServerBucketFilter) => void;
    inline?: boolean;
}) {
    const caption = SERVER_OPTIONS.find(o => o.key === value)?.meaning ?? NO_BUTTON_CAPTION;
    return (
        <ToggleGroup inline={inline} caption={inline ? undefined : caption}>
            {SERVER_OPTIONS.map(({ key, label, meaning, Icon }) => {
                const on = value === key;
                return (
                    <button
                        key={key}
                        type="button"
                        aria-pressed={on}
                        title={on ? `${meaning} Click again to show all.` : meaning}
                        onClick={() => onChange(on ? null : key)}
                        className={toggleClass(on, inline)}
                    >
                        <Icon size={14} aria-hidden="true" className="shrink-0" />
                        {label}
                    </button>
                );
            })}
        </ToggleGroup>
    );
}

// A mod's environment tag; warning marks one that does not belong on a server.
export function EnvTag({ label, title, warning = false }: { label: string; title?: string; warning?: boolean }) {
    return (
        <span title={title} className={`badge ${warning ? 'badge-warning' : 'badge-neutral'} text-[10px] font-sans shrink-0`}>
            {warning && <AlertTriangle size={10} aria-hidden="true" />}
            {label}
        </span>
    );
}
