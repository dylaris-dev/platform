"use client";

import { useState } from 'react';
import { ShieldCheck, Copy, Check, AlertTriangle } from 'lucide-react';

// Shown once after the wizard created an admin with 2FA: these codes are the
// only way back in without the authenticator, and Core never shows them again.
export default function SetupBackupCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
    const [copied, setCopied] = useState(false);
    const [acknowledged, setAcknowledged] = useState(false);

    const copyAll = async () => {
        try {
            await navigator.clipboard.writeText(codes.join('\n'));
            setCopied(true);
            setTimeout(() => setCopied(false), 1500);
        } catch { /* clipboard refused; the codes stay selectable */ }
    };

    return (
        <div className="card p-6 w-full max-w-md">
            <header className="mb-4">
                <div className="flex items-center gap-2 mb-1">
                    <ShieldCheck size={18} className="text-(--accent-light)" />
                    <h1 className="text-lg font-display text-(--base-09)">Backup codes</h1>
                </div>
                <p className="text-xs text-(--base-06)">
                    Your administrator account is ready and two-factor sign-in is on.
                </p>
            </header>
            <div className="space-y-4">
                <div className="alert alert-warning text-(--warning) text-xs">
                    <AlertTriangle size={14} className="shrink-0 mt-0.5" />
                    <span>
                        Store these codes somewhere safe. Each one works once and signs you in if you lose
                        your authenticator. They will never be shown again.
                    </span>
                </div>
                <div className="grid grid-cols-2 gap-2">
                    {codes.map(c => (
                        <code key={c} className="bg-(--base-02) border border-(--base-03) rounded-md px-2.5 py-1.5 text-xs font-mono text-(--base-09) text-center select-all">
                            {c}
                        </code>
                    ))}
                </div>
                <button type="button" onClick={copyAll} className="btn btn-secondary btn-sm w-full">
                    {copied ? <><Check size={12} /> Copied</> : <><Copy size={12} /> Copy all codes</>}
                </button>
                <label className="flex items-start gap-2 text-xs text-(--base-07) cursor-pointer pt-1">
                    <input type="checkbox" className="checkbox mt-0.5" checked={acknowledged} onChange={e => setAcknowledged(e.target.checked)} />
                    <span>I have saved these codes in a secure location.</span>
                </label>
                <button
                    type="button"
                    onClick={onDone}
                    disabled={!acknowledged}
                    className="btn btn-primary w-full disabled:opacity-40 disabled:cursor-not-allowed"
                >
                    Continue to the panel
                </button>
            </div>
        </div>
    );
}
