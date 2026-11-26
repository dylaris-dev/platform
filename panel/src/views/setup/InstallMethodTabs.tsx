"use client";

import Tabs from '@/components/ui/Tabs';
import type { InstallTab } from './installTarget';

/** The install-method switch shared by the new-server wizard and the edit form. */
export default function InstallMethodTabs({ value, onChange, templatesEnabled }: {
    value: InstallTab;
    onChange: (tab: InstallTab) => void;
    templatesEnabled?: boolean;
}) {
    const items: { id: InstallTab; label: string }[] = [
        { id: 'online', label: 'Online' },
        ...(templatesEnabled ? [{ id: 'template' as const, label: 'Template' }] : []),
        { id: 'upload', label: 'Upload / SFTP' },
        { id: 'backup', label: 'Backup' },
        { id: 'modpack', label: 'Modrinth modpacks' },
        { id: 'pack', label: 'My modpacks' },
        { id: 'technic', label: 'Technic' },
    ];
    return <Tabs items={items} active={value} onChange={onChange} ariaLabel="Installation method" className="overflow-x-auto" />;
}
