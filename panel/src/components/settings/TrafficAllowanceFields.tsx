"use client";

import { LimitField, LimitHelp } from '@/components/settings/LimitField';
import HelpTip from '@/components/ui/HelpTip';
import Checkbox from '@/components/ui/Checkbox';

/**
 * One scope's answer for one (region, kind).
 *
 * `set` is not decoration. A row that does not exist and a row holding two
 * nulls mean different things: absent lets the next scope answer, present with
 * nulls answers "no limit" here and stops the walk. Two number fields cannot
 * show that difference, so the checkbox carries it.
 */
export interface TrafficAllowance {
    set: boolean;
    includedGb: number | null;
    maxPurchaseGb: number | null;
}

export const emptyAllowance: TrafficAllowance = { set: false, includedGb: null, maxPurchaseGb: null };

export const sameAllowance = (a: TrafficAllowance, b: TrafficAllowance) =>
    a.set === b.set && a.includedGb === b.includedGb && a.maxPurchaseGb === b.maxPurchaseGb;

/**
 * The editor for one traffic allowance.
 *
 * Shared by the platform default in Settings and the per-tenant override on the
 * user's billing dialog. They ask the same question, so they are the same
 * control - a second copy would be free to drift into wording the operator
 * reads as a different question.
 *
 * `unsetNote` is the one thing the two callers differ on: at the default scope
 * "nothing decided" means unlimited, at an override it means the default
 * decides instead.
 */
export default function TrafficAllowanceFields({
    value,
    onChange,
    unsetNote,
}: {
    value: TrafficAllowance;
    onChange: (patch: Partial<TrafficAllowance>) => void;
    unsetNote: string;
}) {
    return (
        <div className="space-y-3">
            <Checkbox
                checked={value.set}
                onChange={c => onChange({ set: c })}
                label="Set an allowance here"
            />
            {!value.set ? (
                <p className="text-xs text-(--base-06)">{unsetNote}</p>
            ) : (
                <div className="flex flex-wrap items-center gap-6">
                    <div className="flex items-center gap-3">
                        <span className="srv-label w-28 flex items-center gap-1.5">
                            Included
                            <HelpTip label="About the included allowance">
                                <p className="mb-2">
                                    The traffic that costs nothing here. Metered billing, if it is
                                    on, only starts above this.
                                </p>
                                {LimitHelp}
                            </HelpTip>
                        </span>
                        <LimitField
                            value={value.includedGb}
                            onChange={v => onChange({ includedGb: v })}
                            unit="GB"
                        />
                    </div>
                    <div className="flex items-center gap-3">
                        <span className="srv-label w-28 flex items-center gap-1.5">
                            May buy
                            <HelpTip label="About the purchase ceiling">
                                <p className="mb-2">
                                    The most a customer may add on top of the included amount, and
                                    where their spending stops.
                                </p>
                                {LimitHelp}
                                <p className="mt-2">
                                    This is the one place a cap of 0 is the useful answer: it lets
                                    them use what is included and buy nothing beyond it.
                                </p>
                            </HelpTip>
                        </span>
                        <LimitField
                            value={value.maxPurchaseGb}
                            onChange={v => onChange({ maxPurchaseGb: v })}
                            unit="GB"
                        />
                    </div>
                </div>
            )}
        </div>
    );
}
