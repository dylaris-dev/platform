"use client";

import { toast as notifyToast } from '@/components/ui/Toast';

import React, { useCallback, useEffect, useState } from 'react';
import { useRouter } from 'next/navigation';
import { Store, ExternalLink, CircleCheck, CircleAlert, Loader2 } from 'lucide-react';
import { getStoreStatus, startStoreLink, getStoreAccountSummary, type StoreStatus, type StoreAccountSummary } from '@/lib/api/store';
import StoreAccountCard from '@/components/StoreAccountCard';
import { useAppData } from '@/lib/AppDataContext';
import { SkeletonCard } from '@/components/Skeleton';
import PageHeader from '@/components/ui/PageHeader';
import EmptyState from '@/components/ui/EmptyState';

// Connect-store page. Links this panel account to a dylaris.com store account so
// purchases/subscriptions can be tied back to it. The link is authenticated: the
// Core mints a single-use token and redirects to the store, which verifies it
// before persisting. Only present on the hosted (store-enabled) build.
export default function StoreConnectPage() {
    const router = useRouter();
    const { featureFlags } = useAppData();
    const [status, setStatus] = useState<StoreStatus | null>(null);
    const [summary, setSummary] = useState<StoreAccountSummary | null>(null);
    const [loading, setLoading] = useState(true);
    const [redirecting, setRedirecting] = useState(false);


    const showToast = useCallback((msg: string, ok = true) => {
        notifyToast(msg, ok);

    }, []);

    const refresh = useCallback(async () => {
        const s = await getStoreStatus();
        setStatus(s);
        setLoading(false);
        // Only for a linked account: the summary is about a store account, and
        // asking for one that does not exist yet would answer "not linked" a
        // second time in a different shape.
        if (s.linked) setSummary(await getStoreAccountSummary());
    }, []);

    useEffect(() => { refresh(); }, [refresh]);

    // The whole feature is gated; bounce self-host builds back to the dashboard.
    useEffect(() => {
        if (!featureFlags.store) router.replace('/');
    }, [featureFlags.store, router]);

    const handleConnect = async () => {
        setRedirecting(true);
        const res = await startStoreLink();
        if (res.success && res.redirectUrl) {
            window.location.href = res.redirectUrl;
            return;
        }
        setRedirecting(false);
        showToast(res.message || 'Could not start the connect flow', false);
    };

    if (!featureFlags.store) return null;

    return (
        <main className="flex-1 overflow-y-auto app-page space-y-6">
            <PageHeader
                icon={<Store size={16} />}
                title="Dylaris Store"
                description="Link this panel account to your store account for subscriptions and purchases."
            />

            {loading ? (
                <SkeletonCard />
            ) : status && status.success === false ? (
                <EmptyState
                    tone="error"
                    icon={<CircleAlert size={20} />}
                    title="Could not reach the store"
                    description={status.message || 'The store did not answer, so the link state is unknown.'}
                    action={<button onClick={() => { setLoading(true); refresh(); }} className="btn btn-secondary btn-sm">Retry</button>}
                />
            ) : status?.linked ? (
                <div className="srv-surface p-5 space-y-3">
                    <div className="flex items-center gap-2 text-(--success-light)">
                        <CircleCheck size={18} />
                        <span className="font-medium">Linked</span>
                    </div>
                    <p className="text-sm text-(--base-07)">
                        This panel account is linked to your store account
                        {status.email ? <> (<span className="font-mono text-(--base-09)">{status.email}</span>)</> : null}.
                    </p>
                    {status.storeUrl && (
                        <a href={status.storeUrl} target="_blank" rel="noopener noreferrer" className="btn btn-secondary btn-sm inline-flex items-center gap-1.5 w-fit">
                            Open store <ExternalLink size={14} />
                        </a>
                    )}
                </div>
            ) : (
                <div className="srv-surface p-5 space-y-4">
                    <p className="text-sm text-(--base-07)">
                        You will be sent to the Dylaris store to sign in and confirm the link. Your panel account stays in full control; the store only learns which account to attach purchases to.
                    </p>
                    {/* Said here because this is where somebody arrives who has just linked a
                        NEW panel account expecting their servers to follow. They do not: servers,
                        addresses and nodes are owned by the panel account that created them and
                        there is no transfer. Learning that before the link is a decision; learning
                        it after is a support ticket. */}
                    <p className="text-sm text-(--base-06)">
                        Linking attaches purchases to this account. It does not bring servers, protected addresses or nodes over from another panel account, because those stay with the account that created them.
                    </p>
                    <button onClick={handleConnect} disabled={redirecting} className="btn btn-primary inline-flex items-center gap-2 w-fit disabled:opacity-60">
                        {redirecting ? <Loader2 size={16} className="animate-spin" /> : <Store size={16} />}
                        {redirecting ? 'Redirecting…' : 'Connect Store'}
                    </button>
                </div>
            )}

            {status?.linked && (summary
                ? <StoreAccountCard summary={summary} onChanged={refresh} onError={m => showToast(m, false)} />
                : <SkeletonCard />)}


        </main>
    );
}
