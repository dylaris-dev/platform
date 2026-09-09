"use client";

import Link from 'next/link';
import { Users } from 'lucide-react';
import UserManagementTab from '@/components/settings/UserManagementTab';

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
        <div className="flex flex-col h-full min-h-0">
            <div className="flex flex-wrap items-start justify-between gap-3 mb-5">
                <div className="min-w-0">
                    <h2 className="h-section">User settings</h2>
                    <p className="text-sm text-(--base-06)">
                        How accounts behave here: registration, sign-in, password reset, renames and retention.
                    </p>
                </div>
                <Link href="/admin/users" className="btn btn-secondary btn-sm shrink-0">
                    <Users size={13} /> Manage user accounts
                </Link>
            </div>

            <div className="flex-1 min-h-0">
                <UserManagementTab />
            </div>
        </div>
    );
}
