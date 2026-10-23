'use client';

import { useEffect, useRef, useState } from 'react';
import { useRouter } from 'next/navigation';
import { Search, X } from 'lucide-react';
import { searchSettings, hrefFor, type SettingsHit } from '@/lib/settingsIndex';
import { useUnsavedChangesState, UnsavedDialog } from '@/components/settings/UnsavedChanges';

/**
 * Search over individual settings, above the settings navigation.
 *
 * The nav lists page names, and a page name is the wrong question: nobody
 * looking for Resend, a bucket or a retention horizon knows which of the 26
 * pages it lives behind, and half of them are two levels deep now that the long
 * ones are tabbed.
 */
export default function SettingsSearch({ onNavigate }: { onNavigate?: () => void }) {
    const router = useRouter();
    const [q, setQ] = useState('');
    const [active, setActive] = useState(0);
    const boxRef = useRef<HTMLDivElement>(null);
    // Every other navigation in the panel goes through GuardedLink. This one
    // called router.replace directly, so searching while a form was dirty left
    // the page, unmounted the section and dropped the edits without asking -
    // the same failure the rest of this work exists to remove, reintroduced by
    // the component that was meant to help.
    const registration = useUnsavedChangesState();
    const [pending, setPending] = useState<SettingsHit | null>(null);
    const [saving, setSaving] = useState(false);

    const hits = searchSettings(q);

    useEffect(() => setActive(0), [q]);

    useEffect(() => {
        const onDown = (e: MouseEvent) => {
            if (!boxRef.current?.contains(e.target as Node)) setQ('');
        };
        document.addEventListener('mousedown', onDown);
        return () => document.removeEventListener('mousedown', onDown);
    }, []);

    const navigate = (hit: SettingsHit) => {
        setQ('');
        onNavigate?.();
        // replace, matching how the settings nav itself navigates: the sidebar
        // is not a place anyone wants twenty back-button entries from.
        router.replace(hrefFor(hit));
    };

    const go = (hit: SettingsHit) => {
        if (registration?.dirty) {
            setPending(hit);
            return;
        }
        navigate(hit);
    };

    const onKeyDown = (e: React.KeyboardEvent) => {
        if (e.key === 'Escape') { setQ(''); return; }
        if (hits.length === 0) return;
        if (e.key === 'ArrowDown') { e.preventDefault(); setActive(i => (i + 1) % hits.length); }
        if (e.key === 'ArrowUp') { e.preventDefault(); setActive(i => (i - 1 + hits.length) % hits.length); }
        if (e.key === 'Enter') { e.preventDefault(); go(hits[active]); }
    };

    return (
        <div ref={boxRef} className="relative px-3">
            <div className="relative">
                <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-(--base-06) pointer-events-none" />
                <input
                    type="search"
                    value={q}
                    onChange={e => setQ(e.target.value)}
                    onKeyDown={onKeyDown}
                    placeholder="Search settings"
                    aria-label="Search settings"
                    className="input-field input-icon-left input-icon-right w-full"
                />
                {q && (
                    <button
                        type="button"
                        onClick={() => setQ('')}
                        aria-label="Clear search"
                        className="absolute right-2 top-1/2 -translate-y-1/2 text-(--base-06) hover:text-(--base-09)"
                    >
                        <X size={13} />
                    </button>
                )}
            </div>

            {q.trim().length >= 2 && (
                <div className="absolute left-3 right-3 mt-1 z-40 dropdown-menu max-h-80 overflow-y-auto">
                    {hits.length === 0 ? (
                        <p className="px-3 py-2.5 text-xs text-(--base-06)">
                            Nothing matches. Try what the setting does rather than what it is called.
                        </p>
                    ) : (
                        hits.map((hit, i) => (
                            <button
                                key={`${hit.page}-${hit.tab ?? ''}-${hit.label}`}
                                type="button"
                                onClick={() => go(hit)}
                                onMouseEnter={() => setActive(i)}
                                className={`dropdown-item flex-col items-start gap-0 ${i === active ? 'bg-(--base-03) text-(--base-09)' : ''}`}
                            >
                                <span className="text-sm text-(--base-09)">{hit.label}</span>
                                <span className="text-[11px] text-(--base-06)">{hit.where}</span>
                            </button>
                        ))
                    )}
                </div>
            )}

            {pending && registration && (
                <UnsavedDialog
                    saving={saving}
                    onCancel={() => { setPending(null); setSaving(false); }}
                    onSave={async () => {
                        setSaving(true);
                        let ok = false;
                        try {
                            ok = await registration.save();
                        } catch {
                            ok = false;
                        } finally {
                            setSaving(false);
                        }
                        if (!ok) return;
                        const hit = pending;
                        setPending(null);
                        navigate(hit);
                    }}
                    onDiscard={() => {
                        registration.discard();
                        const hit = pending;
                        setPending(null);
                        navigate(hit);
                    }}
                />
            )}
        </div>
    );
}
