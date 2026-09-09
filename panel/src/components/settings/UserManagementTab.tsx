"use client";

import React, { useState } from 'react';
import {
    ShieldCheck, Mail, KeyRound, Trash2, HelpCircle, Plus, X, FileText, UserCog,
    AlertTriangle,
} from 'lucide-react';
import {
    AuthPolicy,
    getAuthPolicy, saveAuthPolicy,
    getDemoAccount, setDemoAccount,
} from '@/lib/api/authSettings';
import { getAdminSecurityQuestionPool, setAdminSecurityQuestionPool } from '@/lib/api/securityQuestions';
import { getAuditPolicy, saveAuditPolicy, AuditPolicy } from '@/lib/api/serverAudit';
import { Skeleton, SkeletonText, SkeletonFormRow } from '@/components/Skeleton';
import { useAppData } from '@/lib/AppDataContext';
import { useSettingsForm, type SettingsForm } from '@/lib/useSettingsForm';
import { useTabParam } from '@/lib/useTabParam';
import { type TabItem } from '@/components/ui/Tabs';
import GuardedTabs from '@/components/settings/GuardedTabs';
import Switch from '@/components/ui/Switch';
import HelpTip from '@/components/ui/HelpTip';
import AccountPolicyCard from '@/components/settings/AccountPolicyCard';
import SettingsCard, { SettingsRow, StatusBadge } from '@/components/settings/SettingsCard';
import { toast } from '@/components/ui/Toast';

type UserTab = 'signin' | 'accounts' | 'retention' | 'questions';

// Kept beside the type so the settings-index test can prove every tab the
// search points at actually exists here.
export const USER_TABS: readonly UserTab[] = ['signin', 'accounts', 'retention', 'questions'];

// Six unrelated cards used to sit stacked down one scroll, each with its own
// Save button and its own idea of what feedback looks like. Tabs are the panel's
// one sub-navigation, and this page is the clearest case for them: nobody
// arrives here wanting to look at registration policy AND audit retention.
export default function UserManagementTab() {
    const { featureFlags } = useAppData();
    const [tab, setTab] = useTabParam<UserTab>(USER_TABS, 'signin');

    // ONE AuthPolicy form, shared by the two tabs that edit it.
    //
    // They used to be two independent cards, each loading and saving the WHOLE
    // AuthPolicy DTO. Whichever saved last wrote its own copy of the other's
    // fields back over them - so changing a 2FA rule and then, later, an
    // auto-delete rule silently reverted the first. Loading it once removes the
    // second copy that made that possible.
    const [uncovered, setUncovered] = useState<{ missing: number; total: number } | null>(null);
    // Whether Core can send mail at all. Core answers it by loading the same
    // transport a real send loads, so this cannot say "configured" about a
    // configuration that would not actually deliver.
    const [mailConfigured, setMailConfigured] = useState<boolean | null>(null);
    const auth = useSettingsForm<AuthPolicy>({
        load: async () => {
            const res = await getAuthPolicy();
            if (!res.success || !res.policy) return null;
            if (typeof res.accountsMissingSecurityQuestions === 'number') {
                setUncovered({ missing: res.accountsMissingSecurityQuestions, total: res.accountsTotal ?? 0 });
            }
            if (typeof res.mailConfigured === 'boolean') setMailConfigured(res.mailConfigured);
            return res.policy as AuthPolicy;
        },
        save: async policy => {
            const res = await saveAuthPolicy(policy);
            return { ok: !!res.success, message: res.message, value: res.policy as AuthPolicy | undefined };
        },
        successMessage: 'Account settings saved',
        // Turning auto-delete on arms a daily job that removes people. Nothing
        // else on this page acts on its own after the tab is closed.
        confirmBeforeSave: (next, prev) =>
            next.inactiveDeleteEnabled && !prev.inactiveDeleteEnabled
                ? {
                      title: 'Start deleting inactive accounts?',
                      message:
                          `A daily job will ${next.deletionMode === 'hard_delete' ? 'permanently delete' : 'anonymise'} ` +
                          `accounts inactive for ${next.inactiveDaysBeforeDelete} days. ` +
                          (next.deleteEmailWarningDays > 0
                              ? `They are warned by email ${next.deleteEmailWarningDays} days first.`
                              : 'They are NOT warned first, because the lead time is 0.'),
                      confirmLabel: 'Turn it on',
                      destructive: next.deletionMode === 'hard_delete',
                  }
                : null,
    });

    const TABS: TabItem<UserTab>[] = [
        { id: 'signin', label: 'Registration & sign-in', icon: ShieldCheck },
        { id: 'accounts', label: 'Accounts', icon: UserCog },
        { id: 'retention', label: 'Retention', icon: Trash2 },
        { id: 'questions', label: 'Security questions', icon: HelpCircle },
    ];

    return (
        <div className="flex flex-col h-full min-h-0">
            <GuardedTabs items={TABS} active={tab} onChange={setTab} ariaLabel="User settings" />

            <div className="flex-1 overflow-y-auto pt-5">
                <div className="space-y-6 max-w-3xl">
                    {tab === 'signin' && (
                        <AuthPolicySection form={auth} uncovered={uncovered} mailConfigured={mailConfigured} />
                    )}
                    {tab === 'accounts' && (
                        <>
                            <AccountPolicyCard />
                            {featureFlags.store && <DemoAccountSection />}
                        </>
                    )}
                    {tab === 'retention' && (
                        <>
                            <AutoDeleteSection form={auth} />
                            <AuditPolicySection />
                        </>
                    )}
                    {tab === 'questions' && <SecurityQuestionsPoolSection />}
                </div>
            </div>
        </div>
    );
}

// ── Demo account section ──────────────────────────────────────────────
// Designates a single read-only account that sees the demo servers and is
// forced GET-only server-side. Create a normal user first, then name it here.
function DemoAccountSection() {
    const form = useSettingsForm<{ username: string }>({
        load: async () => {
            const res = await getDemoAccount();
            return res.success ? { username: res.username || '' } : null;
        },
        save: async v => {
            const res = await setDemoAccount(v.username.trim());
            return {
                ok: !!res.success,
                message: res.message || res.error,
                value: res.success ? { username: res.username || '' } : undefined,
            };
        },
        successMessage: 'Demo account saved',
    });

    return (
        <SettingsCard
            title="Public demo account"
            icon={ShieldCheck}
            form={form}
            description="A single read-only account that sees every server flagged as a demo. It is forced GET-only: it cannot edit, power, create or download anything. Reachable with its own password, or one-click via the public demo session. Leave empty to disable."
        >
            {form.loading || !form.value ? (
                <SkeletonFormRow />
            ) : (
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label" htmlFor="demo-account-username">Demo account username</label>
                    <input
                        id="demo-account-username"
                        type="text"
                        value={form.value.username}
                        onChange={e => form.patch({ username: e.target.value })}
                        placeholder="demo"
                        className="input-field w-full"
                        spellCheck={false}
                    />
                </div>
            )}
        </SettingsCard>
    );
}

// ── Auth policy section (registration + 2FA + reset) ──────────────────

function AuthPolicySection({
    form,
    uncovered,
    mailConfigured,
}: {
    form: SettingsForm<AuthPolicy>;
    uncovered: { missing: number; total: number } | null;
    mailConfigured: boolean | null;
}) {
    const policy = form.value;

    if (form.loading || !policy) {
        return (
            <SettingsCard title="Authentication policy" icon={Mail}>
                <div className="space-y-3">
                    <SkeletonText width="w-1/3" className="h-3.5" />
                    <Skeleton className="h-10 w-full" />
                    <Skeleton className="h-10 w-full" />
                    <Skeleton className="h-10 w-full" />
                    <SkeletonFormRow />
                </div>
            </SettingsCard>
        );
    }

    const set = (patch: Partial<AuthPolicy>) => form.patch(patch);

    return (
        <SettingsCard title="Authentication policy" icon={Mail} form={form}>
            <div className="space-y-4">
                {/* Everything on this card that reaches a person reaches them by
                    mail: the verification link, the reset link, the notice sent
                    before an inactive account is deleted. With no transport
                    configured none of it is sent, and nothing else here would
                    say so - the reset endpoint answers success either way, on
                    purpose, so that it cannot be used to test whether an address
                    has an account. That leaves this screen as the only place an
                    operator can find out. */}
                {mailConfigured === false && (
                    <div className="flex items-start gap-2 bg-(--error-ghost) border border-(--error)/30 rounded-md p-3 text-xs">
                        <AlertTriangle size={14} className="mt-0.5 shrink-0 text-(--error-light)" />
                        <span className="text-(--error-light)">
                            <strong>No mail is configured, so nothing on this card can be delivered.</strong>{' '}
                            Verification links, password resets and inactivity warnings are all
                            silently dropped. Password reset still reports success to the person
                            asking, so nobody but you can notice. Set it up under the{' '}
                            <strong>Email</strong> tab and confirm it with the test send.
                        </span>
                    </div>
                )}
                <h4 className="mono-label text-(--base-07)">Registration</h4>
                <ToggleRow
                    label="Allow self-registration"
                    description="When enabled, anyone with access to the panel can create an account at /register."
                    value={policy.registrationEnabled}
                    onChange={v => set({ registrationEnabled: v })}
                />
                <ToggleRow
                    label="Require email verification"
                    description="New users must click a confirmation link before they can sign in."
                    value={policy.emailVerifyRequired}
                    onChange={v => set({ emailVerifyRequired: v })}
                    help={
                        <>
                            <p className="mb-2">
                                This needs working mail. With nothing configured under the{' '}
                                <strong>Email</strong> tab, the confirmation link is never sent and
                                nobody can finish registering.
                            </p>
                            <p>
                                Existing accounts are unaffected: the requirement applies at
                                registration, not at the next sign-in.
                            </p>
                        </>
                    }
                />
                <ToggleRow
                    label="New users get all-regions access by default"
                    description="Off (default): self-registered users see no servers until an admin grants region access. Admin-created users still default to all-regions."
                    value={policy.defaultNewUserAllRegions}
                    onChange={v => set({ defaultNewUserAllRegions: v })}
                />
                <div className="flex items-center gap-3">
                    <label className="input-label mb-0 shrink-0">Password min length</label>
                    <input
                        type="number"
                        min={4}
                        max={128}
                        value={policy.passwordMinLength}
                        onChange={e => set({ passwordMinLength: Number(e.target.value) || 0 })}
                        className="input-field w-24"
                    />
                    <p className="text-xs text-(--base-06) leading-tight">Enforced on registration and password reset (4–128).</p>
                </div>
            </div>

            <div className="space-y-4 pt-4 border-t border-(--base-03)">
                <h4 className="mono-label text-(--base-07) flex items-center gap-2">
                    <ShieldCheck size={12} /> Two-factor enforcement
                </h4>
                <ToggleRow
                    label="Require 2FA for administrators"
                    description="Admin accounts that haven't enrolled get a short-lived enrollment token at login instead of a full session. They must complete 2FA setup before accessing the panel."
                    value={policy.require2FAForAdmins}
                    onChange={v => set({ require2FAForAdmins: v })}
                />
                <ToggleRow
                    label="Require 2FA for all users"
                    description="Same enforcement as above, applied to every account on the platform — including regular users. Stronger guarantee, more friction at first login. Implies the admin toggle."
                    value={policy.require2FAForAllUsers}
                    onChange={v => set({ require2FAForAllUsers: v })}
                />
                <p className="text-xs text-(--base-06)">
                    Existing sessions remain valid — enforcement triggers at the next login. Backup codes are generated automatically as part of enrollment.
                </p>
            </div>

            <div className="space-y-4 pt-4 border-t border-(--base-03)">
                <h4 className="mono-label text-(--base-07) flex items-center gap-2">
                    <HelpCircle size={12} /> Security questions
                </h4>
                <ToggleRow
                    label="Enable security questions"
                    description="Master toggle. When off, the picker is hidden everywhere — registration, profile, and reset all skip it."
                    value={policy.securityQuestionsEnabled}
                    onChange={v => set({ securityQuestionsEnabled: v })}
                />
                <ToggleRow
                    label="Require at signup"
                    description="New users must pick + answer questions during registration. Existing users keep working without them."
                    value={policy.securityQuestionsRequiredAtSignup}
                    onChange={v => set({ securityQuestionsRequiredAtSignup: v })}
                />
                <ToggleRow
                    label="Require during password reset"
                    description="Reset flow asks for the answers before consuming the email link. Users without questions stored skip this step — otherwise the policy would lock them out."
                    value={policy.securityQuestionsRequiredAtReset}
                    onChange={v => set({ securityQuestionsRequiredAtReset: v })}
                />
                {/* "Users without questions skip this step" and "37 of 40 accounts
                    skip this step" are the same sentence and very different facts.
                    The moment this is switched on, the second one is every account
                    that existed before it. */}
                {policy.securityQuestionsRequiredAtReset && uncovered && uncovered.missing > 0 && (
                    <p className="flex items-start gap-1.5 text-xs text-(--warning-light) -mt-1">
                        <HelpCircle size={12} className="mt-0.5 shrink-0" />
                        <span>
                            {uncovered.missing} of {uncovered.total} accounts have no questions stored,
                            so their password reset still needs nothing but the email link.
                        </span>
                    </p>
                )}
                <div className="flex items-center gap-3">
                    <label className="input-label mb-0 shrink-0">Required answers</label>
                    <input
                        type="number"
                        min={1}
                        max={10}
                        value={policy.securityQuestionsCount}
                        onChange={e => set({ securityQuestionsCount: Number(e.target.value) || 0 })}
                        className="input-field w-24"
                    />
                    <p className="text-xs text-(--base-06) leading-tight">How many questions a user must pick + answer (1–10).</p>
                </div>
            </div>

            <div className="space-y-4 pt-4 border-t border-(--base-03)">
                <h4 className="mono-label text-(--base-07) flex items-center gap-2">
                    <KeyRound size={12} /> Password reset
                </h4>
                <div className="flex items-center gap-3">
                    <label className="input-label mb-0 shrink-0">Reset link lifetime</label>
                    <input
                        type="number"
                        min={5}
                        max={1440}
                        value={policy.passwordResetLinkTTLMinutes}
                        onChange={e => set({ passwordResetLinkTTLMinutes: Number(e.target.value) || 0 })}
                        className="input-field w-24"
                    />
                    <span className="text-sm text-(--base-07)">minutes</span>
                    <p className="text-xs text-(--base-06) leading-tight">Range 5–1440. Shorter is safer if links leak; longer is friendlier for users with slow inbox delivery.</p>
                </div>
                <p className="text-xs text-(--base-06)">
                    The reset link goes out through whatever is configured under the Email tab — the same
                    transport used for verification.
                </p>
            </div>
        </SettingsCard>
    );
}

// ── Small UI helpers ──────────────────────────────────────────────────

function ToggleRow({
    label,
    description,
    value,
    onChange,
    help,
}: {
    label: string;
    description: string;
    value: boolean;
    onChange: (v: boolean) => void;
    help?: React.ReactNode;
}) {
    return (
        <div className="flex items-start justify-between gap-4">
            <div>
                <p className="text-sm font-medium flex items-center gap-1.5">
                    {label}
                    {help && <HelpTip label={`About: ${label}`}>{help}</HelpTip>}
                </p>
                <p className="text-xs text-(--base-06) leading-snug mt-0.5">{description}</p>
            </div>
            <Switch checked={value} onChange={onChange} ariaLabel={label} />
        </div>
    );
}

// ── Auto-delete inactive users section ────────────────────────────────
// Shares the AuthPolicy form with the sign-in tab: it is the same DTO, and two
// copies of it is how one card silently reverted the other.

function AutoDeleteSection({ form }: { form: SettingsForm<AuthPolicy> }) {
    const policy = form.value;
    // The stored master toggle, not the switch on screen: flipping it off and
    // walking away without saving leaves the daily job running.
    const running = !!form.saved?.inactiveDeleteEnabled;

    if (form.loading || !policy) {
        return (
            <SettingsCard title="Auto-delete inactive users" icon={Trash2}>
                <SkeletonText width="w-3/4" />
                <Skeleton className="h-10 w-full" />
            </SettingsCard>
        );
    }

    const set = (patch: Partial<AuthPolicy>) => form.patch(patch);

    return (
        <SettingsCard
            title="Auto-delete inactive users"
            icon={Trash2}
            form={form}
            actions={<StatusBadge active={running} />}
            description="Daily job that warns dormant accounts by email and then removes them. Admins are never affected. Users with server history get an additional grace window. Signing in at any point cancels the scheduled deletion automatically."
        >
            <ToggleRow
                label="Enable auto-delete"
                description="Master toggle. When off, the daily job is a no-op."
                value={policy.inactiveDeleteEnabled}
                onChange={v => set({ inactiveDeleteEnabled: v })}
                help={
                    <>
                        <p className="mb-2">
                            This is the one setting on this page that acts on its own after you close
                            the tab, so it asks before it is switched on.
                        </p>
                        <p>
                            The warning email goes out through the Email tab. With no mail configured
                            the accounts are still deleted, silently and without notice - set the
                            lead time to 0 deliberately, or configure mail first.
                        </p>
                    </>
                }
            />

            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Inactive days before action</label>
                    <input
                        type="number"
                        min={7}
                        max={3650}
                        value={policy.inactiveDaysBeforeDelete}
                        onChange={e => set({ inactiveDaysBeforeDelete: Number(e.target.value) || 0 })}
                        className="input-field w-full"
                    />
                    <p className="text-xs text-(--base-06)">Days since last login (or account creation when never logged in). 7–3650.</p>
                </div>
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">History grace extra days</label>
                    <input
                        type="number"
                        min={0}
                        max={3650}
                        value={policy.historyGraceExtraDays}
                        onChange={e => set({ historyGraceExtraDays: Number(e.target.value) || 0 })}
                        className="input-field w-full"
                    />
                    <p className="text-xs text-(--base-06)">Additional days for accounts with server history (owns or invited).</p>
                </div>
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Warning lead time (days)</label>
                    <input
                        type="number"
                        min={0}
                        max={30}
                        value={policy.deleteEmailWarningDays}
                        onChange={e => set({ deleteEmailWarningDays: Number(e.target.value) || 0 })}
                        className="input-field w-full"
                    />
                    <p className="text-xs text-(--base-06)">Days between warning email and execution. 0 = no warning, immediate.</p>
                </div>
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Deletion mode</label>
                    <select
                        value={policy.deletionMode}
                        onChange={e => set({ deletionMode: e.target.value as 'anonymize' | 'hard_delete' })}
                        className="input-field w-full"
                    >
                        <option value="anonymize">Anonymize (DSGVO-friendly, keeps row + id)</option>
                        <option value="hard_delete">Hard delete (permanent, cascades)</option>
                    </select>
                    <p className="text-xs text-(--base-06)">Anonymize wipes PII but preserves audit references. Hard delete removes the row.</p>
                </div>
            </div>
        </SettingsCard>
    );
}

// ── Audit retention section ───────────────────────────────────────────
// Platform-wide server-audit retention. Per-server audit toggles live on
// each server's Audit tab so admins can flip them in context.

function AuditPolicySection() {
    const form = useSettingsForm<AuditPolicy>({
        load: async () => {
            const res = await getAuditPolicy();
            return res.success && res.policy ? (res.policy as AuditPolicy) : null;
        },
        save: async policy => {
            const res = await saveAuditPolicy(policy);
            return { ok: !!res.success, message: res.message, value: res.policy as AuditPolicy | undefined };
        },
        successMessage: 'Audit retention saved',
    });

    const policy = form.value;
    // The horizon the sweep is actually applying. 0 means it deletes nothing,
    // which is also what an install that never saved this card gets.
    const inForceDays = form.saved?.serverRetentionDays ?? 0;

    if (form.loading || !policy) {
        return (
            <SettingsCard title="Server audit retention" icon={FileText}>
                <SkeletonText width="w-3/4" />
                <Skeleton className="h-9 w-24" />
            </SettingsCard>
        );
    }

    return (
        <SettingsCard
            title="Server audit retention"
            icon={FileText}
            form={form}
            actions={<StatusBadge active={inForceDays > 0} inactiveLabel="Keeping forever" />}
            description="How long server-audit events are kept before the daily sweep deletes them. The sweep only applies a horizon saved here, and 365 is a good one. Per-server audit is auto-enabled on first member invite; admins can force it on from each server's Audit tab."
        >
            <SettingsRow
                label="Server-event retention"
                htmlFor="audit-retention-days"
                description="0 keeps them forever. Range 0 to 3650."
            >
                <input
                    id="audit-retention-days"
                    type="number"
                    min={0}
                    max={3650}
                    value={policy.serverRetentionDays}
                    onChange={e => form.patch({ serverRetentionDays: Number(e.target.value) || 0 })}
                    className="input-field w-24"
                />
                <span className="text-sm text-(--base-07)">days</span>
            </SettingsRow>
        </SettingsCard>
    );
}

// ── Security questions pool section ───────────────────────────────────

function SecurityQuestionsPoolSection() {
    const [newQuestion, setNewQuestion] = useState('');

    const form = useSettingsForm<{ pool: string[] }>({
        load: async () => {
            const res = await getAdminSecurityQuestionPool();
            return res.success && Array.isArray(res.pool) ? { pool: res.pool as string[] } : null;
        },
        save: async v => {
            // Refused here rather than at the server, because the server's own
            // refusal would arrive as a toast on a form that still shows two
            // questions and looks saveable.
            if (v.pool.length < 3) {
                return { ok: false, message: 'The pool needs at least 3 questions.' };
            }
            const res = await setAdminSecurityQuestionPool(v.pool);
            return {
                ok: !!res.success,
                message: res.message,
                value: Array.isArray(res.pool) ? { pool: res.pool as string[] } : undefined,
            };
        },
        successMessage: 'Question pool saved',
    });

    const pool = form.value?.pool ?? [];

    const handleAdd = () => {
        const q = newQuestion.trim();
        if (!q) return;
        if (pool.some(p => p.toLowerCase() === q.toLowerCase())) {
            toast('That question is already in the pool.', false);
            return;
        }
        form.patch({ pool: [...pool, q] });
        setNewQuestion('');
    };

    if (form.loading || !form.value) {
        return (
            <SettingsCard title="Security questions pool" icon={HelpCircle}>
                <SkeletonText width="w-3/4" />
                <div className="space-y-1.5">
                    {Array.from({ length: 5 }).map((_, i) => (
                        <Skeleton key={i} className="h-9 w-full" />
                    ))}
                </div>
            </SettingsCard>
        );
    }

    return (
        <SettingsCard
            title="Security questions pool"
            icon={HelpCircle}
            form={form}
            /* Always in force: the pool is whatever it contains, no state to be wrong about. */
            actions={<StatusBadge active />}
            description="The list of questions users can choose from. Removing a question does not affect users who already picked it: their chosen wording is stored inline."
        >
            <div className="space-y-1.5 max-h-72 overflow-y-auto pr-1">
                {pool.length === 0 && (
                    <p className="text-sm text-(--base-06) italic text-center py-4">Pool is empty. Add some questions below.</p>
                )}
                {pool.map((q, idx) => (
                    <div key={idx} className="flex items-center gap-2 p-2 rounded-md bg-(--base-02) border border-(--base-03)">
                        <span className="font-mono text-xs text-(--base-06) w-6 text-center">{idx + 1}</span>
                        <span className="flex-1 text-sm text-(--base-09)">{q}</span>
                        <button
                            type="button"
                            onClick={() => form.patch({ pool: pool.filter((_, i) => i !== idx) })}
                            aria-label={`Remove question ${idx + 1}`}
                            className="p-1.5 rounded text-(--error-light) hover:bg-(--error-ghost)"
                        >
                            <X size={13} />
                        </button>
                    </div>
                ))}
            </div>

            <div className="flex gap-2 items-stretch">
                <input
                    type="text"
                    value={newQuestion}
                    onChange={e => setNewQuestion(e.target.value)}
                    onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); handleAdd(); } }}
                    placeholder="Add a new question…"
                    maxLength={200}
                    className="input-field flex-1"
                />
                <button
                    type="button"
                    onClick={handleAdd}
                    disabled={!newQuestion.trim()}
                    className="btn btn-secondary inline-flex items-center gap-1.5 disabled:opacity-40"
                >
                    <Plus size={14} /> Add
                </button>
            </div>
            <p className="text-xs text-(--base-06)">
                Adding and removing are edits like any other here: nothing is written until this
                card is saved.
            </p>
        </SettingsCard>
    );
}
