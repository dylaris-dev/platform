import type { ReactNode } from 'react';

/**
 * A page section: short title, optional one-line description, actions on the
 * right, then the body. No surface of its own; put a `.srv-surface` card (or a
 * RowList) inside it.
 *   <Section title="Members" description="Who can open this." actions={...}>...</Section>
 */
export default function Section({ title, description, actions, children, className = '' }: {
    title: ReactNode;
    description?: ReactNode;
    actions?: ReactNode;
    children: ReactNode;
    className?: string;
}) {
    return (
        <section className={`space-y-3 ${className}`}>
            <div className="flex flex-wrap items-end justify-between gap-x-4 gap-y-2">
                <div className="min-w-0">
                    <h2 className="text-[15px] font-semibold text-(--base-09)">{title}</h2>
                    {description && <p className="mt-0.5 text-[13px] text-(--base-06)">{description}</p>}
                </div>
                {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
            </div>
            {children}
        </section>
    );
}
