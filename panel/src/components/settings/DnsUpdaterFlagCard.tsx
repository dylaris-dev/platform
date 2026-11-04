'use client';

import { Globe } from 'lucide-react';
import SettingsCard from '@/components/settings/SettingsCard';
import SettingsLoadError from '@/components/settings/SettingsLoadError';
import { SwitchRow } from '@/components/ui/Switch';
import { useSettingsForm } from '@/lib/useSettingsForm';
import { getDnsAdmin, saveDnsSettings } from '@/lib/api/serverDomains';

// feature_dns_updater_enabled is not part of the Features payload: Core serves
// and saves it through the DNS settings endpoint, so this card has its own
// load and save instead of joining the platform-flags subset.
export default function DnsUpdaterFlagCard() {
    const form = useSettingsForm<{ enabled: boolean }>({
        load: async () => {
            const r = await getDnsAdmin();
            return r.success ? { enabled: !!r.enabled } : null;
        },
        save: async v => {
            const res = await saveDnsSettings({ enabled: v.enabled });
            return { ok: !!res.success, message: res.message };
        },
        successMessage: 'DNS updater setting saved',
    });
    const v = form.value;

    if (form.loadFailed) return <SettingsLoadError what="the DNS updater setting" onRetry={() => { void form.reload(); }} />;

    return (
        <SettingsCard title="DNS updater (direct mode)" icon={Globe} form={form}
            description="Servers can get their own domain. Core writes the A and SRV records at your DNS provider. Configure accounts under Settings, DNS.">
            {v && (
                <SwitchRow
                    label="DNS updater (direct mode)"
                    description="Only active while game traffic is not routed through the gateway. Off writes nothing to any provider."
                    checked={v.enabled}
                    onChange={o => form.patch({ enabled: o })}
                />
            )}
        </SettingsCard>
    );
}
