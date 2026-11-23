"use client";

import Link from 'next/link';
import { Settings } from 'lucide-react';
import UsersTab from '@/components/settings/UsersTab';
import Section from '@/components/ui/Section';
import { useAppData } from '@/lib/AppDataContext';

// The roster lives here rather than under Settings: it is a list of PEOPLE, and
// Settings is where the platform is configured. The rules about how accounts
// behave (registration, password policy, renames) stayed behind, so each page
// answers one question - and links to the other, because whoever is on one
// regularly wants the other next.
export default function AdminUsersPage() {
    const { user } = useAppData();

    return (
        <Section
            title="Users"
            description="Who has an account here, and what they can do."
            actions={
                <Link href="/settings/users" className="btn btn-secondary btn-sm">
                    <Settings size={14} /> User settings
                </Link>
            }
        >
            <UsersTab currentUser={user ?? undefined} />
        </Section>
    );
}
