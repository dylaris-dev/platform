'use client';

import Switch from '@/components/ui/Switch';

/**
 * LimitField is the ONE control for every "how many may they have" number in the
 * panel.
 *
 * The platform convention it renders (see services.Limits in core):
 *
 *   null  - no limit at all
 *   0     - none; they may hold zero of this
 *   n     - the cap
 *
 * Before this there were three different widgets for the same idea: a checkbox
 * that emptied the field (Backups), a toggle that wrote -1 (Gateway), and a bare
 * number input with "0 = unlimited" in the help text (Beam, Nodes). An operator
 * had to remember which screen meant what, and two of the three could not say
 * "none" at all.
 *
 * The number input is hidden rather than disabled while unlimited is on: a
 * greyed-out field still showing "5" invites the reading that 5 is somehow still
 * in force.
 */
export function LimitField({
    value,
    onChange,
    unit,
    min = 0,
    step = 1,
    id,
    describedBy,
    unlimitedLabel = 'No limit',
}: {
    value: number | null;
    onChange: (v: number | null) => void;
    /** Shown after the input, e.g. "GB", "Mbit/s". Omit for a bare count. */
    unit?: string;
    /** Lowest acceptable cap. 0 by default, because "none" is a valid cap. */
    min?: number;
    /**
     * Input step. Integral by default, and the value is truncated to match -
     * "3.7 nodes" is not a thing. A fractional step turns that off, for the few
     * limits measured in a unit where a fraction is meaningful (GiB).
     */
    step?: number;
    id?: string;
    describedBy?: string;
    /**
     * What the null state is CALLED here. Defaults to "No limit", which is what
     * null means everywhere a limit stands on its own.
     *
     * A scoped limit is the exception: leaving one blank hands the question to
     * the scope below it, so calling that "No limit" on screen states the
     * opposite of what it does whenever the scope below has a cap.
     */
    unlimitedLabel?: string;
}) {
    const unlimited = value === null;

    return (
        <div className="flex items-center gap-3 shrink-0">
            {!unlimited && (
                <div className="flex items-center gap-1.5">
                    <input
                        id={id}
                        type="number"
                        min={min}
                        step={step}
                        value={value}
                        aria-describedby={describedBy}
                        onChange={e => {
                            // An emptied field is not zero. Holding it at the
                            // current value until a number arrives keeps a
                            // half-typed edit from briefly meaning "none".
                            const raw = e.target.value;
                            if (raw === '') return;
                            const n = Number(raw);
                            if (!Number.isFinite(n)) return onChange(value);
                            onChange(Math.max(min, Number.isInteger(step) ? Math.trunc(n) : n));
                        }}
                        className="input-mono w-20 text-center"
                    />
                    {unit && <span className="text-xs text-(--base-06)">{unit}</span>}
                </div>
            )}
            <label className="flex items-center gap-1.5 cursor-pointer select-none">
                <Switch
                    ariaLabel={unlimitedLabel}
                    checked={unlimited}
                    onChange={() => onChange(unlimited ? 0 : null)}
                />
                <span className="text-xs text-(--base-06)">{unlimitedLabel}</span>
            </label>
        </div>
    );
}

/**
 * The convention itself, as help text.
 *
 * ONE node shared by every LimitField site rather than a sentence written 21
 * times. The three states are the whole point of the control and none of them
 * is legible from the screen: an operator who types 0 cannot see whether that
 * means "none" or "switched off", and on this platform it used to mean both
 * depending on which page they were on.
 */
export const LimitHelp = (
    <>
        <p className="mb-2">This number has <strong>three</strong> meanings, not two.</p>
        <p className="mb-2">
            <strong>No limit</strong> - the switch - removes the cap entirely.
            <br />
            <strong>0</strong> means <em>none</em>: they may hold zero of this.
            <br />
            <strong>Any other number</strong> is the cap.
        </p>
        <p>
            So 0 and &quot;No limit&quot; are opposites here. Every limit in the panel
            reads the same way, which is why the switch exists instead of a magic
            number.
        </p>
    </>
);

/** How a limit reads in prose, for summaries and list rows. */
export function limitLabel(value: number | null, unit?: string): string {
    if (value === null) return 'No limit';
    if (value === 0) return 'None';
    return unit ? `${value} ${unit}` : String(value);
}
