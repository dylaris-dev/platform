"use client";

import MailingTab from '@/components/settings/MailingTab';
import SettingsPage from '@/components/settings/SettingsPage';

// Both halves of mail on one page: how it gets out, and what it says. The
// transport half used to sit under User settings, which was the wrong home -
// six things here send mail and only two of them are about accounts.
export default function SettingsMailingPage() {
    return (
        <SettingsPage
            title="Mailing"
            description="How the platform sends email, and the wording of every message it sends."
            width="full"
        >
            <MailingTab />
        </SettingsPage>
    );
}
