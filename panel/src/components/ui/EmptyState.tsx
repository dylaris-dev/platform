import type { ReactNode } from 'react';

/**
 * Empty (or error) state inside a surface. `icon` is a lucide element at 20px,
 * `action` is a button/link. `tone="error"` colors the icon for load failures.
 *   <EmptyState icon={<Server size={20} />} title="No servers yet" description="Create one." action={<button className="btn btn-primary">Create</button>} />
 * `bare` drops the surface (use when already inside a card).
 */
export default function EmptyState({ icon, title, description, action, tone = 'neutral', bare = false, className = '' }: {
    icon?: ReactNode;
    title: ReactNode;
    description?: ReactNode;
    action?: ReactNode;
    tone?: 'neutral' | 'error';
    bare?: boolean;
    className?: string;
}) {
    return (
        <div
            className={`flex flex-col items-center px-6 py-12 text-center ${bare ? '' : 'srv-surface'} ${className}`}
            role={tone === 'error' ? 'alert' : undefined}
        >
            {icon && (
                <span
                    className={`mb-3 flex size-10 items-center justify-center rounded-full bg-(--base-03) ${tone === 'error' ? 'text-(--error-light)' : 'text-(--base-06)'}`}
                    aria-hidden="true"
                >
                    {icon}
                </span>
            )}
            <p className="text-sm font-medium text-(--base-09)">{title}</p>
            {description && <p className="mt-1 max-w-md text-[13px] leading-relaxed text-(--base-06)">{description}</p>}
            {action && <div className="mt-4 flex items-center gap-2">{action}</div>}
        </div>
    );
}
