"use client";

import React, { useEffect, useRef, useState } from 'react';
import Link from 'next/link';
import { useSearchParams } from 'next/navigation';
import { verifyEmail } from '@/lib/api/registration';
import { CircleCheck, CircleAlert, Loader2 } from 'lucide-react';
import { SkeletonHeader, SkeletonCard } from '@/components/Skeleton';

type Status = 'pending' | 'ok' | 'error';

function VerifyEmailInner() {
    const params = useSearchParams();
    const token = params.get('token') || '';
    const [status, setStatus] = useState<Status>('pending');
    const [username, setUsername] = useState('');
    const [message, setMessage] = useState('');
    const triggered = useRef(false);

    useEffect(() => {
        if (triggered.current) return; // Strict-mode double-invoke guard.
        triggered.current = true;

        if (!token) {
            setStatus('error');
            setMessage('No verification token provided. Please use the link from your email.');
            return;
        }
        verifyEmail(token).then(res => {
            if (res.success) {
                setStatus('ok');
                setUsername(res.username || '');
            } else {
                setStatus('error');
                setMessage(res.message || 'Verification failed.');
            }
        }).catch(() => {
            setStatus('error');
            setMessage('Could not contact the server.');
        });
    }, [token]);

    return (
        <div className="app-auth">
            <div className="app-auth-card text-center space-y-4">
                {status === 'pending' && (
                    <>
                        <Loader2 size={32} className="mx-auto animate-spin text-(--accent-light)" />
                        <h1 className="font-display text-xl">Verifying your email…</h1>
                    </>
                )}
                {status === 'ok' && (
                    <>
                        <CircleCheck size={32} className="mx-auto text-(--success-light)" />
                        <h1 className="font-display text-xl">Email verified</h1>
                        <p className="text-sm text-(--base-07)">
                            {username
                                ? <>Welcome, <span className="font-mono text-(--base-09)">{username}</span>! Your account is now active.</>
                                : 'Your account is now active.'}
                        </p>
                        <Link href="/login" className="btn btn-primary w-full">Go to login</Link>
                    </>
                )}
                {status === 'error' && (
                    <>
                        <CircleAlert size={32} className="mx-auto text-(--error-light)" />
                        <h1 className="font-display text-xl">Verification failed</h1>
                        <p className="text-sm text-(--base-07)">{message}</p>
                        <p className="text-xs text-(--base-06)">
                            The link may have expired or been used already. You can request a new one from the login screen.
                        </p>
                        <Link href="/login" className="btn btn-secondary w-full">Back to login</Link>
                    </>
                )}
            </div>
        </div>
    );
}

// Wrapper so useSearchParams renders inside Suspense as Next.js requires.
import { Suspense } from 'react';
export default function VerifyEmailPage() {
    return (
        <Suspense fallback={
            <div className="app-auth">
                <div className="app-auth-card space-y-3">
                    <SkeletonHeader />
                    <SkeletonCard height="h-16" />
                </div>
            </div>
        }>
            <VerifyEmailInner />
        </Suspense>
    );
}
