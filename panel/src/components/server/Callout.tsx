import type { ReactNode } from 'react';
import { ChevronDown } from 'lucide-react';

type Tone = 'info' | 'warning' | 'error';

const ICON_TONE: Record<Tone, string> = {
    info: 'text-(--accent-light)',
    warning: 'text-(--warning-light)',
    error: 'text-(--error-light)',
};

/**
 * A quiet inline notice: tinted surface, no border. `actions` sit at the end.
 * `foldable` (needs `title`): only the title line shows, the body folds out on
 * click, for long explanations that would otherwise dominate a card.
 */
export default function Callout({ tone, icon, title, children, actions, role, foldable = false }: {
    tone: Tone;
    icon: ReactNode;
    title?: string;
    children?: ReactNode;
    actions?: ReactNode;
    role?: 'alert' | 'status';
    foldable?: boolean;
}) {
    if (foldable && title) {
        return (
            <details className="srv-callout group" data-tone={tone} role={role}>
                <summary className="flex min-w-0 flex-1 cursor-pointer list-none items-start gap-3 rounded-md focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] [&::-webkit-details-marker]:hidden">
                    <span className={`mt-0.5 shrink-0 ${ICON_TONE[tone]}`} aria-hidden="true">{icon}</span>
                    <span className="min-w-0 flex-1 font-medium text-(--base-09)">{title}</span>
                    <ChevronDown size={16} aria-hidden="true" className="mt-0.5 shrink-0 text-(--base-06) transition-transform group-open:rotate-180 motion-reduce:transition-none" />
                </summary>
                <div className="mt-2 pl-7 text-(--base-07)">{children}</div>
            </details>
        );
    }
    return (
        <div className="srv-callout" data-tone={tone} role={role}>
            <span className={`mt-0.5 shrink-0 ${ICON_TONE[tone]}`} aria-hidden="true">{icon}</span>
            <div className="min-w-0 flex-1">
                {title && <div className="font-medium text-(--base-09)">{title}</div>}
                {children && <div className={title ? 'text-(--base-07)' : ''}>{children}</div>}
            </div>
            {actions && <div className="flex shrink-0 items-center gap-1.5">{actions}</div>}
        </div>
    );
}
