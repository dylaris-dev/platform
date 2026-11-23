"use client";

import Link from 'next/link';
import { LifeBuoy, Trash2, ArrowRight } from 'lucide-react';
import { getTicketSettings, saveTicketSettings, TicketSettings } from '@/lib/api/tickets';
import { useSettingsForm } from '@/lib/useSettingsForm';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard, { SettingsGroup, SettingsRow } from '@/components/settings/SettingsCard';
import { SwitchRow } from '@/components/ui/Switch';
import { LimitField } from '@/components/settings/LimitField';

const defaultSettings: TicketSettings = {
    crossTeamVisibility: true,
    watchersDefaultCanReply: false,
    allowUsersToAddWatchers: true,
    auditRetentionDays: 0, // matches the server default: no horizon until one is saved
    maxFileSizeMb: 10,
    maxTicketSizeMb: 50,
    maxUserSizeMb: 500,
    autoCloseEnabled: false,
    autoCloseDaysAfterResolved: 7,
    deletionEnabled: false,
};

export default function TicketSettingsTab() {
    const form = useSettingsForm<TicketSettings>({
        load: async () => {
            const res = await getTicketSettings();
            return res.success && res.settings ? res.settings : null;
        },
        save: async value => {
            const res = await saveTicketSettings(value);
            return { ok: res.success, message: res.message, value: res.settings };
        },
        successMessage: 'Ticket settings saved.',
    });

    // Before the first successful load there is nothing to edit. The form keeps
    // `dirty` false against a null snapshot, so the defaults below are display
    // only and cannot be written over the stored configuration.
    const s = form.value ?? defaultSettings;
    const patch = form.patch;

    return (
        <SettingsPage
            title="Ticket settings"
            icon={LifeBuoy}
            description="Visibility scope for support teams, watcher (CC) policy, attachment quotas and how long ticket audit history is kept."
            loading={form.loading}
        >
            <SettingsCard
                title="Tickets"
                description="Applies to every ticket in the panel."
                form={form}
            >
                <SettingsGroup first>
                    <SwitchRow
                        label="Cross-team visibility"
                        description="When on, every supporter sees every ticket. When off, supporters only see tickets assigned to them, unassigned tickets (for triage), or tickets matching their support_team."
                        checked={s.crossTeamVisibility}
                        onChange={v => patch({ crossTeamVisibility: v })}
                    />
                    <SwitchRow
                        label="Allow users to add watchers"
                        description="Lets the ticket creator CC other users on their own ticket. Watchers see all non-internal messages."
                        checked={s.allowUsersToAddWatchers}
                        onChange={v => patch({ allowUsersToAddWatchers: v })}
                    />
                    <SwitchRow
                        label="Default new watchers to reply-allowed"
                        description="When on, newly added watchers can post replies (non-internal). When off, watchers are read-only by default; admins and support can still flip individual watchers on a per-ticket basis."
                        checked={s.watchersDefaultCanReply}
                        onChange={v => patch({ watchersDefaultCanReply: v })}
                    />
                </SettingsGroup>

                <SettingsGroup title="Audit retention">
                    <SettingsRow
                        label="Keep ticket history for"
                        htmlFor="ticket-audit-retention"
                        description="0 keeps it forever. A positive value arms a daily sweep that deletes ticket audit history past that age; 180 is a reasonable horizon."
                    >
                        <input
                            id="ticket-audit-retention"
                            type="number"
                            min={0}
                            max={3650}
                            value={s.auditRetentionDays}
                            onChange={e => patch({ auditRetentionDays: Number(e.target.value) || 0 })}
                            className="input-field w-24"
                        />
                        <span className="text-sm text-(--base-07)">days</span>
                    </SettingsRow>
                </SettingsGroup>

                <SettingsGroup
                    title="Attachment quotas (MB)"
                    description="Switch an axis to No limit to leave it uncapped, or set 0 to forbid attachments on that axis entirely. The file quota gates a single upload, the ticket quota caps the total per ticket, and the user quota caps the total one user has stored across all their tickets."
                >
                    <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="ticket-quota-file">Per file</label>
                            <LimitField
                                id="ticket-quota-file"
                                value={s.maxFileSizeMb}
                                onChange={maxFileSizeMb => patch({ maxFileSizeMb })}
                                unit="MB"
                            />
                        </div>
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="ticket-quota-ticket">Per ticket</label>
                            <LimitField
                                id="ticket-quota-ticket"
                                value={s.maxTicketSizeMb}
                                onChange={maxTicketSizeMb => patch({ maxTicketSizeMb })}
                                unit="MB"
                            />
                        </div>
                        <div className="flex flex-col gap-[5px]">
                            <label className="input-label" htmlFor="ticket-quota-user">Per user</label>
                            <LimitField
                                id="ticket-quota-user"
                                value={s.maxUserSizeMb}
                                onChange={maxUserSizeMb => patch({ maxUserSizeMb })}
                                unit="MB"
                            />
                        </div>
                    </div>
                </SettingsGroup>

                <SettingsGroup title="Auto-close">
                    <SwitchRow
                        label="Auto-close resolved tickets"
                        description="Daily background job. When on, resolved tickets get closed after the configured idle period, which keeps the inbox clean without manual sweeping."
                        checked={s.autoCloseEnabled}
                        onChange={v => patch({ autoCloseEnabled: v })}
                    />
                    <SettingsRow
                        label="Idle days before close"
                        htmlFor="ticket-autoclose-days"
                        className={s.autoCloseEnabled ? '' : 'opacity-50'}
                    >
                        <input
                            id="ticket-autoclose-days"
                            type="number" min={1} max={365}
                            value={s.autoCloseDaysAfterResolved}
                            onChange={e => patch({ autoCloseDaysAfterResolved: Number(e.target.value) || 0 })}
                            className="input-field w-24"
                            disabled={!s.autoCloseEnabled}
                        />
                        <span className="text-sm text-(--base-07)">days</span>
                    </SettingsRow>
                </SettingsGroup>

                {/* Off by default. When on, admins get a Delete button on the
                    ticket detail page and every delete is stamped in the audit
                    log. */}
                <SettingsGroup title="Deletion">
                    <SwitchRow
                        label="Allow ticket deletion"
                        description="When enabled, admins can permanently delete tickets. The audit entry is preserved."
                        checked={s.deletionEnabled}
                        onChange={v => patch({ deletionEnabled: v })}
                    />
                    <Link
                        href="/settings/tickets/deletion-log"
                        className="btn btn-secondary btn-sm inline-flex items-center gap-1.5"
                    >
                        <Trash2 size={13} /> View deletion log
                        <ArrowRight size={13} />
                    </Link>
                </SettingsGroup>
            </SettingsCard>

            <p className="text-xs text-(--base-06)">
                Categories are managed under <strong>Settings &rarr; Ticket categories</strong>.
                Enabling the Tickets module itself is done under <strong>Modules</strong>.
            </p>
        </SettingsPage>
    );
}
