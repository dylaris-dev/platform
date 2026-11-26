"use client";

import { useState, useEffect } from 'react';
import { FolderOpen } from 'lucide-react';
import { getFileManagerSettings, saveFileManagerSettings, FileManagerSettings } from '@/lib/api';
import { useSettingsForm } from '@/lib/useSettingsForm';
import SettingsPage from '@/components/settings/SettingsPage';
import SettingsCard, { SettingsGroup } from '@/components/settings/SettingsCard';
import Select from '@/components/ui/Select';
import Switch from '@/components/ui/Switch';

const UNITS = [
    { label: 'MB', multiplier: 1024 * 1024 },
    { label: 'GB', multiplier: 1024 * 1024 * 1024 },
];

const DEFAULTS: FileManagerSettings = {
    adminUploadLimit: 2 * 1024 * 1024 * 1024,
    adminDownloadLimit: 5 * 1024 * 1024 * 1024,
    userUploadLimit: 500 * 1024 * 1024,
    userDownloadLimit: 1 * 1024 * 1024 * 1024,
};

function bytesToDisplay(bytes: number): { value: number; unit: string } {
    if (bytes >= 1024 * 1024 * 1024 && bytes % (1024 * 1024 * 1024) === 0) {
        return { value: bytes / (1024 * 1024 * 1024), unit: 'GB' };
    }
    return { value: Math.round(bytes / (1024 * 1024)), unit: 'MB' };
}

function displayToBytes(value: number, unit: string): number {
    const u = UNITS.find(u => u.label === unit);
    return value * (u?.multiplier || 1);
}

interface LimitFieldProps {
    id: string;
    label: string;
    /** null = no limit; 0 = none (transfers refused); n = the cap in bytes. */
    bytes: number | null;
    onChange: (bytes: number | null) => void;
}

/**
 * A byte ceiling with a unit picker, on the platform limit convention.
 *
 * Not the shared LimitField: these are sizes, so the unit picker is the whole
 * reason a local control exists. It carries the same three states, which it did
 * not before - the number had min={1}, so neither "none" nor "no limit" was
 * reachable, and the enforcement site read a stored 0 as "use the default".
 */
function LimitField({ id, label, bytes, onChange }: LimitFieldProps) {
    const display = bytesToDisplay(bytes ?? 0);
    const [value, setValue] = useState(display.value);
    const [unit, setUnit] = useState(display.unit);

    useEffect(() => {
        // Only track a real cap. Re-reading while unlimited would reset the
        // remembered number to 0, so switching the limit back on would land on
        // "none" instead of the value the operator last had.
        if (bytes === null) return;
        const d = bytesToDisplay(bytes);
        setValue(d.value);
        setUnit(d.unit);
    }, [bytes]);

    const unlimited = bytes === null;

    const handleValueChange = (v: number) => {
        setValue(v);
        onChange(displayToBytes(v, unit));
    };

    const handleUnitChange = (u: string) => {
        setUnit(u);
        onChange(displayToBytes(value, u));
    };

    return (
        <div className="flex flex-col gap-[5px]">
            <label className="input-label" htmlFor={id}>{label}</label>
            <div className="flex gap-2 items-center">
                {!unlimited && (
                    <>
                        <input
                            id={id}
                            type="number"
                            min={0}
                            value={value}
                            onChange={e => handleValueChange(Math.max(0, parseInt(e.target.value) || 0))}
                            className="input-field w-24 text-right"
                        />
                        <Select
                            value={unit}
                            onChange={handleUnitChange}
                            ariaLabel={`${label} unit`}
                            className="w-20"
                            options={UNITS.map(u => ({ value: u.label, label: u.label }))}
                        />
                    </>
                )}
                <label className="flex items-center gap-1.5 cursor-pointer select-none">
                    <Switch
                        ariaLabel={`${label}: no limit`}
                        checked={unlimited}
                        onChange={() => onChange(unlimited ? displayToBytes(value || 1, unit) : null)}
                    />
                    <span className="text-xs text-(--base-06)">No limit</span>
                </label>
            </div>
        </div>
    );
}

export default function FileManagerTab() {
    const form = useSettingsForm<FileManagerSettings>({
        load: async () => {
            const res = await getFileManagerSettings();
            return res.success && res.settings ? res.settings : null;
        },
        save: async value => {
            const res = await saveFileManagerSettings(value);
            return { ok: res.success, message: res.message };
        },
        successMessage: 'File manager settings saved.',
    });

    const s = form.value ?? DEFAULTS;
    const set = (key: keyof FileManagerSettings, value: number | null) => form.patch({ [key]: value });

    return (
        <SettingsPage
            title="Transfer limits"
            icon={FolderOpen}
            description="Maximum file sizes for uploads and downloads in the browser file manager only. The Beam desktop app is not governed by these; it enforces its own upload limits."
            width="2xl"
            loading={form.loading}
        >
            <SettingsCard
                title="File manager"
                description="Separate ceilings for admins and regular users."
                form={form}
            >
                <SettingsGroup
                    title="Admin limits"
                    first
                    help={
                        <>
                            <p className="mb-2">
                                The largest single file an admin may move through the browser file
                                manager. Uploads and downloads are capped separately.
                            </p>
                            <p className="mb-2">
                                This is always a ceiling: there is no &quot;unlimited&quot; here and no
                                &quot;none&quot;. The smallest you can set is 1, and a value that never
                                got saved falls back to the built-in default rather than to zero.
                            </p>
                            <p>
                                It governs the browser only. The Beam desktop app has its own upload
                                limits under Beam, and SFTP has none of these at all.
                            </p>
                        </>
                    }
                >
                    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                        <LimitField id="fm-admin-up" label="Upload limit" bytes={s.adminUploadLimit} onChange={v => set('adminUploadLimit', v)} />
                        <LimitField id="fm-admin-down" label="Download limit" bytes={s.adminDownloadLimit} onChange={v => set('adminDownloadLimit', v)} />
                    </div>
                </SettingsGroup>

                <SettingsGroup
                    title="User limits"
                    help={
                        <>
                            <p className="mb-2">
                                The same two ceilings for everyone who is not an admin. They are read
                                per request from the caller&apos;s own role, so a user promoted to admin
                                gets the admin ceiling on their next transfer, with nothing to
                                re-save.
                            </p>
                            <p>
                                Set these below the admin pair, not above: nothing stops you, and the
                                result is a support ticket that reads like a permissions bug.
                            </p>
                        </>
                    }
                >
                    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                        <LimitField id="fm-user-up" label="Upload limit" bytes={s.userUploadLimit} onChange={v => set('userUploadLimit', v)} />
                        <LimitField id="fm-user-down" label="Download limit" bytes={s.userDownloadLimit} onChange={v => set('userDownloadLimit', v)} />
                    </div>
                </SettingsGroup>
            </SettingsCard>
        </SettingsPage>
    );
}
