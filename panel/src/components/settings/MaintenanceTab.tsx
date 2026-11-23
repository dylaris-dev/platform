"use client";

import { Wrench } from 'lucide-react';
import { getMaintenance, saveMaintenance, MaintenanceState } from '@/lib/api';
import { toLocalInput, fromLocalInput } from '@/lib/localDateTime';
import { useSettingsForm } from '@/lib/useSettingsForm';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard, { SettingsGroup } from '@/components/settings/SettingsCard';
import { SwitchRow } from '@/components/ui/Switch';
import RadioCards from '@/components/settings/RadioCards';

const BLOCK_LEVELS: { value: MaintenanceState['blockLevel']; label: string; help: string }[] = [
    { value: 'off',          label: 'Off',              help: 'Feature off entirely. Banner hidden, no blocking.' },
    { value: 'banner_only',  label: 'Banner only',      help: 'Show the banner site-wide. Do not block any traffic.' },
    { value: 'block_writes', label: 'Block writes',     help: 'Non-admins can still read (GET) but cannot create, update or delete.' },
    { value: 'block_all',    label: 'Block everything', help: 'Non-admins are fully locked out. Admins always pass.' },
];

const defaultState: MaintenanceState = {
    active: false,
    title: 'Maintenance in progress',
    message: 'We are performing scheduled maintenance and will be back shortly.',
    expectedEnd: '',
    blockLevel: 'banner_only',
};

export default function MaintenanceTab() {
    // A failed load used to leave defaultState on screen looking exactly like a
    // stored config - maintenance OFF at banner_only - with Save enabled. This
    // is the one screen where writing that back is destructive: the DB migration
    // deliberately holds block_all for its whole run, and saving the default
    // would lift it and let users write to a database being copied. The hook's
    // null snapshot is that guard, and it also drives the card's disabled Save.
    const form = useSettingsForm<MaintenanceState>({
        load: async () => {
            const res = await getMaintenance();
            return res.success && res.state ? res.state : null;
        },
        save: async value => {
            const res = await saveMaintenance(value);
            return { ok: res.success, message: res.message, value: res.state };
        },
        successMessage: 'Maintenance settings saved.',
    });

    const state = form.value ?? defaultState;
    const patch = form.patch;

    return (
        <SettingsPage
            title="Maintenance mode"
            icon={Wrench}
            description="Show a system-wide banner to logged-in users, and optionally block writes or all non-admin traffic while you work. Admins are never blocked, otherwise you could not turn this off again from the panel."
            loading={form.loading}
        >
            <SettingsCard
                title="Maintenance"
                form={form}
                loadFailedMessage="The current maintenance settings could not be loaded, so the values below are defaults rather than what is stored. Saving is disabled until a reload succeeds: writing these back could lift a maintenance mode that is holding right now."
            >
                <SettingsGroup first>
                    <SwitchRow
                        label="Active"
                        description="When on, the banner is shown and the configured block level applies."
                        help={
                            <>
                                <p className="mb-2">
                                    <strong>You cannot lock yourself out.</strong> Admins are never
                                    blocked, and sign-in, the status endpoint and this page stay
                                    reachable for everyone - otherwise nobody could see why they are
                                    being turned away, or switch it back off.
                                </p>
                                <p>
                                    It gates the panel and its API only. Game servers keep running and
                                    players already connected stay connected; nothing here stops or
                                    starts a server.
                                </p>
                            </>
                        }
                        checked={state.active}
                        onChange={v => patch({ active: v })}
                    />
                </SettingsGroup>

                <SettingsGroup title="What users see">
                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="maint-title">Title</label>
                        <input
                            id="maint-title"
                            type="text"
                            value={state.title}
                            onChange={e => patch({ title: e.target.value })}
                            maxLength={120}
                            className="input-field w-full"
                        />
                    </div>

                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="maint-message">Message</label>
                        <textarea
                            id="maint-message"
                            value={state.message}
                            onChange={e => patch({ message: e.target.value })}
                            rows={3}
                            maxLength={500}
                            className="input-field w-full"
                        />
                        <p className="text-xs text-(--base-06)">Plain text.</p>
                    </div>

                    <div className="flex flex-col gap-[5px]">
                        <label className="input-label" htmlFor="maint-end">Expected end (optional)</label>
                        <input
                            id="maint-end"
                            type="datetime-local"
                            value={state.expectedEnd ? toLocalInput(state.expectedEnd) : ''}
                            onChange={e => patch({ expectedEnd: e.target.value ? fromLocalInput(e.target.value) : '' })}
                            style={{ colorScheme: 'dark' }}
                            className="input-field datetime-field w-full"
                        />
                        <p className="text-xs text-(--base-06)">
                            Used as a countdown in the banner and as a Retry-After hint on 503 responses.
                        </p>
                    </div>
                </SettingsGroup>

                <SettingsGroup
                    title="Block level"
                    help={
                        <>
                            <p className="mb-2">
                                How hard the door is shut, from a banner nobody is stopped by to a
                                full refusal.
                            </p>
                            <p>
                                <strong>Block writes</strong> is the one worth knowing: reads still
                                answer, so users can look at their servers and see the banner, and
                                only requests that change something are refused with a 503. It is the
                                right level for a migration; a full block is for when even reading
                                would be misleading.
                            </p>
                        </>
                    }
                >
                    <RadioCards
                        className="grid grid-cols-1 gap-2"
                        ariaLabel="Block level"
                        value={state.blockLevel}
                        onChange={blockLevel => patch({ blockLevel })}
                        options={BLOCK_LEVELS.map(opt => ({ value: opt.value, label: opt.label, description: opt.help }))}
                        cardClassName={checked => `flex items-start gap-3 p-3 rounded-lg border cursor-pointer transition-colors ${
                            checked
                                ? 'border-(--accent-border) bg-(--accent-ghost)'
                                : 'border-transparent bg-(--base-03) hover:bg-(--base-04)'
                        }`}
                    />
                </SettingsGroup>
            </SettingsCard>

            <p className="text-xs text-(--base-06)">
                The banner is mounted globally in the authenticated layout and polls{' '}
                <span className="font-mono">/api/maintenance</span> every 30 seconds while you are signed in.
            </p>
        </SettingsPage>
    );
}
