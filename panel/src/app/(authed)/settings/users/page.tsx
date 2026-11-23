"use client";

import Link from 'next/link';
import { Users } from 'lucide-react';
import UserManagementTab from '@/components/settings/UserManagementTab';
import SettingsPage from '@/components/settings/SettingsPage';

// Rules about accounts, not the accounts themselves. The roster moved to
// /admin/users - this page had been both, and a page that is a list of people
// AND a form of policy switches makes the reader work out which half they are
// looking at. Each side links to the other.
//
// The seven sections it used to stack down one scroll - each with its own Save
// button and its own idea of what feedback looks like - are now tabs inside
// UserManagementTab. AccountPolicyCard moved in with them; rendering it here as
// well would have put two copies of one form on one page, each able to overwrite
// the other.
//
// The Email one is no longer among them. It configured the mail TRANSPORT,
// which is not an account setting, and now sits under Settings -> Mailing
// beside the templates it sends.
export default function SettingsUsersPage() {
    return (
        <SettingsPage
            title="User settings"
            description="How accounts behave here: registration, sign-in, password reset, renames and retention."
            width="full"
            actions={
                <Link href="/admin/users" className="btn btn-secondary btn-sm shrink-0">
                    <Users size={13} /> Manage user accounts
                </Link>
            }
        >
            <UserManagementTab />
        </SettingsPage>
    );
}
