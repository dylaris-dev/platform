'use client';

import type { ReactNode } from 'react';
import Link from 'next/link';

/**
 * Quiet rows instead of tables: no borders, hover fill. Put `<RowList>` inside a
 * `.srv-surface` card (or alone) and `<Row>` inside it.
 *   <RowList label="Backups">
 *     <Row leading={<Icon/>} title="main" meta="2 GB" trailing={<Badge/>} actions={<Menu .../>} href="/x" />
 *   </RowList>
 * `title`/`meta` stack on the left, `trailing` is quiet info on the right, `actions`
 * (a row `Menu` when more than two) sits last. With `href` the whole row is a link, with
 * `onClick` a button; the `actions` slot stays outside it so it keeps its own click.
 * `active` marks the selected row (aria-current). `RowHead` is an optional column-label line.
 */
export function RowList({ children, label, className = '' }: { children: ReactNode; label?: string; className?: string }) {
    return <div role="list" aria-label={label} className={`flex flex-col gap-0.5 p-1.5 ${className}`}>{children}</div>;
}

export function RowHead({ children, className = '' }: { children: ReactNode; className?: string }) {
    return <div className={`srv-label flex items-center gap-3 px-3 pb-1 pt-2 ${className}`}>{children}</div>;
}

export function Row({ leading, title, meta, trailing, actions, href, onClick, active, className = '' }: {
    leading?: ReactNode;
    title: ReactNode;
    meta?: ReactNode;
    trailing?: ReactNode;
    actions?: ReactNode;
    href?: string;
    onClick?: () => void;
    active?: boolean;
    className?: string;
}) {
    const body = (
        <>
            {leading && <span className="shrink-0 text-(--base-06)">{leading}</span>}
            <span className="min-w-0 flex-1">
                <span className="block truncate text-sm font-medium text-(--base-09)">{title}</span>
                {meta && <span className="mt-0.5 block truncate text-xs text-(--base-06)">{meta}</span>}
            </span>
            {trailing && <span className="shrink-0 text-xs text-(--base-07)">{trailing}</span>}
        </>
    );
    const current = active ? 'page' : undefined;
    const interactive = href || onClick;
    return (
        <div role="listitem" className={`app-row ${className}`} aria-current={interactive ? undefined : current}>
            {href ? (
                <Link href={href} aria-current={current} className="flex min-w-0 flex-1 items-center gap-3 rounded-md focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">{body}</Link>
            ) : onClick ? (
                <button type="button" onClick={onClick} aria-current={current} className="flex min-w-0 flex-1 items-center gap-3 rounded-md text-left focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]">{body}</button>
            ) : (
                <div className="flex min-w-0 flex-1 items-center gap-3">{body}</div>
            )}
            {actions && <span className="flex shrink-0 items-center gap-1">{actions}</span>}
        </div>
    );
}
