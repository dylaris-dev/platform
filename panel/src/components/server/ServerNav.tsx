"use client";

import { useLayoutEffect, useRef, useState } from 'react';
import Link from 'next/link';
import { MoreHorizontal } from 'lucide-react';
import { DynamicIcon } from '@/lib/icons';
import Menu, { type MenuEntry } from '@/components/ui/Menu';

export interface NavTab {
    slug: string;
    icon: string;
    label: string;
    disabled: boolean;
}

const MORE_WIDTH = 84;

/**
 * The server's tab row. The tabs are routes, so they are links with
 * aria-current rather than a tablist. Tabs that do not fit move into a "More"
 * menu; the active one is always kept in the row.
 */
export default function ServerNav({ serverId, tabs, pathname }: { serverId: number; tabs: NavTab[]; pathname: string }) {
    const rowRef = useRef<HTMLDivElement>(null);
    const measureRef = useRef<HTMLDivElement>(null);
    const [fit, setFit] = useState(tabs.length);

    const hrefOf = (t: NavTab) => `/servers/${serverId}/${t.slug}`;
    // Configuration has sub-tabs (config/properties, ...): the whole prefix keeps it active.
    const isActive = (t: NavTab) => (t.slug === 'config' ? pathname.startsWith(hrefOf(t)) : pathname === hrefOf(t));

    // Widths come from a hidden copy of the full row, so a tab that is in the
    // menu still has a size to compare against the space.
    // A string, not `tabs`: the shell passes a new array every render, which would rebuild the observer each tick.
    const tabsKey = tabs.map(t => `${t.slug}:${t.label}:${t.disabled}`).join('|');
    useLayoutEffect(() => {
        const row = rowRef.current;
        const meter = measureRef.current;
        if (!row || !meter) return;
        const compute = () => {
            const widths = Array.from(meter.children).map(c => (c as HTMLElement).offsetWidth);
            const avail = row.clientWidth;
            const total = widths.reduce((a, b) => a + b, 0);
            if (total <= avail) { setFit(tabs.length); return; }
            let used = MORE_WIDTH;
            let n = 0;
            for (const w of widths) {
                if (used + w > avail) break;
                used += w;
                n++;
            }
            setFit(Math.max(n, 1));
        };
        compute();
        const ro = new ResizeObserver(compute);
        ro.observe(row);
        return () => ro.disconnect();
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [tabsKey]);

    let shown = tabs.slice(0, fit);
    let hidden = tabs.slice(fit);
    const activeHidden = hidden.find(isActive);
    if (activeHidden && shown.length > 0) {
        // Swap the active tab in for the last one that fits.
        const bumped = shown[shown.length - 1];
        shown = [...shown.slice(0, -1), activeHidden];
        hidden = [bumped, ...hidden.filter(t => t !== activeHidden)];
    }

    const linkFor = (t: NavTab) => {
        const inner = (
            <>
                <DynamicIcon name={t.icon} size={16} />
                <span>{t.label}</span>
            </>
        );
        if (t.disabled) {
            return <span key={t.slug} aria-disabled="true" className="srv-nav-link">{inner}</span>;
        }
        return (
            <Link
                key={t.slug}
                href={hrefOf(t)}
                replace
                aria-current={isActive(t) ? 'page' : undefined}
                className="srv-nav-link"
            >
                {inner}
            </Link>
        );
    };

    const items: MenuEntry[] = hidden.map(t => ({
        id: t.slug,
        label: t.label,
        icon: <DynamicIcon name={t.icon} size={16} />,
        disabled: t.disabled,
        render: <Link href={hrefOf(t)} replace />,
    }));

    return (
        <nav aria-label="Server sections" className="relative overflow-hidden px-1 pb-0.5">
            <div ref={rowRef} className="flex items-center gap-0.5">
                {shown.map(t => linkFor(t))}
                {hidden.length > 0 && (
                    <Menu
                        align="end"
                        trigger={
                            <button type="button" aria-label="More sections" className="srv-nav-link" style={{ width: MORE_WIDTH - 2 }}>
                                <MoreHorizontal size={16} />
                                <span>More</span>
                            </button>
                        }
                        items={items}
                    />
                )}
            </div>
            <div ref={measureRef} aria-hidden="true" className="pointer-events-none invisible absolute left-0 top-0 flex h-0 w-max overflow-hidden whitespace-nowrap" {...{ inert: true }}>
                {tabs.map(t => <span key={t.slug} className="srv-nav-link shrink-0">
                    <DynamicIcon name={t.icon} size={16} />
                    <span>{t.label}</span>
                </span>)}
            </div>
        </nav>
    );
}
