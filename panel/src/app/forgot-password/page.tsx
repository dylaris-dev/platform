"use client";

import React, { useState } from 'react';
import Link from 'next/link';
import { forgotPassword } from '@/lib/api/registration';
import { MailCheck, ArrowLeft, Loader2 } from 'lucide-react';

export default function ForgotPasswordPage() {
    const [email, setEmail] = useState('');
    const [submitting, setSubmitting] = useState(false);
    const [sent, setSent] = useState(false);
    const [error, setError] = useState('');

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        setError('');
        setSubmitting(true);
        const res = await forgotPassword(email);
        setSubmitting(false);
        // Backend always returns success for enumeration safety. Only the
        // network-failed branch shows an error — the API client surfaces
        // that as success:false with a generic "connection failed" message.
        if (res.success) {
            setSent(true);
        } else {
            setError(res.message || 'Could not contact the server.');
        }
    };

    if (sent) {
        return (
            <div className="app-auth">
                <div className="app-auth-card text-center space-y-4">
                    <MailCheck size={32} className="mx-auto text-(--accent-light)" />
                    <h1 className="font-display text-xl">Check your inbox</h1>
                    <p className="text-sm text-(--base-07)">
                        If <span className="font-mono">{email}</span> is on file, a password reset link is on its way.
                        The link works exactly once and expires shortly.
                    </p>
                    <p className="text-xs text-(--base-06)">
                        Didn&apos;t receive anything within a few minutes? Check your spam folder or contact your administrator.
                    </p>
                    <Link href="/login" className="btn btn-primary w-full inline-flex items-center justify-center gap-1.5">
                        <ArrowLeft size={12} /> Back to login
                    </Link>
                </div>
            </div>
        );
    }

    return (
        <div className="app-auth">
            <div className="app-auth-card">
                <h1 className="text-4xl text-center mb-2 font-logo tracking-widest select-none">
                    <span className="text-(--accent-light)">D</span>
                    <span className="text-(--base-09)">ylaris</span>
                </h1>
                <p className="text-center text-xs text-(--base-06) mb-6">Reset your password</p>

                {error && (
                    <div className="alert alert-error mb-4 justify-center text-center font-medium">{error}</div>
                )}

                <form onSubmit={handleSubmit} className="space-y-4">
                    <div className="flex flex-col gap-[5px]">
                        <label className="srv-label">Email address</label>
                        <input
                            required
                            type="email"
                            value={email}
                            onChange={e => setEmail(e.target.value)}
                            autoComplete="email"
                            autoFocus
                            className="input-field w-full"
                            disabled={submitting}
                            placeholder="you@example.com"
                        />
                        <p className="text-xs text-(--base-06)">
                            We&apos;ll send a reset link to this address if it&apos;s linked to a Dylaris account.
                        </p>
                    </div>
                    <button
                        type="submit"
                        disabled={submitting}
                        className="btn btn-primary btn-lg w-full inline-flex items-center justify-center gap-2"
                    >
                        {submitting && <Loader2 size={14} className="animate-spin" />}
                        {submitting ? 'Sending…' : 'Send reset link'}
                    </button>
                </form>

                <Link href="/login" className="flex items-center justify-center gap-1.5 w-full text-xs text-(--base-06) hover:text-(--base-08) transition-colors mt-6">
                    <ArrowLeft size={12} /> Back to login
                </Link>
            </div>
        </div>
    );
}
