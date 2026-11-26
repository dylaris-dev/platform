'use client';

import { useEffect, useState } from 'react';
import { Database, CircleCheck, CircleAlert } from 'lucide-react';
import SettingsCard, { SettingsGroup, SettingsRow } from '@/components/settings/SettingsCard';
import Switch from '@/components/ui/Switch';
import { useSettingsForm } from '@/lib/useSettingsForm';
import { getModCacheSettings, saveModCacheSettings, type ModCacheSettings } from '@/lib/api/modCache';

/**
 * Where Modrinth metadata is cached.
 *
 * Left empty, it is the Redis this panel already runs on, and that is the right
 * answer for almost every install: Redis is not optional here, so the cache
 * always has a home and nothing has to be configured before mods or modpacks
 * work. The field exists for the operator who would rather their control plane
 * never shared memory with a cache, not as a step everyone has to take.
 */

const DEFAULTS: ModCacheSettings = {
    addr: '',
    username: '',
    db: 0,
    tls: false,
    passwordSet: false,
    status: { dedicated: false, healthy: false },
};

export default function ModCacheCard() {
    const [status, setStatus] = useState(DEFAULTS.status);
    const form = useSettingsForm<ModCacheSettings>({
        load: async () => {
            const s = await getModCacheSettings();
            if (s) setStatus(s.status);
            return s;
        },
        save: async value => {
            const res = await saveModCacheSettings({
                addr: value.addr,
                username: value.username,
                db: value.db,
                tls: value.tls,
                // Blank keeps the stored password; the form never shows it.
                password: value.password || '',
            });
            if (res.success && res.status) setStatus(res.status);
            return { ok: res.success, message: res.message };
        },
    });

    useEffect(() => { form.reload(); }, []);

    const settings = form.value ?? DEFAULTS;
    const patch = form.patch;
    const dedicated = settings.addr.trim() !== '';

    return (
        <SettingsCard
            title="Mod metadata cache"
            icon={Database}
            description="Where Modrinth project and version metadata is kept between requests. It serves both the modpack builder and every server's Content tab."
            form={form}
        >
            <SettingsGroup title="Location" first>
                <div className="flex items-start justify-between gap-4 rounded-lg bg-(--base-03) px-4 py-3">
                    <div className="min-w-0">
                        <div className="text-sm text-(--base-09)">
                            {status.dedicated
                                ? `Using a dedicated Redis${status.addr ? ` at ${status.addr}` : ''}`
                                : 'Using the Redis this panel already runs on'}
                        </div>
                        <div className="text-xs text-(--base-06) mt-0.5">
                            {status.dedicated
                                ? 'If this endpoint stops answering, metadata is fetched fresh every time instead of falling back to the panel Redis, so moving the cache away stays moved.'
                                : 'The default, and the right one unless you specifically want the cache off your control plane. Nothing has to be set here for mods or modpacks to work.'}
                        </div>
                    </div>
                    <span
                        className={`badge shrink-0 inline-flex items-center gap-1 ${status.healthy ? 'badge-success' : 'badge-warning'}`}
                        title={status.error || undefined}
                    >
                        {status.healthy ? <CircleCheck size={11} /> : <CircleAlert size={11} />}
                        {status.healthy ? 'Reachable' : 'Not reachable'}
                    </span>
                </div>

                <SettingsRow
                    label="Redis address"
                    description="host:port. Leave empty to keep using the Redis this panel already runs on."
                    htmlFor="mod-cache-addr"
                >
                    <input
                        id="mod-cache-addr"
                        className="input-field input-mono w-64"
                        placeholder="cache.internal:6379"
                        value={settings.addr}
                        onChange={e => patch({ addr: e.target.value })}
                    />
                </SettingsRow>
            </SettingsGroup>

            {dedicated && (
                <SettingsGroup
                    title="Credentials"
                    description="Only needed when that Redis requires them."
                >
                    <SettingsRow label="Username" description="Leave empty for a Redis without ACL users." htmlFor="mod-cache-user">
                        <input
                            id="mod-cache-user"
                            className="input-field input-mono w-64"
                            value={settings.username}
                            onChange={e => patch({ username: e.target.value })}
                        />
                    </SettingsRow>

                    <SettingsRow
                        label="Password"
                        description={settings.passwordSet
                            ? 'A password is stored. Leave empty to keep it, or type a new one to replace it.'
                            : 'Stored encrypted and never sent back to the browser.'}
                        htmlFor="mod-cache-pass"
                    >
                        <input
                            id="mod-cache-pass"
                            type="password"
                            autoComplete="new-password"
                            className="input-field input-mono w-64"
                            placeholder={settings.passwordSet ? '••••••••' : ''}
                            value={settings.password ?? ''}
                            onChange={e => patch({ password: e.target.value })}
                        />
                    </SettingsRow>

                    <SettingsRow label="Database" description="0 to 15." htmlFor="mod-cache-db">
                        <input
                            id="mod-cache-db"
                            type="number"
                            min={0}
                            max={15}
                            className="input-field w-24"
                            value={settings.db}
                            onChange={e => patch({ db: Math.min(15, Math.max(0, Number(e.target.value) || 0)) })}
                        />
                    </SettingsRow>

                    <SettingsRow label="TLS" description="Connect with TLS.">
                        <Switch
                            checked={settings.tls}
                            onChange={v => patch({ tls: v })}
                            ariaLabel="Connect to the cache Redis with TLS"
                        />
                    </SettingsRow>
                </SettingsGroup>
            )}

            <SettingsGroup title="Why this exists">
                <p className="text-xs text-(--base-06)">
                    A Modrinth version list weighs between 270 KB and 1.2 MB and is kept for an hour per filter
                    combination, so browsing is the only thing here that grows with use rather than with fleet size.
                    Three things bound it: changelogs are stripped before anything is stored, which is about 60 percent
                    of a typical list and nothing displays them; responses still over 512 KB are served through and
                    never stored; and the panel Redis evicts cached entries before anything else when it runs short.
                    Every published build stays available, because installing an older one is the point of the version
                    picker. Moving the cache off this Redis entirely is the stronger version of the same idea.
                </p>
            </SettingsGroup>
        </SettingsCard>
    );
}
