"use client";

import React, { useEffect, useState } from 'react';
import { useRouter } from 'next/navigation';
import OtpInput from '@/components/ui/OtpInput';
import Link from 'next/link';
import { setupTOTPWithToken, verifyTOTPWithToken } from '@/lib/api/auth';
import { ShieldCheck, CircleCheck, Copy, Loader2, ArrowLeft } from 'lucide-react';

type Step = 'setup' | 'verify' | 'codes' | 'expired';

export default function ForcedSetup2FAPage() {
    const router = useRouter();
    const [step, setStep] = useState<Step>('setup');
    const [setupToken, setSetupToken] = useState('');
    const [secret, setSecret] = useState('');
    const [otpAuthURL, setOtpAuthURL] = useState('');
    const [account, setAccount] = useState('');
    const [code, setCode] = useState('');
    const [backupCodes, setBackupCodes] = useState<string[]>([]);
    const [error, setError] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [copied, setCopied] = useState(false);

    useEffect(() => {
        const t = sessionStorage.getItem('setupToken');
        const exp = Number(sessionStorage.getItem('setupTokenExpires') || '0');
        if (!t) {
            // No setup token — user landed here directly without going through
            // the login gate. Bounce them back.
            router.replace('/login');
            return;
        }
        if (exp && Date.now() / 1000 > exp) {
            setStep('expired');
            return;
        }
        setSetupToken(t);

        setupTOTPWithToken(t).then(res => {
            if (res.success) {
                setSecret(res.secret);
                setOtpAuthURL(res.otpAuthURL);
                setAccount(res.account || '');
            } else {
                setError(res.message || 'Could not start 2FA setup.');
                if ((res.message || '').toLowerCase().includes('token')) {
                    setStep('expired');
                }
            }
        });
    }, [router]);

    const handleVerify = async (e: React.FormEvent) => {
        e.preventDefault();
        setError('');
        setSubmitting(true);
        const res = await verifyTOTPWithToken(setupToken, secret, code.trim());
        setSubmitting(false);
        if (res.success) {
            setBackupCodes(res.backupCodes || []);
            sessionStorage.removeItem('setupToken');
            sessionStorage.removeItem('setupTokenExpires');
            setStep('codes');
        } else {
            setError(res.message || 'Verification failed.');
        }
    };

    const copyAll = async () => {
        try {
            await navigator.clipboard.writeText(backupCodes.join('\n'));
            setCopied(true);
            setTimeout(() => setCopied(false), 2000);
        } catch {
            /* clipboard blocked — user can copy manually */
        }
    };

    if (step === 'expired') {
        return (
            <Shell>
                <ShieldCheck size={32} className="mx-auto text-(--error-light)" />
                <h1 className="font-display text-xl">Setup session expired</h1>
                <p className="text-sm text-(--base-07)">
                    The 2FA enrollment token only lasts 15 minutes. Please sign in again to get a fresh setup link.
                </p>
                <Link href="/login" className="btn btn-primary w-full">Back to login</Link>
            </Shell>
        );
    }

    if (step === 'codes') {
        return (
            <Shell>
                <CircleCheck size={32} className="mx-auto text-(--success-light)" />
                <h1 className="font-display text-xl">Save your backup codes</h1>
                <p className="text-sm text-(--base-07)">
                    Store these one-time codes somewhere safe — a password manager is ideal. Each works exactly once
                    if you lose access to your authenticator app. They will <strong>never be shown again</strong>.
                </p>
                <div className="rounded-md bg-(--base-02) border border-(--base-03) p-3 font-mono text-sm grid grid-cols-2 gap-1.5 text-left">
                    {backupCodes.map(c => (
                        <code key={c} className="text-(--base-09)">{c}</code>
                    ))}
                </div>
                <button
                    type="button"
                    onClick={copyAll}
                    className="btn btn-secondary w-full inline-flex items-center justify-center gap-2"
                >
                    <Copy size={14} />
                    {copied ? 'Copied!' : 'Copy all to clipboard'}
                </button>
                <Link href="/login" className="btn btn-primary w-full">
                    Done — sign in
                </Link>
            </Shell>
        );
    }

    return (
        <Shell>
            <div className="flex items-center gap-2 justify-center text-(--accent-light)">
                <ShieldCheck size={18} />
                <span className="font-medium">Two-factor setup required</span>
            </div>
            <p className="text-sm text-(--base-07)">
                {account
                    ? <>Hi <span className="font-mono">{account}</span> — this account requires two-factor authentication. Add the secret below to your authenticator app (Google Authenticator, Aegis, 1Password, etc.) and enter the 6-digit code to finish enrollment.</>
                    : <>This account requires two-factor authentication. Add the secret below to your authenticator app and enter the 6-digit code to finish enrollment.</>}
            </p>

            {error && (
                <div className="alert alert-error text-sm text-center font-medium">{error}</div>
            )}

            {secret ? (
                <>
                    <div className="space-y-2 text-left">
                        <p className="text-xs font-mono uppercase tracking-[0.08em] text-(--base-06)">Secret (Base32)</p>
                        <code className="block bg-(--base-02) border border-(--base-03) rounded-md p-3 text-sm font-mono break-all text-(--base-09)">
                            {secret}
                        </code>
                        {otpAuthURL && (
                            <p className="text-xs text-(--base-06)">
                                Or scan: <a href={otpAuthURL} className="text-(--accent-light) underline">otpauth link</a> (most apps support copying it from a QR-only field).
                            </p>
                        )}
                    </div>

                    <form onSubmit={handleVerify} className="space-y-3 mt-2">
                        <div className="flex flex-col gap-[5px] text-left">
                            <label htmlFor="setup-2fa-code" className="srv-label">6-digit code from your app</label>
                            <OtpInput id="setup-2fa-code" value={code} onChange={setCode} />
                        </div>
                        <button
                            type="submit"
                            disabled={submitting || code.trim().length < 6}
                            className="btn btn-primary btn-lg w-full inline-flex items-center justify-center gap-2"
                        >
                            {submitting && <Loader2 size={14} className="animate-spin" />}
                            {submitting ? 'Verifying…' : 'Verify & finish'}
                        </button>
                    </form>
                </>
            ) : (
                <Loader2 size={20} className="animate-spin mx-auto text-(--accent-light)" />
            )}

            <Link href="/login" className="flex items-center justify-center gap-1.5 w-full text-xs text-(--base-06) hover:text-(--base-08) transition-colors mt-2">
                <ArrowLeft size={12} /> Cancel and go back
            </Link>
        </Shell>
    );
}

function Shell({ children }: { children: React.ReactNode }) {
    return (
        <div className="app-auth">
            <div className="app-auth-card text-center space-y-4">{children}</div>
        </div>
    );
}
