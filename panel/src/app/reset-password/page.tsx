"use client";

import React, { useEffect, useRef, useState, Suspense } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { validateResetToken, resetPassword, getRegistrationStatus } from '@/lib/api/registration';
import { CircleCheck, CircleAlert, Loader2, KeyRound, ArrowLeft, HelpCircle } from 'lucide-react';
import { Skeleton, SkeletonHeader, SkeletonFormRow } from '@/components/Skeleton';

type Status = 'validating' | 'invalid' | 'ready' | 'submitting' | 'done';

function ResetPasswordInner() {
    const params = useSearchParams();
    const token = params.get('token') || '';
    const [status, setStatus] = useState<Status>('validating');
    const [username, setUsername] = useState('');
    const [password, setPassword] = useState('');
    const [confirm, setConfirm] = useState('');
    const [minLength, setMinLength] = useState(12);
    const [error, setError] = useState('');
    const triggered = useRef(false);
    // security questions integration. Questions and answers
    // live alongside the password fields; the page submits everything in
    // one go and the backend verifies before consuming the token.
    const [questions, setQuestions] = useState<string[]>([]);
    const [answers, setAnswers] = useState<string[]>([]);

    useEffect(() => {
        if (triggered.current) return; // Strict-mode double-invoke guard.
        triggered.current = true;

        if (!token) {
            setStatus('invalid');
            return;
        }

        // Probe registration-status alongside validate so we have the
        // policy-driven password-min-length before the form renders. Both
        // are public — no auth header needed.
        Promise.all([validateResetToken(token), getRegistrationStatus()])
            .then(([validRes, statusRes]) => {
                if (statusRes.success && statusRes.passwordMinLength) {
                    setMinLength(statusRes.passwordMinLength);
                }
                if (validRes.success && validRes.valid) {
                    setUsername(validRes.username || '');
                    if (validRes.requiresSecurityQuestions && Array.isArray(validRes.questions)) {
                        setQuestions(validRes.questions);
                        setAnswers(new Array(validRes.questions.length).fill(''));
                    }
                    setStatus('ready');
                } else {
                    setStatus('invalid');
                }
            })
            .catch(() => setStatus('invalid'));
    }, [token]);

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        setError('');
        if (password !== confirm) {
            setError('Passwords do not match.');
            return;
        }
        if (password.length < minLength) {
            setError(`Password must be at least ${minLength} characters.`);
            return;
        }
        if (questions.length > 0 && answers.some(a => !a.trim())) {
            setError('Please answer all security questions.');
            return;
        }
        setStatus('submitting');
        const res = await resetPassword(token, password, questions.length > 0 ? answers : undefined);
        if (res.success) {
            setStatus('done');
        } else {
            // 401 here = security answers wrong. Backend's message field
            // already says what's up; just surface it.
            setError(res.message || 'Reset failed. Request a new link from the login page.');
            setStatus('ready');
        }
    };

    if (status === 'validating') {
        return (
            <Shell>
                <Loader2 size={28} className="mx-auto animate-spin text-(--accent-light)" />
                <h1 className="font-display text-xl">Checking your reset link…</h1>
            </Shell>
        );
    }

    if (status === 'invalid') {
        return (
            <Shell>
                <CircleAlert size={32} className="mx-auto text-(--error-light)" />
                <h1 className="font-display text-xl">Link is invalid or expired</h1>
                <p className="text-sm text-(--base-07)">
                    Password reset links can only be used once and expire after a short window. Please request a new one.
                </p>
                <Link href="/forgot-password" className="btn btn-primary w-full">Request a new link</Link>
                <Link href="/login" className="flex items-center justify-center gap-1.5 w-full text-xs text-(--base-06) hover:text-(--base-08) transition-colors">
                    <ArrowLeft size={12} /> Back to login
                </Link>
            </Shell>
        );
    }

    if (status === 'done') {
        return (
            <Shell>
                <CircleCheck size={32} className="mx-auto text-(--success-light)" />
                <h1 className="font-display text-xl">Password updated</h1>
                <p className="text-sm text-(--base-07)">
                    {username
                        ? <>You can now sign in as <span className="font-mono text-(--base-09)">{username}</span> with the new password.</>
                        : 'You can now sign in with the new password.'}
                </p>
                <Link href="/login" className="btn btn-primary w-full">Go to login</Link>
            </Shell>
        );
    }

    return (
        <Shell>
            <KeyRound size={28} className="mx-auto text-(--accent-light)" />
            <h1 className="font-display text-xl">Choose a new password</h1>
            {username && (
                <p className="text-sm text-(--base-07)">
                    Resetting password for <span className="font-mono text-(--base-09)">{username}</span>
                </p>
            )}

            {error && (
                <div className="alert alert-error text-sm text-center font-medium">{error}</div>
            )}

            <form onSubmit={handleSubmit} className="space-y-4 text-left">
                {questions.length > 0 && (
                    <div className="space-y-2 pb-3 border-b border-(--base-03)">
                        <div className="flex items-center gap-2 text-(--accent-light)">
                            <HelpCircle size={14} />
                            <span className="text-sm font-medium">Security questions</span>
                        </div>
                        <p className="text-xs text-(--base-06)">
                            Answer the questions you set up earlier. Answers are case-insensitive.
                        </p>
                        {questions.map((q, idx) => (
                            <div key={idx} className="flex flex-col gap-[5px]">
                                <label className="srv-label">{q}</label>
                                <input
                                    type="text"
                                    value={answers[idx] || ''}
                                    onChange={e => {
                                        const next = [...answers];
                                        next[idx] = e.target.value;
                                        setAnswers(next);
                                    }}
                                    autoComplete="off"
                                    className="input-field w-full"
                                    disabled={status === 'submitting'}
                                    required
                                />
                            </div>
                        ))}
                    </div>
                )}
                <div className="flex flex-col gap-[5px]">
                    <label className="srv-label">New password</label>
                    <input
                        required
                        type="password"
                        value={password}
                        onChange={e => setPassword(e.target.value)}
                        autoComplete="new-password"
                        minLength={minLength}
                        className="input-field w-full"
                        disabled={status === 'submitting'}
                        autoFocus={questions.length === 0}
                    />
                    <p className="text-xs text-(--base-06)">At least {minLength} characters.</p>
                </div>
                <div className="flex flex-col gap-[5px]">
                    <label className="srv-label">Confirm new password</label>
                    <input
                        required
                        type="password"
                        value={confirm}
                        onChange={e => setConfirm(e.target.value)}
                        autoComplete="new-password"
                        className="input-field w-full"
                        disabled={status === 'submitting'}
                    />
                </div>
                <button
                    type="submit"
                    disabled={status === 'submitting'}
                    className="btn btn-primary btn-lg w-full inline-flex items-center justify-center gap-2"
                >
                    {status === 'submitting' && <Loader2 size={14} className="animate-spin" />}
                    {status === 'submitting' ? 'Updating…' : 'Reset password'}
                </button>
            </form>
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

export default function ResetPasswordPage() {
    return (
        <Suspense fallback={
            <div className="app-auth">
                <div className="app-auth-card space-y-4">
                    <SkeletonHeader />
                    <SkeletonFormRow />
                    <SkeletonFormRow />
                    <Skeleton className="h-10 w-full mt-2" />
                </div>
            </div>
        }>
            <ResetPasswordInner />
        </Suspense>
    );
}
