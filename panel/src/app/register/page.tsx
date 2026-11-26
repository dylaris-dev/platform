"use client";

import Select from '@/components/ui/Select';
import React, { useState, useEffect } from 'react';
import Link from 'next/link';
import { getRegistrationStatus, register } from '@/lib/api/registration';
import { isUsername } from '@/lib/validation';
import { MailCheck, ArrowLeft, HelpCircle } from 'lucide-react';
import { Skeleton, SkeletonHeader, SkeletonFormRow } from '@/components/Skeleton';

interface SecurityQA { question: string; answer: string }

export default function RegisterPage() {
    const [username, setUsername] = useState('');
    const [email, setEmail] = useState('');
    const [password, setPassword] = useState('');
    const [confirm, setConfirm] = useState('');
    const [minLength, setMinLength] = useState(12);
    const [emailVerifyRequired, setEmailVerifyRequired] = useState(false);
    const [enabled, setEnabled] = useState<boolean | null>(null);
    const [submitting, setSubmitting] = useState(false);
    const [error, setError] = useState('');
    const [success, setSuccess] = useState<{ verifySent: boolean } | null>(null);

    // security questions step. When required, an extra
    // panel below the password fields renders before submit is allowed.
    const [securityQuestionsRequired, setSecurityQuestionsRequired] = useState(false);
    const [securityQuestionsCount, setSecurityQuestionsCount] = useState(0);
    const [securityQuestionsPool, setSecurityQuestionsPool] = useState<string[]>([]);
    const [securityAnswers, setSecurityAnswers] = useState<SecurityQA[]>([]);

    useEffect(() => {
        getRegistrationStatus().then(res => {
            if (res.success) {
                setEnabled(!!res.registrationEnabled);
                setMinLength(res.passwordMinLength || 12);
                setEmailVerifyRequired(!!res.emailVerifyRequired);
                if (res.securityQuestionsRequired) {
                    setSecurityQuestionsRequired(true);
                    setSecurityQuestionsCount(res.securityQuestionsCount || 3);
                    setSecurityQuestionsPool(res.securityQuestionsPool || []);
                    // Pre-seed N empty rows so the picker renders the right
                    // number of dropdowns from the start.
                    setSecurityAnswers(Array.from({ length: res.securityQuestionsCount || 3 }, () => ({ question: '', answer: '' })));
                }
            } else {
                setEnabled(false);
            }
        }).catch(() => setEnabled(false));
    }, []);

    const handleSubmit = async (e: React.FormEvent) => {
        e.preventDefault();
        setError('');
        if (!isUsername(username)) {
            setError('Username: 3-32 characters, must start with a letter or digit, then letters, digits, . _ or -');
            return;
        }
        if (password !== confirm) {
            setError('Passwords do not match.');
            return;
        }
        if (password.length < minLength) {
            setError(`Password must be at least ${minLength} characters.`);
            return;
        }
        if (securityQuestionsRequired) {
            if (securityAnswers.some(qa => !qa.question || !qa.answer.trim())) {
                setError('Please pick a question and provide an answer for every row.');
                return;
            }
            const picked = securityAnswers.map(qa => qa.question);
            if (new Set(picked).size !== picked.length) {
                setError('Each question may be chosen at most once.');
                return;
            }
        }
        setSubmitting(true);
        const res = await register({
            username,
            email,
            password,
            ...(securityQuestionsRequired ? { securityQuestions: securityAnswers } : {}),
        });
        setSubmitting(false);
        if (res.success) {
            setSuccess({ verifySent: !!res.emailVerifySent });
        } else {
            setError(res.message || 'Registration failed.');
        }
    };

    if (enabled === null) {
        return (
            <div className="app-auth">
                <div className="app-auth-card space-y-4">
                    <SkeletonHeader />
                    <SkeletonFormRow />
                    <SkeletonFormRow />
                    <SkeletonFormRow />
                    <Skeleton className="h-10 w-full mt-2" />
                </div>
            </div>
        );
    }

    if (enabled === false) {
        return (
            <div className="app-auth">
                <div className="app-auth-card text-center space-y-4">
                    <h1 className="font-display text-xl">Registration disabled</h1>
                    <p className="text-sm text-(--base-06)">
                        This Dylaris instance does not accept self-registration. Ask an administrator to create an account for you.
                    </p>
                    <Link href="/login" className="btn btn-secondary inline-flex items-center gap-1.5">
                        <ArrowLeft size={12} /> Back to login
                    </Link>
                </div>
            </div>
        );
    }

    if (success) {
        return (
            <div className="app-auth">
                <div className="app-auth-card text-center space-y-4">
                    <MailCheck size={32} className="mx-auto text-(--success-light)" />
                    <h1 className="font-display text-xl">Account created</h1>
                    {success.verifySent ? (
                        <p className="text-sm text-(--base-07)">
                            We sent a verification link to <span className="font-mono">{email}</span>. Click it to activate your account, then come back to sign in.
                        </p>
                    ) : (
                        <p className="text-sm text-(--base-07)">
                            Your account is ready. You can sign in now.
                        </p>
                    )}
                    <Link href="/login" className="btn btn-primary w-full">Go to login</Link>
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
                <p className="text-center text-xs text-(--base-06) mb-6">Create your account</p>

                {error && (
                    <div className="alert alert-error mb-4 justify-center text-center font-medium">{error}</div>
                )}

                <form onSubmit={handleSubmit} className="space-y-4">
                    <div className="flex flex-col gap-[5px]">
                        <label className="srv-label">Username</label>
                        <input
                            required
                            type="text"
                            value={username}
                            onChange={e => setUsername(e.target.value)}
                            autoComplete="username"
                            minLength={3}
                            maxLength={32}
                            className="input-field w-full"
                            disabled={submitting}
                        />
                    </div>
                    <div className="flex flex-col gap-[5px]">
                        <label className="srv-label">Email</label>
                        <input
                            required
                            type="email"
                            value={email}
                            onChange={e => setEmail(e.target.value)}
                            autoComplete="email"
                            className="input-field w-full"
                            disabled={submitting}
                        />
                        {emailVerifyRequired && (
                            <p className="text-xs text-(--base-06)">A confirmation link will be sent here.</p>
                        )}
                    </div>
                    <div className="flex flex-col gap-[5px]">
                        <label className="srv-label">Password</label>
                        <input
                            required
                            type="password"
                            value={password}
                            onChange={e => setPassword(e.target.value)}
                            autoComplete="new-password"
                            minLength={minLength}
                            className="input-field w-full"
                            disabled={submitting}
                        />
                        <p className="text-xs text-(--base-06)">At least {minLength} characters.</p>
                    </div>
                    <div className="flex flex-col gap-[5px]">
                        <label className="srv-label">Confirm password</label>
                        <input
                            required
                            type="password"
                            value={confirm}
                            onChange={e => setConfirm(e.target.value)}
                            autoComplete="new-password"
                            className="input-field w-full"
                            disabled={submitting}
                        />
                    </div>

                    {securityQuestionsRequired && (
                        <div className="space-y-3 pt-3 border-t border-(--base-03)">
                            <div className="flex items-center gap-2 text-(--accent-light)">
                                <HelpCircle size={14} />
                                <span className="text-sm font-medium">Security questions</span>
                            </div>
                            <p className="text-xs text-(--base-06)">
                                Pick {securityQuestionsCount} question{securityQuestionsCount === 1 ? '' : 's'} and remember your exact answers — they help recover your account if you lose your password.
                            </p>
                            {securityAnswers.map((qa, idx) => {
                                const otherPicks = securityAnswers.filter((_, i) => i !== idx).map(o => o.question);
                                const options = securityQuestionsPool.filter(p => !otherPicks.includes(p));
                                return (
                                    <div key={idx} className="space-y-1.5">
                                        <Select
                                            value={qa.question}
                                            onChange={v => {
                                                const next = [...securityAnswers];
                                                next[idx] = { ...next[idx], question: v };
                                                setSecurityAnswers(next);
                                            }}
                                            options={options.map(q => ({ value: q, label: q }))}
                                            placeholder={`— Pick question #${idx + 1} —`}
                                            ariaLabel={`Security question ${idx + 1}`}
                                            disabled={submitting}
                                        />
                                        <input
                                            type="text"
                                            value={qa.answer}
                                            onChange={e => {
                                                const next = [...securityAnswers];
                                                next[idx] = { ...next[idx], answer: e.target.value };
                                                setSecurityAnswers(next);
                                            }}
                                            placeholder="Your answer"
                                            maxLength={200}
                                            className="input-field w-full"
                                            disabled={submitting || !qa.question}
                                            required
                                        />
                                    </div>
                                );
                            })}
                            <p className="text-xs text-(--base-06)">
                                Answers are case-insensitive and never displayed again. Store them somewhere safe.
                            </p>
                        </div>
                    )}

                    <button
                        type="submit"
                        disabled={submitting}
                        className="btn btn-primary btn-lg w-full"
                    >
                        {submitting ? 'Creating account…' : 'Register'}
                    </button>
                </form>

                <p className="text-xs text-center text-(--base-06) mt-6">
                    Already have an account?{' '}
                    <Link href="/login" className="text-(--accent-light) hover:underline">Sign in</Link>
                </p>
            </div>
        </div>
    );
}
