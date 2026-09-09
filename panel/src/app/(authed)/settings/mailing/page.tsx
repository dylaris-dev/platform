"use client";

import MailingTab from '@/components/settings/MailingTab';

// Both halves of mail on one page: how it gets out, and what it says. The
// transport half used to sit under User settings, which was the wrong home -
// six things here send mail and only two of them are about accounts.
export default function SettingsMailingPage() {
    return (
        <div className="flex flex-col h-full min-h-0">
            <div className="mb-5">
                <h2 className="h-section">Mailing</h2>
                <p className="text-sm text-(--base-06)">
                    How the platform sends email, and the wording of every message it sends.
                </p>
            </div>

            <div className="flex-1 min-h-0">
                <MailingTab />
            </div>
        </div>
    );
}
