'use client';

import React from 'react';
import { Tabs as SdkTabs, TabsList, TabsTab } from '@ui-sdk/react/tabs';

/**
 * The panel's sub-navigation.
 *
 * Two screens used to render a SECOND vertical sidebar beside the settings
 * sidebar, and several more stacked five or six unrelated sections down one
 * scroll. Navigation depth here is capped at the sidebar plus one of these
 * bars; anything that would need a third level is a sign the page is two pages.
 */
export interface TabItem<Id extends string = string> {
    id: Id;
    label: string;
    icon?: React.ElementType;
    disabled?: boolean;
    /** Native tooltip; the reason a tab is disabled. */
    title?: string;
}

export default function Tabs<Id extends string = string>({
    items,
    active,
    onChange,
    className = '',
    ariaLabel = 'Sections',
}: {
    items: TabItem<Id>[];
    active: Id;
    onChange: (id: Id) => void;
    className?: string;
    ariaLabel?: string;
}) {
    // List only: callers render the panels themselves, so no TabsPanel.
    return (
        <SdkTabs value={active} onValueChange={v => { if (v != null) onChange(v as Id); }}>
            <TabsList className={`tab-bar ${className}`} aria-label={ariaLabel}>
                {items.map(({ id, label, icon: Icon, disabled, title }) => {
                    const isActive = id === active;
                    return (
                        <TabsTab
                            key={id}
                            value={id}
                            disabled={disabled}
                            title={title}
                            className={`tab ${isActive ? 'tab-active' : ''}`}
                        >
                            {Icon && (
                                <Icon
                                    size={14}
                                    className={isActive ? 'text-(--accent-light)' : 'text-(--base-06)'}
                                />
                            )}
                            {label}
                        </TabsTab>
                    );
                })}
            </TabsList>
        </SdkTabs>
    );
}
