"use client";

import { useState } from 'react';
import { Loader2, Send } from 'lucide-react';
import {
    SMTPConfig, MailProvider,
    getSMTPConfig, saveSMTPConfig, testSendSMTP,
} from '@/lib/api/authSettings';
import { SkeletonFormRow } from '@/components/Skeleton';
import { useSettingsForm } from '@/lib/useSettingsForm';
import { SMTP_TEST_TIMEOUT_MS } from '@/lib/connectionTest';
import HelpTip from '@/components/ui/HelpTip';
import SettingsCard, { StatusBadge } from '@/components/settings/SettingsCard';
import { toast } from '@/components/ui/Toast';

// How the platform HANDS a message over - the SMTP server or the Resend key,
// and the address it sends as. What the messages SAY is the Templates tab
// beside this one. The two never overlapped; they were simply on different
// pages, so an operator setting mail up for the first time had to find both.
//
// Its old home was User settings, which was already the wrong one before the
// move: six things here send mail and only two of them are about accounts.

const ENCRYPTION_OPTIONS = [
    { value: 'starttls', label: 'STARTTLS (port 587)' },
    { value: 'tls', label: 'Implicit TLS (port 465)' },
    { value: 'none', label: 'None — plaintext (private network only)' },
];

export default function MailDeliverySection() {
    const [testTo, setTestTo] = useState('');
    const [testing, setTesting] = useState(false);

    const form = useSettingsForm<SMTPConfig>({
        load: async () => {
            const res = await getSMTPConfig();
            if (!res.success || !res.config) return null;
            const c = res.config as SMTPConfig;
            return { ...c, provider: c.provider || 'smtp', password: '', resendApiKey: '' };
        },
        save: async cfg => {
            const res = await saveSMTPConfig(cfg);
            const stored = res.config as SMTPConfig | undefined;
            return {
                ok: !!res.success,
                message: res.message,
                // Blank the write-only fields again after a save, so the form
                // does not read dirty against a value the server never returns.
                value: stored ? { ...stored, provider: stored.provider || 'smtp', password: '', resendApiKey: '' } : undefined,
            };
        },
        successMessage: 'Email settings saved',
    });

    const cfg = form.value;

    if (form.loading || !cfg) {
        return (
            <SettingsCard title="Outgoing email" icon={Send}>
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                    <SkeletonFormRow />
                    <SkeletonFormRow />
                    <SkeletonFormRow />
                    <SkeletonFormRow />
                </div>
            </SettingsCard>
        );
    }

    const isResend = cfg.provider === 'resend';
    // What the mailer would use RIGHT NOW - the STORED profile, not the form
    // being typed into. A badge that turns green while you type has only moved
    // the lie earlier, which is why this reads form.saved and not cfg.
    const stored = form.saved;
    const sending = !!stored && (
        stored.provider === 'resend'
            ? !!stored.resendApiKeySet && !!stored.fromEmail.trim()
            : !!stored.host.trim() && !!stored.fromEmail.trim()
    );

    const handleTest = async () => {
        setTesting(true);
        // A deadline, because fetch has none: a mail server that accepts the
        // TCP connection and then stalls left this button spinning forever.
        // Longer than the connection tests elsewhere on purpose - this one
        // actually hands a message over, and a slow relay is not a broken one.
        const controller = new AbortController();
        const timer = setTimeout(() => controller.abort(), SMTP_TEST_TIMEOUT_MS);
        try {
            const res = await testSendSMTP(testTo.trim() || undefined, controller.signal);
            toast(res.message || (res.success ? 'Test sent.' : 'Test failed.'), !!res.success);
        } catch {
            toast(`No answer within ${Math.round(SMTP_TEST_TIMEOUT_MS / 1000)} seconds, so the test was ` +
                'stopped. The mail may still be on its way - check the inbox before changing anything.', false);
        } finally {
            clearTimeout(timer);
            setTesting(false);
        }
    };

    return (
        <SettingsCard
            title="Outgoing email"
            icon={Send}
            form={form}
            actions={<StatusBadge active={sending} inactiveLabel="Not configured" />}
            description="Used for email verification, password resets, deletion warnings and billing notices. With nothing configured here, every one of those silently does not arrive."
        >
            <div className="flex flex-col gap-[5px]">
                <label className="input-label flex items-center gap-1.5">
                    Provider
                    <HelpTip label="About the mail providers">
                        <p className="mb-2">
                            <strong>SMTP</strong> talks to a mail server you already have. It is the
                            right answer if you run one, and the wrong one if setting it up means a
                            server, a reverse DNS entry and an SPF record first.
                        </p>
                        <p className="mb-2">
                            <strong>Resend</strong> is an HTTP mail API: verify your domain once in
                            their dashboard, paste an API key here, done. No port 25, no TLS
                            negotiation, and it works from hosts that block outbound SMTP - which
                            many do.
                        </p>
                        <p>
                            The sender address below is shared, so switching does not mean retyping
                            it, and switching back does not lose the other one&apos;s credential.
                        </p>
                    </HelpTip>
                </label>
                <select
                    value={cfg.provider}
                    onChange={e => form.patch({ provider: e.target.value as MailProvider })}
                    className="input-field w-full"
                >
                    <option value="smtp">SMTP server</option>
                    <option value="resend">Resend (API)</option>
                </select>
            </div>

            {isResend ? (
                <div className="flex flex-col gap-[5px]">
                    <label className="input-label">Resend API key</label>
                    <input
                        type="password"
                        value={cfg.resendApiKey || ''}
                        placeholder={cfg.resendApiKeySet ? '(stored — leave blank to keep)' : 're_...'}
                        autoComplete="new-password"
                        onChange={e => form.patch({ resendApiKey: e.target.value })}
                        className="input-field input-mono w-full"
                    />
                    <p className="text-xs text-(--base-06)">
                        From the Resend dashboard under API Keys. Sending permission is enough. The
                        domain in the sender address below has to be verified there, or Resend
                        refuses the message and says so in the test result.
                    </p>
                </div>
            ) : (
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
                    <Field label="Host" value={cfg.host} onChange={v => form.patch({ host: v })} placeholder="smtp.example.com" />
                    <Field label="Port" type="number" value={String(cfg.port || '')} onChange={v => form.patch({ port: Number(v) || 0 })} placeholder="587 / 465 / 25" />
                    <Field label="Username" value={cfg.username} onChange={v => form.patch({ username: v })} autoComplete="off" />
                    <PasswordField
                        label="Password"
                        value={cfg.password || ''}
                        placeholder={cfg.passwordSet ? '(stored — leave blank to keep)' : ''}
                        onChange={v => form.patch({ password: v })}
                    />
                    <div className="flex flex-col gap-[5px] sm:col-span-2">
                        <label className="input-label">Encryption</label>
                        <select
                            value={cfg.encryption || 'starttls'}
                            onChange={e => form.patch({ encryption: e.target.value })}
                            className="input-field w-full"
                        >
                            {ENCRYPTION_OPTIONS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
                        </select>
                    </div>
                </div>
            )}

            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 pt-4 border-t border-(--base-03)">
                <Field label="From email" value={cfg.fromEmail} onChange={v => form.patch({ fromEmail: v })} placeholder="noreply@example.com" />
                <Field label="From name" value={cfg.fromName} onChange={v => form.patch({ fromName: v })} placeholder="Dylaris" />
            </div>

            <div className="flex flex-col sm:flex-row sm:items-center gap-3 pt-2 border-t border-(--base-03)">
                <input
                    type="email"
                    placeholder="Send test to (defaults to your own email)"
                    value={testTo}
                    onChange={e => setTestTo(e.target.value)}
                    className="input-field flex-1"
                />
                <button
                    type="button"
                    onClick={handleTest}
                    disabled={testing || form.dirty}
                    className="btn btn-secondary inline-flex items-center gap-2 disabled:opacity-40"
                >
                    {testing && <Loader2 size={14} className="animate-spin" />}
                    Send test email
                </button>
            </div>
            {/* The test uses what is STORED, not what is on screen. Sending one
                against a half-typed form and reporting the old configuration's
                result is how a broken setup passes its own test. */}
            {form.dirty && (
                <p className="text-xs text-(--base-06)">
                    Save first — the test sends with the stored configuration, not the one on screen.
                </p>
            )}
        </SettingsCard>
    );
}

function Field({ label, value, onChange, type, placeholder, autoComplete }: { label: string; value: string; onChange: (v: string) => void; type?: string; placeholder?: string; autoComplete?: string }) {
    return (
        <div className="flex flex-col gap-[5px]">
            <label className="input-label">{label}</label>
            <input
                type={type || 'text'}
                value={value}
                onChange={e => onChange(e.target.value)}
                placeholder={placeholder}
                autoComplete={autoComplete}
                className="input-field w-full"
            />
        </div>
    );
}

function PasswordField({ label, value, onChange, placeholder }: { label: string; value: string; onChange: (v: string) => void; placeholder?: string }) {
    return (
        <div className="flex flex-col gap-[5px]">
            <label className="input-label">{label}</label>
            <input
                type="password"
                value={value}
                onChange={e => onChange(e.target.value)}
                placeholder={placeholder}
                autoComplete="new-password"
                className="input-field w-full"
            />
        </div>
    );
}
