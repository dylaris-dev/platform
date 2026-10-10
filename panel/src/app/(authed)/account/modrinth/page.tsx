"use client";

import React, { useCallback, useEffect, useState } from 'react';
import { Package, Key, ExternalLink, CircleCheck, Trash2, AlertTriangle, EyeOff, Eye } from 'lucide-react';
import {
    getModrinthPATStatus, setModrinthPAT, clearModrinthPAT,
    type ModrinthPATStatus,
} from '@/lib/api/modrinthPat';
import { useAppData } from '@/lib/AppDataContext';
import { SkeletonCard, SkeletonHeader } from '@/components/Skeleton';
import { toast } from '@/components/ui/Toast';

// Modrinth account integration. Lets the user attach a Personal
// Access Token so the platform can publish modpacks on their behalf. Token
// validation hits Modrinth's /v2/user once; ciphertext is stored AES-GCM
// encrypted in the DB. Plaintext is never echoed back from any read.

const REQUIRED_SCOPES = [
    'USER_READ',
    'PROJECT_CREATE',
    'PROJECT_WRITE',
    'VERSION_CREATE',
    'VERSION_WRITE',
];

export default function ModrinthIntegrationPage() {
    // The STATUS read is deliberately allowed while modpacks are off (existing
    // packs stay readable), so a successful load says nothing about whether a
    // token can be SAVED - that write is 503 until modpacks are on. Without the
    // flag this page showed a fully working connect form that only failed on submit.
    const { featureFlags } = useAppData();
    const modpacksDisabled = !featureFlags.modpacks;
    const [status, setStatus] = useState<ModrinthPATStatus | null>(null);
    const [loading, setLoading] = useState(true);
    const [token, setToken] = useState('');
    const [showToken, setShowToken] = useState(false);
    const [saving, setSaving] = useState(false);

    const showToast = (msg: string, ok = true) => toast(msg, ok);

    const refresh = useCallback(async () => {
        const s = await getModrinthPATStatus();
        setStatus(s);
        setLoading(false);
    }, []);

    useEffect(() => { refresh(); }, [refresh]);

    const handleSave = async () => {
        if (!token.trim()) { showToast('Token required', false); return; }
        setSaving(true);
        const res = await setModrinthPAT(token.trim());
        setSaving(false);
        if (res.success && res.connected) {
            setToken('');
            setStatus(res);
            showToast(res.message || 'Connected.', true);
        } else {
            showToast(res.message || 'Token rejected', false);
        }
    };

    const handleClear = async () => {
        const res = await clearModrinthPAT();
        if (res.success) {
            setStatus({ success: true, connected: false });
            showToast('Disconnected.', true);
        } else {
            showToast(res.message || 'Failed to clear', false);
        }
    };

    return (
        <main className="flex-1 overflow-y-auto p-6 max-w-3xl">
            <header className="flex items-center gap-3 mb-4">
                <Package size={20} className="text-(--accent-light)" />
                <h1 className="text-base font-display font-semibold text-(--base-09)">Modrinth Integration</h1>
            </header>

            {modpacksDisabled && (
                <div className="card p-4 mb-4 text-xs flex items-start gap-2">
                    <AlertTriangle size={14} className="text-(--warning-light) shrink-0 mt-0.5" />
                    <div>
                        <p className="text-(--base-08)">Modpacks are turned off on this platform.</p>
                        <p className="mt-1 text-(--base-06)">
                            A Modrinth token is only used to publish packs you author here, so it
                            cannot be connected or changed right now. An admin can re-enable
                            modpacks under Settings &rarr; Features.
                        </p>
                    </div>
                </div>
            )}

            <div className="card p-4 mb-4 text-xs text-(--base-07) flex items-start gap-2">
                <Key size={14} className="text-(--accent-light) shrink-0 mt-0.5" />
                <div>
                    Required to publish modpacks you author in Dylaris to your Modrinth account.
                    Generate a Personal Access Token in your Modrinth settings, paste it below.
                    Token is stored encrypted (AES-256-GCM) and validated against Modrinth&apos;s API
                    once on save — never returned by any read endpoint.
                </div>
            </div>

            {loading ? (
                <div className="space-y-3">
                    <SkeletonHeader />
                    <SkeletonCard height="h-32" />
                </div>
            ) : status?.connected ? (
                <section className="card p-5 space-y-4">
                    <header className="flex items-start gap-3">
                        <CircleCheck size={18} className="text-(--success-light) shrink-0 mt-0.5" />
                        <div className="min-w-0 flex-1">
                            <h2 className="text-base font-display font-semibold text-(--base-09)">
                                Connected as <code className="font-mono text-(--accent-light)">{status.modrinthUsername}</code>
                            </h2>
                            {status.lastValidatedAt && (
                                <p className="text-xs text-(--base-06) mt-0.5">
                                    Token validated {new Date(status.lastValidatedAt).toLocaleString()}
                                </p>
                            )}
                        </div>
                        <button onClick={handleClear} className="btn btn-secondary btn-sm">
                            <Trash2 size={12} className="text-(--error-light)" />
                            Disconnect
                        </button>
                    </header>
                </section>
            ) : (
                <section className="card p-5 space-y-4">
                    <header className="flex items-start gap-3">
                        <div className="w-9 h-9 rounded-md bg-(--accent-ghost) flex items-center justify-center shrink-0">
                            <Key size={16} className="text-(--accent-light)" />
                        </div>
                        <div className="min-w-0 flex-1">
                            <h2 className="text-base font-display font-semibold text-(--base-09)">Connect a Modrinth account</h2>
                            <p className="text-xs text-(--base-06) mt-0.5">
                                Create a token at{' '}
                                <a href="https://modrinth.com/settings/pats" target="_blank" rel="noopener noreferrer"
                                    className="text-(--accent-light) inline-flex items-center gap-1">
                                    modrinth.com/settings/pats <ExternalLink size={9} />
                                </a>
                                . Required scopes are listed below.
                            </p>
                        </div>
                    </header>

                    <div>
                        <label className="input-label">Required scopes</label>
                        <div className="flex flex-wrap gap-1.5 mt-1">
                            {REQUIRED_SCOPES.map(s => (
                                <code key={s} className="mono-label bg-(--base-03) px-1.5 py-0.5 rounded-sm text-(--base-07)">
                                    {s}
                                </code>
                            ))}
                        </div>
                    </div>

                    <div>
                        <label className="input-label">Personal Access Token</label>
                        <div className="flex items-center gap-2">
                            <input
                                type={showToken ? 'text' : 'password'}
                                value={token}
                                onChange={e => setToken(e.target.value)}
                                placeholder="mrp_…"
                                className="input-field input-mono flex-1"
                                autoComplete="off"
                                spellCheck={false}
                            />
                            <button onClick={() => setShowToken(v => !v)} type="button" className="btn btn-secondary btn-sm">
                                {showToken ? <EyeOff size={12} /> : <Eye size={12} />}
                            </button>
                        </div>
                        <p className="text-xs text-(--base-06) mt-1 flex items-start gap-1.5">
                            <AlertTriangle size={10} className="mt-0.5 shrink-0 text-(--warning-light)" />
                            Save validates by calling Modrinth&apos;s /v2/user with this token.
                            Token is never returned again — back it up in your own secret manager.
                        </p>
                    </div>

                    <div className="flex items-center justify-end">
                        <button
                            onClick={handleSave}
                            title={modpacksDisabled ? 'Modpacks are disabled' : undefined}
                            className="btn btn-primary btn-sm disabled:opacity-40"
                            disabled={saving || modpacksDisabled || !token.trim()}
                        >
                            {saving ? 'Validating…' : 'Connect'}
                        </button>
                    </div>
                </section>
            )}

        </main>
    );
}
