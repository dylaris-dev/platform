"use client";

import React, { useEffect, useRef, useState } from 'react';
import { Loader2 } from 'lucide-react';
import { voiceNameState, type VoiceNameInput } from '@/lib/voiceChat';

const TONE_CLASS: Record<string, string> = {
    neutral: 'text-xs text-(--base-06)',
    success: 'text-xs text-(--base-07)',
    info: 'alert alert-info text-xs',
    warning: 'alert alert-warning text-xs',
};

/**
 * The white-label voice name under a voice switch, for a server and for a
 * route-only address. onSet runs the set ("" cancels, the same name checks
 * again) and resolves true when the field may close; the caller shows the
 * error. onRestart is the caller's restart offer, absent where Core cannot
 * restart the server. Nothing restarts on its own (owner decision O5).
 */
export default function VoiceNameState({ input, busy, onSet, onRestart }: {
    input: VoiceNameInput;
    busy: boolean;
    onSet: (name: string) => Promise<boolean>;
    onRestart?: () => void;
}) {
    const [open, setOpen] = useState(false);
    const [draft, setDraft] = useState('');
    const [running, setRunning] = useState(false);
    const [slow, setSlow] = useState(false);
    const openerRef = useRef<HTMLButtonElement>(null);
    const boxRef = useRef<HTMLDivElement>(null);
    const wasOpen = useRef(false);

    // Focus goes back where it came from when the field closes: the opener,
    // or the block when a save replaced the opener with the new state.
    useEffect(() => {
        if (wasOpen.current && !open) (openerRef.current ?? boxRef.current)?.focus();
        wasOpen.current = open;
    }, [open]);

    // House rule: nothing under ~400 ms, then a spinner; only for a call made
    // here, the switch above has its own.
    useEffect(() => {
        if (!running) { setSlow(false); return; }
        const t = setTimeout(() => setSlow(true), 400);
        return () => clearTimeout(t);
    }, [running]);

    const s = voiceNameState(input);
    const name = input.name || '';
    const set = async (n: string) => {
        setRunning(true);
        try {
            if (await onSet(n)) setOpen(false);
        } finally {
            setRunning(false);
        }
    };
    const btn = 'btn btn-secondary btn-sm text-xs py-1';

    return (
        <div ref={boxRef} tabIndex={-1} className="mt-2 space-y-2 outline-none" aria-busy={running}>
            <div className={TONE_CLASS[s.tone]} role={input.state === 'fallen_back' ? 'alert' : undefined}>
                <p>{s.text}</p>
            </div>
            <div className="flex flex-wrap items-center gap-2">
                {s.actions.includes('useOwn') && !open && (
                    <button
                        ref={openerRef}
                        type="button"
                        onClick={() => { setDraft(''); setOpen(true); }}
                        disabled={busy}
                        className="text-xs text-(--base-07) underline underline-offset-2 hover:text-(--base-09) active:text-(--base-08) focus-ring disabled:opacity-40"
                    >
                        Use a name on my own domain
                    </button>
                )}
                {s.actions.includes('restart') && onRestart && (
                    <button type="button" onClick={onRestart} disabled={busy} className="btn btn-primary btn-sm text-xs py-1">
                        Restart now
                    </button>
                )}
                {s.actions.includes('check') && (
                    <button type="button" onClick={() => set(name)} disabled={busy} className={btn}>Check again</button>
                )}
                {s.actions.includes('cancel') && (
                    <button type="button" onClick={() => set('')} disabled={busy} className={btn}>Cancel</button>
                )}
                {s.actions.includes('remove') && (
                    <button type="button" onClick={() => set('')} disabled={busy} className={btn}>Stop using this name</button>
                )}
                {slow && <Loader2 size={14} className="animate-spin text-(--base-06)" aria-hidden="true" />}
            </div>
            {open && (
                <form
                    className="flex flex-wrap items-center gap-2"
                    onSubmit={e => { e.preventDefault(); if (draft.trim()) void set(draft.trim()); }}
                    onKeyDown={e => {
                        // Closes the field only, not the dialog it sits in.
                        if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); setOpen(false); }
                    }}
                >
                    <input
                        type="text"
                        value={draft}
                        onChange={e => setDraft(e.target.value)}
                        disabled={busy}
                        placeholder="voice.play.example.com"
                        autoFocus
                        spellCheck={false}
                        autoCapitalize="off"
                        className="input-field text-xs py-1 w-64 max-w-full font-mono"
                        aria-label="Voice chat name on your own domain"
                    />
                    <button type="submit" disabled={busy || !draft.trim()} className="btn btn-primary btn-sm text-xs py-1">Save</button>
                    <button type="button" onClick={() => setOpen(false)} disabled={busy} className="btn btn-ghost btn-sm text-xs py-1">Close</button>
                </form>
            )}
        </div>
    );
}
