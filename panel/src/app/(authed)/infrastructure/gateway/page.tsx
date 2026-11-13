"use client";

import GatewayPanel from '@/views/infrastructure/GatewayPanel';
import TabGuard from '@/views/infrastructure/TabGuard';
import { useInfra } from '@/views/infrastructure/context';
import { useAppData } from '@/lib/AppDataContext';
import { MANAGED_GATEWAY_NOTE } from '@/lib/gatewayProvider';

export default function Page() {
    const { gatewayDeployed } = useInfra();
    const { gatewayVisibility } = useAppData();
    if (!gatewayVisibility.infrastructureGateway) return <TabGuard reason={MANAGED_GATEWAY_NOTE} />;
    if (!gatewayDeployed) {
        return <TabGuard reason="No gateway is deployed, so there is nothing to show here. Enable gateway routing and deploy an edge first." />;
    }
    return <GatewayPanel />;
}
