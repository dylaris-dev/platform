"use client";

import React, {
    createContext,
    useCallback,
    useContext,
    useState,
    useEffect,
    useId,
    useMemo,
    useRef,
} from 'react';
import { Loader2 } from 'lucide-react';
import ModalPanel from '@/components/ui/ModalPanel';

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

export interface UnsavedChangesRegistration {
    dirty: boolean;
    /**
     * Commit. Resolves TRUE only when everything was actually written.
     *
     * The boolean is the whole contract. It used to resolve void, so a guard
     * could not tell a successful save from a refused one - and both guards
     * navigated away afterwards, unmounting the section and dropping the edits.
     * A server error did it, and so did the operator answering "no" to a
     * confirmation: they said no and were moved off the screen showing what
     * they said no to.
     */
    save: () => Promise<boolean>;
    discard: () => void;
    saving: boolean;
}

// The value read by the navigation guards: one aggregate over every registered
// section, or null when nothing is registered.
type UnsavedChangesContextValue = UnsavedChangesRegistration | null;

interface UnsavedChangesCtx {
    registrations: Record<string, UnsavedChangesRegistration>;
    register: (id: string, reg: UnsavedChangesRegistration) => void;
    unregister: (id: string) => void;
}

// ---------------------------------------------------------------------------
// Aggregation
// ---------------------------------------------------------------------------

/**
 * aggregate folds every registered section into the one thing the navigation
 * guards act on: leaving a page, leaving a tab, closing the browser.
 *
 * Each card commits itself, so this is no longer what draws a Save button. It
 * is still what answers "is anything unsaved anywhere on this page", which is a
 * question about the page and not about any one card.
 *
 * It exists because the context used to hold exactly ONE registration, and an
 * unmount cleared it unconditionally. That was survivable while a settings page
 * was a single form; it stops being survivable the moment a page is a tab bar
 * over several independent sections, because the second section to mount would
 * silently replace the first and the first section's unsaved edits would leave
 * with no prompt.
 *
 * Saves run in SEQUENCE, not in parallel. Two sections of one page routinely
 * write the same settings table, and two concurrent writes of it produce a
 * last-writer-wins result that depends on network timing.
 *
 * Exported for its own test: this is the part that decides whether unsaved work
 * can be lost, and it is pure.
 */
export function aggregate(
    regs: UnsavedChangesRegistration[],
): UnsavedChangesRegistration | null {
    if (regs.length === 0) return null;
    return {
        dirty: regs.some(r => r.dirty),
        saving: regs.some(r => r.saving),
        save: async () => {
            // Only the dirty ones: a clean section's save handler is free to
            // assume it has something to write.
            //
            // STOPS at the first refusal, and reports it. Carrying on would
            // write half a page and then tell the caller it was fine, which is
            // how a guard ends up navigating away from unsaved work.
            for (const r of regs) {
                if (!r.dirty) continue;
                if (!(await r.save())) return false;
            }
            return true;
        },
        discard: () => {
            for (const r of regs) r.discard();
        },
    };
}

// ---------------------------------------------------------------------------
// Context
// ---------------------------------------------------------------------------

const UnsavedChangesContext = createContext<UnsavedChangesCtx>({
    registrations: {},
    register: () => {},
    unregister: () => {},
});

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

export function UnsavedChangesProvider({ children }: { children: React.ReactNode }) {
    const [registrations, setRegistrations] = useState<Record<string, UnsavedChangesRegistration>>({});

    const register = useCallback((id: string, reg: UnsavedChangesRegistration) => {
        setRegistrations(prev => ({ ...prev, [id]: reg }));
    }, []);

    const unregister = useCallback((id: string) => {
        setRegistrations(prev => {
            if (!(id in prev)) return prev;
            const next = { ...prev };
            delete next[id];
            return next;
        });
    }, []);

    // beforeunload — warn the user when they refresh / close the tab / type
    // a new URL while a form on the page is dirty. The custom Save/Discard
    // dialog only fires for in-app navigation (handled by GuardedLink); this
    // catches the cases the SPA can't intercept itself.
    const dirtyRef = useRef(false);
    dirtyRef.current = Object.values(registrations).some(r => r.dirty);
    useEffect(() => {
        const handler = (e: BeforeUnloadEvent) => {
            if (!dirtyRef.current) return;
            e.preventDefault();
            // Modern browsers ignore the message but still need returnValue
            // to be set (truthy) to show their generic prompt.
            e.returnValue = '';
        };
        window.addEventListener('beforeunload', handler);
        return () => window.removeEventListener('beforeunload', handler);
    }, []);

    const value = useMemo(
        () => ({ registrations, register, unregister }),
        [registrations, register, unregister],
    );

    return (
        <UnsavedChangesContext.Provider value={value}>
            {children}
        </UnsavedChangesContext.Provider>
    );
}

// ---------------------------------------------------------------------------
// Hook — used by each settings section to register itself
// ---------------------------------------------------------------------------

/**
 * Call this hook inside a settings section to register its dirty state, save
 * handler and discard handler with the page's navigation guards.
 *
 * Several sections on one page may each call it; they are keyed independently
 * and unregister only themselves on unmount.
 */
export function useUnsavedChanges(reg: {
    dirty: boolean;
    /** Resolve false when nothing was written; see the registration type. */
    save: () => Promise<boolean>;
    discard: () => void;
    saving?: boolean;
}) {
    const { register, unregister } = useContext(UnsavedChangesContext);
    const id = useId();

    // Keep stable refs to the latest callbacks so the effect deps stay minimal.
    const saveRef = useRef(reg.save);
    const discardRef = useRef(reg.discard);
    saveRef.current = reg.save;
    discardRef.current = reg.discard;

    const { dirty, saving = false } = reg;

    useEffect(() => {
        register(id, {
            dirty,
            save: () => Promise.resolve(saveRef.current()),
            discard: () => discardRef.current(),
            saving,
        });
        return () => unregister(id);
        // Re-run whenever dirty or saving changes; callbacks use refs so they
        // don't need to be in deps.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [id, dirty, saving]);
}

// ---------------------------------------------------------------------------
// Consumer hook — used by the layout and the nav guards to read current state
// ---------------------------------------------------------------------------

export function useUnsavedChangesState(): UnsavedChangesContextValue {
    const { registrations } = useContext(UnsavedChangesContext);
    return useMemo(() => aggregate(Object.values(registrations)), [registrations]);
}

// ---------------------------------------------------------------------------
// Shared confirm dialog — shown when navigating away with unsaved changes
// (used both by the settings tab bar and a page's internal tab bar)
// ---------------------------------------------------------------------------

export function UnsavedDialog({
    onSave,
    onDiscard,
    onCancel,
    saving,
}: {
    onSave: () => void | Promise<void>;
    onDiscard: () => void;
    onCancel: () => void;
    saving: boolean;
}) {
    return (
        <div className="modal-overlay animate-fade-in" onClick={onCancel}>
            <ModalPanel onClose={onCancel}
                className="modal-panel w-full max-w-sm"
                onClick={e => e.stopPropagation()}
            >
                <div className="modal-header">
                    <h3 className="modal-title">Unsaved changes</h3>
                </div>
                <div className="modal-body">
                    <p className="text-sm text-(--base-07)">
                        You have unsaved changes on this page. What would you like to do?
                    </p>
                </div>
                <div className="modal-footer">
                    <button
                        type="button"
                        onClick={onCancel}
                        className="btn btn-secondary"
                        disabled={saving}
                    >
                        Cancel
                    </button>
                    <button
                        type="button"
                        onClick={onDiscard}
                        className="btn btn-secondary text-(--error-light) border-(--error) hover:bg-(--error)/10"
                        disabled={saving}
                    >
                        Discard
                    </button>
                    <button
                        type="button"
                        onClick={onSave}
                        disabled={saving}
                        className="btn btn-primary disabled:opacity-40 inline-flex items-center gap-1.5"
                    >
                        {saving && <Loader2 size={13} className="animate-spin" />}
                        Save
                    </button>
                </div>
            </ModalPanel>
        </div>
    );
}
