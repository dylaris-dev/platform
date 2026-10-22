"use client";

import VoicePanel from '@/views/infrastructure/VoicePanel';
import TabGuard from '@/views/infrastructure/TabGuard';
import { useInfra } from '@/views/infrastructure/context';

export default function Page() {
    const { gatewayEnabled } = useInfra();
    if (!gatewayEnabled) {
        return <TabGuard reason="Voice chat runs through the gateway, and the gateway is switched off." />;
    }
    return <VoicePanel />;
}
