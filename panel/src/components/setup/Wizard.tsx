"use client";

import React, { useEffect, useMemo, useState } from 'react';
import { useRouter } from 'next/navigation';
import { QRCodeSVG } from 'qrcode.react';
import { ShieldCheck, ArrowLeft, ArrowRight, CircleAlert, Loader2 } from 'lucide-react';
import {
    getSetupStatus,
    createFirstAdmin,
    type SetupMode,
} from '@/lib/api/setup';
import { setupUiState, type SetupUiState } from '@/lib/setupUiState';
import SetupBackupCodes from './SetupBackupCodes';

type Step = 1 | 2;

export default function SetupWizard() {
    const router = useRouter();
    const [mode, setMode] = useState<SetupMode | null>(null);
    const [adminSecretConfigured, setAdminSecretConfigured] = useState(false);
    // Core's answers, not ours. `open` is computed there by the same gate the
    // create endpoint enforces; needsSecretWarning is Core telling us the door
    // is open and nothing can come through it.
    const [open, setOpen] = useState(false);
    const [needsSecretWarning, setNeedsSecretWarning] = useState(false);
    const [frontendUrl, setFrontendUrl] = useState<string>('');

    const [step, setStep] = useState<Step>(1);
    const [username, setUsername] = useState('');
    const [password, setPassword] = useState('');
    const [passwordRepeat, setPasswordRepeat] = useState('');
    const [adminSecret, setAdminSecret] = useState('');

    // TOTP - secret is generated client-side at mount so the QR code can
    // render without a backend round-trip. Backend re-verifies the code
    // against this secret when the wizard submits.
    const [totpSecret] = useState(() => genBase32(20));
    const [totpCode, setTotpCode] = useState('');
    const [enable2FA, setEnable2FA] = useState(false);

    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [backupCodes, setBackupCodes] = useState<string[] | null>(null);

    useEffect(() => {
        getSetupStatus().then(s => {
            const ui = setupUiState({ mode: s.mode, adminSecretConfigured: s.adminSecretConfigured, open: s.open });
            if (ui === 'complete') {
                router.replace('/login');
                return;
            }
            setMode(s.mode);
            setAdminSecretConfigured(s.adminSecretConfigured);
            setOpen(s.open);
            setNeedsSecretWarning(s.needsSecretWarning);
            setFrontendUrl(s.frontendUrl || '');
        });
    }, [router]);

    const uiState: SetupUiState | null = mode
        ? setupUiState({ mode, adminSecretConfigured, open })
        : null;
    const secretRequired = uiState === 'secret_required';

    const step1Valid =
        username.trim().length >= 3 &&
        /^[a-zA-Z0-9_-]{3,32}$/.test(username.trim()) &&
        password.length >= 8 &&
        password === passwordRepeat &&
        (!secretRequired || adminSecret.trim().length > 0);

    const otpAuthURL = useMemo(() => {
        if (!username) return '';
        const label = encodeURIComponent(`Dylaris:${username.trim()}`);
        return `otpauth://totp/${label}?secret=${totpSecret}&issuer=Dylaris&algorithm=SHA1&digits=6&period=30`;
    }, [username, totpSecret]);

    const submit = async (withTotp: boolean) => {
        setSubmitting(true);
        setError('');
        const res = await createFirstAdmin({
            username: username.trim(),
            password,
            adminSecret: secretRequired ? adminSecret.trim() : undefined,
            totp: withTotp ? { secret: totpSecret, code: totpCode.trim() } : undefined,
        });
        setSubmitting(false);
        if (!res.success) {
            const msg = res.message || res.error || 'Setup failed.';
            setError(msg);
            if (res.error === 'setup_already_complete') {
                setTimeout(() => router.replace('/login'), 2500);
            }
            return;
        }
        // Nothing to store. Core signs the new admin in with the same HttpOnly
        // cookie a login sets, so /servers is reachable straight away - the
        // response still carries a token, and nothing reads it. With 2FA on,
        // the backup codes come first: this is the only time they are shown.
        if (res.backupCodes && res.backupCodes.length > 0) {
            setBackupCodes(res.backupCodes);
            return;
        }
        router.replace('/servers');
    };

    if (backupCodes) {
        return <SetupBackupCodes codes={backupCodes} onDone={() => router.replace('/servers')} />;
    }

    if (!mode || !uiState) {
        return (
            <div className="text-(--base-06) text-sm">Loading setup status...</div>
        );
    }

    if (uiState === 'disabled') {
        return (
            <div className="card p-6 w-full max-w-md">
                <header className="mb-4">
                    <div className="flex items-center gap-2 mb-1">
                        <ShieldCheck size={18} className="text-(--accent-light)" />
                        <h1 className="text-lg font-display text-(--base-09)">Dylaris Setup</h1>
                    </div>
                </header>
                <p className="text-sm text-(--base-07)">
                    Setup is switched off on this instance, which is the normal state once an
                    administrator exists.
                </p>
                <p className="text-xs text-(--base-06) mt-2">
                    To create another administrator, set <code className="font-mono">SETUP=true</code> in
                    Core&apos;s environment together with an{' '}
                    <code className="font-mono">ADMIN_SECRET</code>, restart, and reload this page.
                    Turn it off again afterwards.
                </p>
                <footer className="mt-5 pt-4 border-t border-(--base-03)">
                    <a href="/servers" className="text-xs text-(--accent-light) hover:underline">
                        Back to the panel
                    </a>
                </footer>
            </div>
        );
    }

    if (uiState === 'recovery_closed') {
        return (
            <div className="card p-6 w-full max-w-md">
                <header className="mb-4">
                    <div className="flex items-center gap-2 mb-1">
                        <ShieldCheck size={18} className="text-(--accent-light)" />
                        <h1 className="text-lg font-display text-(--base-09)">Dylaris Setup</h1>
                    </div>
                </header>
                <div className="text-xs text-(--base-06) flex items-start gap-1.5">
                    <CircleAlert size={12} className="mt-0.5 shrink-0" />
                    <span>
                        No admin recovery available. Set the <code className="font-mono">ADMIN_SECRET</code> env
                        and restart Core, then reload.
                    </span>
                </div>
                {frontendUrl && (
                    <footer className="mt-5 pt-4 border-t border-(--base-03) text-[10px] font-mono text-(--base-06)">
                        Platform URL: {frontendUrl}
                    </footer>
                )}
            </div>
        );
    }

    return (
        <div className="card p-6 w-full max-w-md">
            <header className="mb-5">
                <div className="flex items-center gap-2 mb-1">
                    <ShieldCheck size={18} className="text-(--accent-light)" />
                    <h1 className="text-lg font-display text-(--base-09)">Dylaris Setup</h1>
                </div>
                {uiState === 'open' && (
                    <p className="text-xs text-(--base-06)">
                        Fresh install detected. Create the first administrator account to unlock the platform.
                    </p>
                )}
                {secretRequired && (
                    <p className="text-xs text-(--base-06)">
                        Enter the admin secret configured on this Core to create an administrator.
                    </p>
                )}
            </header>

            {needsSecretWarning && (
                <div className="mb-4 flex items-start gap-2 rounded-md border border-(--error)/40 bg-(--error)/10 p-3">
                    <CircleAlert size={14} className="mt-0.5 shrink-0 text-(--error-light)" />
                    <div className="text-xs text-(--error-light)">
                        <div className="font-medium">No admin token is configured.</div>
                        <p className="mt-1 text-(--base-07)">
                            <code className="font-mono">ADMIN_SECRET</code> is unset on this Core. Setup
                            is reachable, but an administrator cannot be created without it unless this
                            is a pristine first install. Set it in Core&apos;s environment and restart.
                        </p>
                    </div>
                </div>
            )}

            {/* Step indicator */}
            <div className="flex items-center gap-2 text-[10px] font-mono uppercase tracking-[0.08em] text-(--base-06) mb-4">
                <span className={step === 1 ? 'text-(--accent-light)' : ''}>1 - Account</span>
                <span>·</span>
                <span className={step === 2 ? 'text-(--accent-light)' : ''}>2 - Two-Factor (optional)</span>
            </div>

            {step === 1 && (
                <div className="space-y-3">
                    <div>
                        <div className="mono-label">Username</div>
                        <input
                            type="text"
                            value={username}
                            onChange={e => setUsername(e.target.value)}
                            placeholder="alice"
                            autoComplete="username"
                            className="input-field w-full"
                        />
                        <div className="text-[10px] text-(--base-06) mt-1">
                            3-32 chars: letters, digits, _ or -
                        </div>
                    </div>

                    <div>
                        <div className="mono-label">Password</div>
                        <input
                            type="password"
                            value={password}
                            onChange={e => setPassword(e.target.value)}
                            autoComplete="new-password"
                            className="input-field w-full"
                        />
                    </div>

                    <div>
                        <div className="mono-label">Repeat password</div>
                        <input
                            type="password"
                            value={passwordRepeat}
                            onChange={e => setPasswordRepeat(e.target.value)}
                            autoComplete="new-password"
                            className="input-field w-full"
                        />
                        {passwordRepeat && password !== passwordRepeat && (
                            <div className="text-[10px] text-(--error-light) mt-1">
                                Passwords do not match.
                            </div>
                        )}
                    </div>

                    {secretRequired && (
                        <div>
                            <div className="mono-label">Admin secret</div>
                            <input
                                type="password"
                                value={adminSecret}
                                onChange={e => setAdminSecret(e.target.value)}
                                placeholder="ADMIN_SECRET from Core's environment"
                                autoComplete="off"
                                className="input-mono w-full font-mono text-xs"
                            />
                            <div className="text-[10px] text-(--base-06) mt-1">
                                The value of the ADMIN_SECRET env configured on this Core.
                            </div>
                        </div>
                    )}

                    {error && (
                        <div className="text-xs text-(--error-light) flex items-start gap-1.5">
                            <CircleAlert size={12} className="mt-0.5 shrink-0" />
                            <span>{error}</span>
                        </div>
                    )}

                    <div className="flex justify-end gap-2 pt-2">
                        <button
                            type="button"
                            onClick={() => submit(false)}
                            disabled={!step1Valid || submitting}
                            className="btn btn-secondary text-xs py-1.5 px-3"
                        >
                            {submitting ? <Loader2 size={12} className="animate-spin" /> : 'Finish without 2FA'}
                        </button>
                        <button
                            type="button"
                            onClick={() => { setStep(2); setEnable2FA(true); }}
                            disabled={!step1Valid || submitting}
                            className="btn btn-primary text-xs py-1.5 px-3 inline-flex items-center gap-1"
                        >
                            Set up 2FA <ArrowRight size={12} />
                        </button>
                    </div>
                </div>
            )}

            {step === 2 && (
                <div className="space-y-3">
                    <p className="text-xs text-(--base-06)">
                        Scan this QR code with your authenticator app, then enter the 6-digit code.
                    </p>
                    <div className="flex justify-center bg-white p-3 rounded">
                        <QRCodeSVG value={otpAuthURL} size={160} />
                    </div>
                    <div className="font-mono text-[10px] text-(--base-06) break-all">
                        Or enter manually: <span className="text-(--base-09)">{totpSecret}</span>
                    </div>
                    <div>
                        <div className="mono-label">6-digit code</div>
                        <input
                            type="text"
                            inputMode="numeric"
                            value={totpCode}
                            onChange={e => setTotpCode(e.target.value.replace(/\D/g, '').slice(0, 6))}
                            placeholder="123456"
                            className="input-mono w-full font-mono"
                            maxLength={6}
                        />
                    </div>
                    {error && (
                        <div className="text-xs text-(--error-light) flex items-start gap-1.5">
                            <CircleAlert size={12} className="mt-0.5 shrink-0" />
                            <span>{error}</span>
                        </div>
                    )}
                    <div className="flex justify-between gap-2 pt-2">
                        <button
                            type="button"
                            onClick={() => { setStep(1); setError(''); }}
                            disabled={submitting}
                            className="btn btn-secondary text-xs py-1.5 px-3 inline-flex items-center gap-1"
                        >
                            <ArrowLeft size={12} /> Back
                        </button>
                        <div className="flex gap-2">
                            <button
                                type="button"
                                onClick={() => submit(false)}
                                disabled={submitting}
                                className="btn btn-secondary text-xs py-1.5 px-3"
                            >
                                Skip for now
                            </button>
                            <button
                                type="button"
                                onClick={() => submit(true)}
                                disabled={submitting || totpCode.length !== 6}
                                className="btn btn-primary text-xs py-1.5 px-3"
                            >
                                {submitting ? <Loader2 size={12} className="animate-spin" /> : 'Enable & Finish'}
                            </button>
                        </div>
                    </div>
                </div>
            )}

            {(frontendUrl || mode === 'complete') && (
                <footer className="mt-5 pt-4 border-t border-(--base-03) space-y-2">
                    {/* This page stays reachable on a finished install because a configured
                        ADMIN_SECRET is the break-glass: it is how an admin is created again
                        when the last one is gone. Without a way back it reads as a dead end
                        to anyone who lands here by typing the URL. */}
                    {mode === 'complete' && (
                        <p className="text-[10px] text-(--base-06)">
                            This platform is already set up. The form above only creates another
                            administrator, and only for whoever holds the admin secret.{' '}
                            <a href="/servers" className="text-(--accent-light) hover:underline">
                                Back to the panel
                            </a>
                        </p>
                    )}
                    {frontendUrl && (
                        <div className="text-[10px] font-mono text-(--base-06)">
                            Platform URL: {frontendUrl}
                        </div>
                    )}
                </footer>
            )}
        </div>
    );
}

function genBase32(byteLen: number): string {
    const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
    const buf = new Uint8Array(byteLen);
    crypto.getRandomValues(buf);
    let out = '';
    for (let i = 0; i < byteLen; i++) out += alphabet[buf[i] % 32];
    return out;
}
