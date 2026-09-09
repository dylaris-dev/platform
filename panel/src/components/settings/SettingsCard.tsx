'use client';

import React from 'react';
import { Loader2 } from 'lucide-react';
import HelpTip from '@/components/ui/HelpTip';

/**
 * The settings box, and the one place a save control is allowed to live.
 *
 * The panel used to put Save at the bottom of the viewport in a fixed bar. It
 * was one bar for the whole panel, which made it consistent and made it wrong:
 * the control sat as far from the field being edited as the screen allows, so
 * on a tall page the operator changed something at the top and got their
 * confirmation at the bottom, in the corner of their eye.
 *
 * The rule this component encodes: ONE CARD IS ONE SAVE. A card owns exactly
 * one form, and that form's whole payload is what its button writes. Where a
 * page has several topics that share a single endpoint, they belong in one card
 * as SettingsGroups - not in three cards with three buttons issuing the same
 * request, which is what per-box saving degenerates into if the boxes are drawn
 * by topic instead of by payload.
 *
 * The card does not replace the navigation guard. useSettingsForm still
 * registers with UnsavedChanges, so leaving the page or the tab with unsaved
 * work still prompts. The bar was the display; the guard is the protection, and
 * only the display went away.
 */

/**
 * What a card needs in order to own a save control.
 *
 * useSettingsForm returns a superset of this, so a form goes straight in. Kept
 * structural rather than importing SettingsForm<T> so a surface can adopt the
 * card before it adopts the hook.
 */
export interface SavableForm {
    dirty: boolean;
    saving: boolean;
    /** True when the load failed: saving is refused because the values on
     *  screen are defaults rather than what is stored. */
    loadFailed?: boolean;
    save: () => Promise<boolean>;
    discard: () => void;
}

export interface SettingsCardProps {
    title: React.ReactNode;
    /** One line under the title. Say what the setting does, not what it is. */
    description?: React.ReactNode;
    /**
     * The long answer for the whole card, behind a help icon on its title.
     * For what the card is FOR - the choice between its modes, the consequence
     * of getting it wrong - rather than any single field in it.
     */
    help?: React.ReactNode;
    icon?: React.ElementType;
    /** Wire this and the card grows a Save/Discard pair in its header. */
    form?: SavableForm;
    /** Overrides the button text. Keep it short: the card title says what. */
    saveLabel?: string;
    /** Replaces the generic load-failure banner where saving stale defaults
     *  back would do something specific and bad, and the operator deserves to
     *  be told which. */
    loadFailedMessage?: React.ReactNode;
    /** Refuses the save and says why. For a form that is dirty but not yet
     *  valid: the button has to stay inert, and a button that is inert without
     *  saying why is the same as a button that is broken. */
    saveBlockedReason?: string;
    /** Card-specific buttons, placed left of Save. */
    actions?: React.ReactNode;
    /** Rendered below the body, inside the card, above its bottom edge. */
    footer?: React.ReactNode;
    children?: React.ReactNode;
    className?: string;
    /** Body spacing. Set 'none' for a card whose child manages its own. */
    bodySpacing?: 'normal' | 'none';
}

export default function SettingsCard({
    title,
    description,
    help,
    icon: Icon,
    form,
    saveLabel = 'Save',
    loadFailedMessage,
    saveBlockedReason,
    actions,
    footer,
    children,
    className = '',
    bodySpacing = 'normal',
}: SettingsCardProps) {
    const loadFailed = form?.loadFailed ?? false;

    return (
        <section className={`card settings-card ${className}`}>
            <header className="settings-card-header">
                <div className="min-w-0">
                    <h3 className="settings-card-title">
                        {Icon && <Icon size={14} className="text-(--accent-light) shrink-0" />}
                        {title}
                        {help && (
                            <HelpTip label={typeof title === 'string' ? `About ${title}` : 'Help'}>
                                {help}
                            </HelpTip>
                        )}
                    </h3>
                    {description && (
                        <p className="settings-card-description">{description}</p>
                    )}
                </div>
                {(actions || form) && (
                    <div className="settings-card-actions">
                        {actions}
                        {form && (
                            <SettingsCardSave
                                form={form}
                                saveLabel={saveLabel}
                                blockedReason={saveBlockedReason}
                            />
                        )}
                    </div>
                )}
            </header>

            <div
                className={`settings-card-body${
                    bodySpacing === 'none' ? ' settings-card-body-plain' : ''
                }`}
            >
                {loadFailed && (
                    <div className="alert alert-error text-xs" role="alert">
                        {loadFailedMessage ?? (
                            <>
                                These settings could not be loaded, so the values shown are defaults
                                rather than what is stored. Saving is disabled until a reload succeeds.
                            </>
                        )}
                    </div>
                )}
                {children}
            </div>

            {footer && <div className="settings-card-footer">{footer}</div>}
        </section>
    );
}

/**
 * What the Save button is doing, which is four states and not two.
 *
 * Pulled out as a function because the difference between them is the whole
 * behaviour of this component and it is easy to collapse by accident: an
 * `!dirty || saving` disables the button in a way that reads correctly and
 * quietly re-enables it the moment a hung save resolves, on a form the operator
 * has since edited again.
 *
 *   idle     nothing to write. Present, grey, inert.
 *   ready    dirty and valid. The only state that saves.
 *   saving   in flight. Inert, spinner. Discard stays live on purpose.
 *   blocked  dirty but refused: the load failed, or the form is not valid yet.
 */
export type SaveState = 'idle' | 'ready' | 'saving' | 'blocked';

export function saveState(form: SavableForm, blockedReason?: string): SaveState {
    if (form.saving) return 'saving';
    // Ordered before `dirty`: a form whose load failed shows defaults, and a
    // defaults-vs-defaults comparison is clean, so "not dirty" would report
    // idle and hide the fact that this form cannot be saved at all.
    if (form.loadFailed || blockedReason) return 'blocked';
    return form.dirty ? 'ready' : 'idle';
}

/**
 * The Save/Discard pair.
 *
 * Save is always present and never moves. Grey and inert while there is nothing
 * to write, so its position is learned before it is needed rather than
 * appearing under the cursor at the moment of use.
 *
 * Discard shows up only when there is something to throw away, to the LEFT of
 * Save so that Save keeps its place when it appears.
 */
export function SettingsCardSave({
    form,
    saveLabel = 'Save',
    blockedReason,
}: {
    form: SavableForm;
    saveLabel?: string;
    /** Set when the form is dirty but not yet valid. */
    blockedReason?: string;
}) {
    const { dirty, saving, save, discard } = form;
    const canSave = saveState(form, blockedReason) === 'ready';

    return (
        <>
            {dirty && (
                /* Deliberately NOT disabled while saving. The save already
                   captured the value it is writing, so discarding cannot
                   corrupt it - and a request that hangs used to leave the
                   operator with dirty edits, no way to save them and no way to
                   throw them away, with beforeunload blocking the reload. */
                <button
                    type="button"
                    onClick={() => discard()}
                    className="btn btn-secondary btn-sm animate-fade-in"
                >
                    Discard
                </button>
            )}
            <button
                type="button"
                onClick={() => { void save(); }}
                disabled={!canSave}
                title={blockedReason}
                aria-label={
                    blockedReason
                        ? `${saveLabel} (${blockedReason})`
                        : dirty
                          ? `${saveLabel} (unsaved changes)`
                          : `${saveLabel} (nothing to save)`
                }
                className={`btn btn-sm inline-flex items-center gap-1.5 ${
                    canSave ? 'btn-primary' : 'btn-secondary settings-card-save-idle'
                }`}
            >
                {saving && <Loader2 size={12} className="animate-spin" />}
                {saveLabel}
            </button>
        </>
    );
}

/**
 * A labelled block inside a card.
 *
 * This is the alternative to opening another card. A settings page that writes
 * one payload should look like one thing being configured; before this, a page
 * of eleven boxes read as eleven independent things, and nothing on screen said
 * which of them the one Save button was about to write.
 */
export function SettingsGroup({
    title,
    description,
    help,
    children,
    className = '',
    /** First group in a card: no divider above it. */
    first = false,
}: {
    title?: React.ReactNode;
    description?: React.ReactNode;
    /**
     * The long answer for the whole group, behind a help icon on its title.
     *
     * Where a group's fields share one explanation - four transfer limits, six
     * ticket quotas - this is the place for it. Repeating the same paragraph on
     * each field is how a help icon stops being worth opening.
     */
    help?: React.ReactNode;
    children: React.ReactNode;
    className?: string;
    first?: boolean;
}) {
    return (
        <div className={`${first ? '' : 'settings-group-divided'} ${className}`}>
            {title && (
                <h4 className="mono-label flex items-center gap-1.5">
                    {title}
                    {help && (
                        <HelpTip label={typeof title === 'string' ? `About ${title}` : 'Help'}>
                            {help}
                        </HelpTip>
                    )}
                </h4>
            )}
            {description && (
                <p className="text-xs text-(--base-06) leading-relaxed mt-1">{description}</p>
            )}
            <div className={title || description ? 'mt-3 space-y-3' : 'space-y-3'}>
                {children}
            </div>
        </div>
    );
}

/**
 * A label and hint on the left, a control on the right.
 *
 * SwitchRow already does this for a switch; this is the same row for everything
 * that is not one - a number, a select, a pair of buttons. Both exist so the
 * two halves of a settings page cannot drift into different rows.
 */
export function SettingsRow({
    label,
    description,
    help,
    htmlFor,
    children,
    className = '',
}: {
    label: React.ReactNode;
    description?: React.ReactNode;
    /**
     * The long answer, behind a help icon beside the label. See SwitchRow for
     * why this is not just a longer `description`.
     */
    help?: React.ReactNode;
    /** Set when the control has an id, so the label actually focuses it. */
    htmlFor?: string;
    children: React.ReactNode;
    className?: string;
}) {
    const body = (
        <>
            <span className="text-sm font-medium text-(--base-09) block">{label}</span>
            {description && (
                <span className="text-xs text-(--base-06) leading-snug mt-0.5 block">
                    {description}
                </span>
            )}
        </>
    );

    // The trigger sits OUTSIDE the <label>. A button inside a label is still
    // part of the label's click target, so opening the help would also toggle
    // the control the label points at.
    return (
        <div className={`flex items-start justify-between gap-4 ${className}`}>
            <div className="min-w-0 flex items-start gap-1.5">
                {htmlFor ? (
                    <label htmlFor={htmlFor} className="min-w-0 cursor-pointer">{body}</label>
                ) : (
                    <div className="min-w-0">{body}</div>
                )}
                {help && (
                    <HelpTip
                        className="mt-0.5"
                        label={typeof label === 'string' ? `About ${label}` : 'Help'}
                    >
                        {help}
                    </HelpTip>
                )}
            </div>
            <div className="shrink-0 flex items-center gap-2">{children}</div>
        </div>
    );
}

/**
 * The pill in a card's `actions` slot that says whether the thing the card
 * configures is actually in force.
 *
 * `active` must be the state IN FORCE, not the half-edited form: a badge that
 * turns green while you type has only moved the lie earlier. It lived beside
 * the user-settings cards until the mail transport moved out of that file and
 * needed it too; every use is a SettingsCard action, so this is its home.
 */
export function StatusBadge({ active, activeLabel = 'Active', inactiveLabel = 'Off' }: { active: boolean; activeLabel?: string; inactiveLabel?: string }) {
    return (
        <span
            className={`text-[9px] font-mono uppercase tracking-[0.08em] px-1.5 py-0.5 rounded-sm ${
                active ? 'text-(--success-light) bg-(--success-ghost)' : 'text-(--base-06) bg-(--base-03)'
            }`}
        >
            {active ? activeLabel : inactiveLabel}
        </span>
    );
}
