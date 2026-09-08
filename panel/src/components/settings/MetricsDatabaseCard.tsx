"use client";

import { useCallback, useState } from 'react';
import { Database, AlertTriangle, Info } from 'lucide-react';
import { useSettingsForm } from '@/lib/useSettingsForm';
import {
    getMetricsDB, saveMetricsDB, testMetricsDB,
    metricsDBIncomplete,
    emptyMetricsDBTarget,
    type MetricsDBSettings, type MetricsDBRequest,
} from '@/lib/api/metricsDb';
import { useConnectionTest, readConnTest } from '@/lib/connectionTest';
import { TestConnectionButton, ConnectionTestNote } from '@/components/ui/ConnectionTest';
import SettingsCard from '@/components/settings/SettingsCard';
import { SwitchRow } from '@/components/ui/Switch';
import Select from '@/components/ui/Select';
import HelpTip from '@/components/ui/HelpTip';
import Checkbox from '@/components/ui/Checkbox';

/**
 * Long-term statistics: whether to record, and into which database.
 *
 * One card and ONE save, because those are one decision. Recording begins the
 * instant the switch goes on, the first bucket lands in whatever target is
 * stored, and nothing can be backfilled afterwards - so a switch that could be
 * flipped on another screen was a way to spend the only chance to choose
 * without noticing.
 *
 * It is also why the card is not part of the feature-switch bundle above: that
 * card saves seven booleans through one endpoint, this one has a target to
 * validate and a database to reach first. Two save models inside one card is
 * the exact confusion this page was untangled to remove; two cards for one
 * decision was the same mistake wearing the other hat.
 *
 * There is no choice of database. Recording into the platform's own database
 * at hour resolution used to be the alternative; there was no conversion
 * between the two resolutions, and the only durable effect of offering both was
 * history that turned out coarser than the operator thought. What remains is
 * one target, and the card's job is to make its requirements plain before
 * anything is written.
 */
export default function MetricsDatabaseCard() {
    // A save can succeed and still have something to say - a database that
    // works but has no TimescaleDB in it. That is not a test result and must
    // not sit in the test banner, where the next test would silently erase it.
    const [saveWarning, setSaveWarning] = useState<string | null>(null);

    const form = useSettingsForm<MetricsDBSettings>({
        load: async () => {
            const res = await getMetricsDB();
            if (!res.success || !res.settings) return null;
            // The server reports whether one is STORED; the checkbox is the
            // inverse of that. Derived on load and again after each save, so
            // the box always describes what is actually saved rather than what
            // was last typed.
            return { ...res.settings, noPassword: !res.settings.passwordSet };
        },
        save: async value => {
            const res = await saveMetricsDB(value);
            if (!res.success) return { ok: false, message: res.message };
            // The server's own copy wins: it normalises the port and ssl mode,
            // and it blanks the password it just stored.
            const stored = res.settings ?? value;
            if (res.warning) setSaveWarning(res.warning);
            return { ok: true, value: { ...stored, password: '', noPassword: !stored.passwordSet } };
        },
        successMessage: 'Statistics database saved.',
    });

    const value = form.value;
    const target: MetricsDBRequest = value ?? emptyMetricsDBTarget;

    const enabled = !!value?.enabled;
    const incomplete = metricsDBIncomplete(target);

    // The shared lifecycle: one request at a time, disabled while it runs, and
    // a deadline, because fetch has none of its own. The stage in the answer is
    // what turns "connection failed" into either "nothing answered on that
    // address" or "answered, and refused these credentials".
    const test = useConnectionTest(useCallback(async (signal: AbortSignal) => {
        const res = await testMetricsDB(target, signal);
        return readConnTest(res, 'Connected.');
    }, [target]));

    // A field edit invalidates the last test result. Leaving a green banner
    // above a host that has since been retyped is a claim about a connection
    // nobody made.
    const set = (partial: Partial<MetricsDBRequest>) => {
        test.clear();
        setSaveWarning(null);
        form.patch(partial);
    };

    return (
        <SettingsCard
            title="Long-term statistics"
            icon={Database}
            description="Whether this platform keeps a record of what it handled, and where that record is written."
            help={
                <>
                    <p className="mb-2">
                        Statistics are kept as minute buckets. At a modest fleet size that is on the
                        order of a hundred million rows a year, which is a query problem before it is
                        a storage one - so this wants <strong>TimescaleDB</strong>, which chunks the
                        table and compresses anything older than a week. A plain PostgreSQL is
                        accepted and works, but stores every minute as an ordinary row.
                    </p>
                    <p className="mb-2">
                        Give it a <strong>database of its own</strong>. The same PostgreSQL the
                        platform already runs on is the expected answer - one server, one extension,
                        a second database inside it. What it must not be is the platform&apos;s own
                        database: a table growing by a hundred million rows a year does not belong
                        beside the tables every page of this panel reads.
                    </p>
                    <p>
                        Nothing is backfilled. History starts at the save, so a target changed later
                        starts a new history rather than moving the old one.
                    </p>
                </>
            }
            form={form}
            // Only while recording is ON. Switching it off needs no database,
            // and blocking that save would leave an installation unable to stop
            // writing to one.
            saveBlockedReason={(enabled ? incomplete : null) ?? undefined}
            loadFailedMessage="The statistics database settings could not be loaded, so they are shown read-only. Saving now would write these defaults over the real configuration."
            actions={
                <TestConnectionButton
                    test={test}
                    small
                    blockedReason={incomplete ?? (form.loading || form.loadFailed
                        ? 'The settings could not be loaded, so there is nothing to test.'
                        : null)}
                />
            }
        >

            <SwitchRow
                label="Record long-term statistics"
                description="Keeps what this platform handles - players, traffic, CPU and RAM, uptime per component - in buckets that survive, so months of operation can be shown later. Off by default. Everything stays in this installation; nothing is sent anywhere."
                checked={enabled}
                disabled={form.loading || form.loadFailed}
                onChange={v => set({ enabled: v })}
            />

            {!enabled && (
                <p className="flex items-start gap-1.5 text-xs text-(--base-06) leading-relaxed">
                    <AlertTriangle size={12} className="mt-0.5 shrink-0 text-(--warning-light)" />
                    <span>
                        Nothing is being recorded. History starts when you switch this on and there
                        is no way to fill in what came before, so fill in the database below and
                        save both together rather than turning it on first.
                    </span>
                </p>
            )}

            {/* Above the fields, not in the help panel behind the icon: these
                two requirements are what makes a filled-in form wrong rather
                than incomplete, and a form cannot warn about that afterwards -
                the first minute bucket is already written by then. */}
            <p className="flex items-start gap-1.5 text-xs text-(--base-06) leading-relaxed max-w-2xl">
                <Info size={12} className="mt-0.5 shrink-0 text-(--base-07)" />
                <span>
                    Needs a PostgreSQL with <strong>TimescaleDB</strong>, and its <strong>own
                    database</strong> inside it. The same server the platform already uses is the
                    expected answer - a second database on it, never the platform&apos;s own. Without
                    the extension every minute becomes an ordinary row: it works, and it is the one
                    combination here that ends badly.
                </span>
            </p>

            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                <Field label="Host" value={target.host} onChange={v => set({ host: v })}
                    placeholder="metricsdb" />
                <Field label="Port" value={target.port} onChange={v => set({ port: v })}
                    placeholder="5432" />
                <Field label="Database name" value={target.dbName} onChange={v => set({ dbName: v })}
                    placeholder="dylaris_metrics" />
                <Field label="User" value={target.user} onChange={v => set({ user: v })}
                    placeholder="metrics" />
                <div>
                    <label className="input-label flex items-center gap-1.5">
                        Password
                        <HelpTip label="About the password">
                            <p className="mb-2">
                                A metrics database reachable only from Core - on its own Docker
                                network, say - can legitimately run without one. Tick
                                <strong> This database has no password</strong> to say so; that
                                is also how a password already saved is REMOVED.
                            </p>
                            <p>
                                With the box unticked, leaving the field blank keeps the stored
                                password - but only while the host, port, database and user are
                                unchanged. Change any of those and blank means blank, so the old
                                credential is never sent to a different machine.
                            </p>
                        </HelpTip>
                    </label>
                    {/* Above the field, because it decides whether the field
                        means anything. Blank alone cannot say "there is
                        none": it already means "keep what is stored", which
                        left a saved password with no way back off. */}
                    <div className="mt-1.5 mb-1.5">
                        <Checkbox
                            checked={!!target.noPassword}
                            onChange={v => set({ noPassword: v, ...(v ? { password: '' } : {}) })}
                            label="This database has no password"
                            hint={value?.passwordSet
                                ? 'One is stored. Ticking this removes it on save.'
                                : 'None is stored.'}
                        />
                    </div>
                    <input
                        className="input-field input-mono w-full"
                        type="password"
                        value={target.password ?? ''}
                        disabled={!!target.noPassword}
                        placeholder={target.noPassword
                            ? 'No password'
                            : value?.passwordSet ? 'Stored - leave blank to keep' : 'Enter a password'}
                        onChange={e => set({ password: e.target.value })}
                        autoComplete="new-password"
                    />
                </div>
                <div>
                    <label className="input-label">SSL mode</label>
                    <div className="mt-1">
                        <Select
                            ariaLabel="SSL mode"
                            value={target.sslMode}
                            onChange={v => set({ sslMode: v })}
                                options={[
                                { value: 'disable', label: 'disable' },
                                { value: 'require', label: 'require' },
                                { value: 'verify-ca', label: 'verify-ca' },
                                { value: 'verify-full', label: 'verify-full' },
                            ]}
                        />
                    </div>
                </div>
            </div>

            <ConnectionTestNote result={test.result} />

            {saveWarning && (
                <p className="flex items-start gap-1.5 text-xs leading-relaxed text-(--warning-light)">
                    <AlertTriangle size={12} className="mt-0.5 shrink-0" />
                    <span>{saveWarning}</span>
                </p>
            )}

            {value?.active && <ActiveLine active={value.active} />}
        </SettingsCard>
    );
}

/**
 * What is being written right now.
 *
 * Separate from the form on purpose: a target that could not be opened leaves
 * the previous one recording, so "what is configured" and "what is running" can
 * legitimately differ, and only one of them is on this screen already.
 */
function ActiveLine({ active }: { active: { recording: boolean; resolution?: string } }) {
    if (!active.recording) {
        return (
            <p className="text-[11px] font-mono text-(--base-06)">
                Not recording: no statistics database is open.
            </p>
        );
    }
    return (
        <p className="text-[11px] font-mono text-(--base-06)">
            Recording now, {active.resolution || 'minute'} buckets.
        </p>
    );
}

function Field({ label, value, onChange, disabled, type = 'text', placeholder }: {
    label: string; value: string; onChange: (v: string) => void;
    disabled?: boolean; type?: string; placeholder?: string;
}) {
    return (
        <div>
            <label className="input-label">{label}</label>
            <input
                className="input-field input-mono w-full mt-1"
                type={type}
                value={value}
                disabled={disabled}
                placeholder={placeholder}
                onChange={e => onChange(e.target.value)}
                autoComplete="off"
            />
        </div>
    );
}
