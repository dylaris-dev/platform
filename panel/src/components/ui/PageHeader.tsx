import type { ReactNode } from 'react';

/**
 * Page title (Syne) + one-line description, actions top-right.
 *   <PageHeader title="Backups" description="One line." actions={<button className="btn btn-primary">New</button>} />
 * `icon` is an optional 16px lucide element before the title. `as` picks the
 * heading level (default h1; use h2 inside a layout that already has an h1).
 */
export default function PageHeader({ title, description, actions, icon, as: Tag = 'h1', className = '' }: {
    title: ReactNode;
    description?: ReactNode;
    actions?: ReactNode;
    icon?: ReactNode;
    as?: 'h1' | 'h2';
    className?: string;
}) {
    return (
        <header className={`flex flex-wrap items-start justify-between gap-x-6 gap-y-3 ${className}`}>
            <div className="min-w-0">
                <Tag className="flex items-center gap-2.5 font-display text-[22px] font-bold leading-tight text-(--base-09)">
                    {icon && <span className="shrink-0 text-(--base-06)" aria-hidden="true">{icon}</span>}
                    <span className="min-w-0 truncate">{title}</span>
                </Tag>
                {description && <p className="mt-1.5 text-sm leading-relaxed text-(--base-06)">{description}</p>}
            </div>
            {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
        </header>
    );
}
