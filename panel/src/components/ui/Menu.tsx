'use client';

import React from 'react';
import { Menu as SdkMenu, MenuContent, MenuItem, MenuSeparator, MenuTrigger } from '@ui-sdk/react/menu';

export type MenuEntry =
    | { type: 'separator'; id?: string }
    | {
        type?: 'item';
        /** Stable key; defaults to the label. */
        id?: string;
        label: string;
        icon?: React.ReactNode;
        onSelect?: () => void | Promise<unknown>;
        /** Renders the item as a link; Enter follows it like a click. */
        href?: string;
        external?: boolean;
        /** Full control over the element, e.g. a next/link: `<Link href="/x" />`. */
        render?: React.ReactElement;
        danger?: boolean;
        disabled?: boolean;
    };

/**
 * A button-triggered action menu: the replacement for the hand-rolled
 * `.dropdown-menu` + click-outside blocks. Focus, arrow keys, typeahead,
 * Escape and outside-press come from the UI SDK Menu; the look is
 * `.ui-popup` / `.dropdown-item`.
 *
 * `trigger` is ONE element (usually a <button>) that becomes the menu button
 * and keeps its own classes and children. It needs an accessible name.
 */
export default function Menu({
    trigger,
    items,
    side = 'bottom',
    align = 'end',
    className = '',
    heading,
}: {
    trigger: React.ReactElement;
    items: MenuEntry[];
    side?: 'top' | 'right' | 'bottom' | 'left';
    align?: 'start' | 'center' | 'end';
    /** Extra classes on the popup, e.g. a min width. */
    className?: string;
    /** Non-interactive block above the items, e.g. "Signed in as". */
    heading?: React.ReactNode;
}) {
    // Controlled so a link item whose own onClick prevented the navigation
    // (the unsaved-changes guard) can close the menu: preventDefault also skips
    // the item's built-in select-and-close, which would leave the menu open
    // above the guard's dialog.
    const [open, setOpen] = React.useState(false);
    const closeIfPrevented = (el: React.ReactElement) => {
        const own = (el.props as { onClick?: (e: React.MouseEvent) => void }).onClick;
        if (!own) return el;
        return React.cloneElement(el as React.ReactElement<{ onClick?: (e: React.MouseEvent) => void }>, {
            onClick: (e: React.MouseEvent) => { own(e); if (e.defaultPrevented) setOpen(false); },
        });
    };
    return (
        <SdkMenu open={open} onOpenChange={setOpen}>
            <MenuTrigger render={trigger} />
            <MenuContent side={side} align={align} sideOffset={4} className={`ui-popup p-1.5 min-w-40 max-h-(--available-height) overflow-y-auto ${className}`}>
                {heading}
                {items.map((entry, i) => {
                    if (entry.type === 'separator') {
                        return <MenuSeparator key={entry.id ?? `sep-${i}`} className="dropdown-separator" />;
                    }
                    const render = entry.render ? closeIfPrevented(entry.render) : (entry.href
                        ? <a href={entry.href} {...(entry.external ? { target: '_blank', rel: 'noopener noreferrer' } : {})} />
                        : undefined);
                    return (
                        <MenuItem
                            key={entry.id ?? entry.label}
                            render={render}
                            disabled={entry.disabled}
                            onSelect={entry.onSelect ? () => entry.onSelect?.() : undefined}
                            className={`dropdown-item gap-3 ${entry.danger ? 'dropdown-item-danger' : ''}`}
                        >
                            {entry.icon}
                            {entry.label}
                        </MenuItem>
                    );
                })}
            </MenuContent>
        </SdkMenu>
    );
}
