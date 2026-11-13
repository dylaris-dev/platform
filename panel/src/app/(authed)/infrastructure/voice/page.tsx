"use client";

import VoicePanel from '@/views/infrastructure/VoicePanel';
import TabGuard from '@/views/infrastructure/TabGuard';
import { useInfra } from '@/views/infrastructure/context';
import { useAppData } from '@/lib/AppDataContext';
import { MANAGED_GATEWAY_NOTE } from '@/lib/gatewayProvider';

export default function Page() {
    const { gatewayEnabled } = useInfra();
    const { gatewayVisibility } = useAppData();
    if (!gatewayVisibility.infrastructureGateway) return <TabGuard reason={MANAGED_GATEWAY_NOTE} />;
    if (!gatewayEnabled) {
        return <TabGuard reason="Voice chat runs through the gateway, and the gateway is switched off." />;
    }
    return <VoicePanel />;
}
