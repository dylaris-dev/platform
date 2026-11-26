'use client';

import { useState } from 'react';
import { useRouter } from 'next/navigation';
import { Search, X } from 'lucide-react';
import {
    Autocomplete,
    AutocompleteContent,
    AutocompleteInput,
    AutocompleteItem,
    AutocompleteList,
} from '@ui-sdk/react/autocomplete';
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
    // Every other navigation in the panel goes through GuardedLink. This one
    // called router.replace directly, so searching while a form was dirty left
    // the page, unmounted the section and dropped the edits without asking -
    // the same failure the rest of this work exists to remove, reintroduced by
    // the component that was meant to help.
    const registration = useUnsavedChangesState();
    const [pending, setPending] = useState<SettingsHit | null>(null);
    const [saving, setSaving] = useState(false);

    const hits = searchSettings(q);

    const hitKey = (hit: SettingsHit) => `${hit.page}-${hit.tab ?? ''}-${hit.label}`;

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

    return (
        <div className="relative px-3">
            {/* The index does its own matching (descriptions, synonyms), so the SDK filter is off.
                Choosing a hit navigates instead of filling the field. */}
            <Autocomplete
                filter={false}
                autoHighlight
                value={q}
                onValueChange={(v, details) => {
                    // On a chosen suggestion `v` is its label text; the key we set as value is on details.item.
                    if (details.item) {
                        details.cancel();
                        const hit = hits.find(h => hitKey(h) === details.item?.value);
                        if (hit) go(hit);
                        return;
                    }
                    setQ(v);
                }}
                open={q.trim().length >= 2}
                onOpenChange={(open, details) => {
                    if (!open && (details.reason === 'escape-key' || details.reason === 'outside-press')) setQ('');
                }}
            >
            <div className="relative">
                <Search size={13} className="absolute left-2.5 top-1/2 -translate-y-1/2 text-(--base-06) pointer-events-none" />
                <AutocompleteInput
                    type="search"
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

            <AutocompleteContent className="ui-popup w-(--anchor-width) max-h-[min(20rem,var(--available-height))] overflow-y-auto p-1.5">
                {hits.length === 0 ? (
                    <p className="px-3 py-2.5 text-xs text-(--base-06)">
                        Nothing matches. Try what the setting does rather than what it is called.
                    </p>
                ) : (
                    <AutocompleteList>
                        {hits.map(hit => (
                            <AutocompleteItem
                                key={hitKey(hit)}
                                value={hitKey(hit)}
                                className="dropdown-item flex-col items-start gap-0"
                            >
                                <span className="text-sm text-(--base-09)">{hit.label}</span>
                                <span className="text-[11px] text-(--base-06)">{hit.where}</span>
                            </AutocompleteItem>
                        ))}
                    </AutocompleteList>
                )}
            </AutocompleteContent>
            </Autocomplete>

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
