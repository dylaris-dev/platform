"use client";

import { Check } from 'lucide-react';
import '@/styles/server-setup.css';

export interface StepDef { id: string; label: string }

/**
 * Numbered steps; the current one is violet, finished ones show a check.
 * A step is a button only once it has been reached, so the order stays guided
 * but nothing already filled in is out of reach.
 */
export default function SetupStepper({ steps, current, reached, onSelect }: {
    steps: StepDef[];
    current: number;
    /** Highest step index the user has been on. */
    reached: number;
    onSelect: (index: number) => void;
}) {
    return (
        <nav aria-label="Setup steps">
            <ol className="srv-setup-steps">
                {steps.map((s, i) => {
                    const done = i < current;
                    const inner = (
                        <>
                            <span className="srv-setup-dot" aria-hidden="true">{done ? <Check size={14} /> : i + 1}</span>
                            {s.label}
                        </>
                    );
                    return (
                        <li key={s.id} className="contents">
                            {i > 0 && <span className="srv-setup-rule" aria-hidden="true" />}
                            {i <= reached ? (
                                <button type="button" className="srv-setup-step" data-done={done} aria-current={i === current ? 'step' : undefined} onClick={() => onSelect(i)}>
                                    {inner}
                                </button>
                            ) : (
                                <span className="srv-setup-step" aria-disabled="true">{inner}</span>
                            )}
                        </li>
                    );
                })}
            </ol>
        </nav>
    );
}
