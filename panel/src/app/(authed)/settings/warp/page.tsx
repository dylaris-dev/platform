"use client";

import WarpTab from '@/components/settings/WarpTab';
import { useAppData } from '@/lib/AppDataContext';
import { MANAGED_GATEWAY_NOTE } from '@/lib/gatewayProvider';

// The nav hides this tab with the managed gateway; the page guards itself
// because it is still one typed address away, and its routes answer 409 there.
export default function SettingsWarpPage() {
    const { gatewayVisibility } = useAppData();
    if (!gatewayVisibility.warpTab) {
        return <div className="card p-8 text-center text-(--base-06) text-sm">{MANAGED_GATEWAY_NOTE}</div>;
    }
    return <WarpTab />;
}
