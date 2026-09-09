"use client";

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';

// /settings itself renders nothing; it hands over to the first page in the nav.
// Status, not Modules: someone opening Settings without a specific destination
// in mind is checking whether the platform is healthy, and Modules is a screen
// you go to on purpose, once.
export default function SettingsIndexPage() {
    const router = useRouter();
    useEffect(() => { router.replace('/settings/status'); }, [router]);
    return null;
}
