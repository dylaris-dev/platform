import type { ReactNode } from 'react';
import { SettingsCardSave, type SavableForm } from '@/components/settings/SettingsCard';

/**
 * A configuration card: short title, optional one-line description, actions
 * (and the card's own Save/Discard when it owns a form) top right. One card is
 * one save, as in SettingsCard; only the look differs.
 */
export default function ConfigSection({ title, description, form, actions, children, className = '' }: {
    title: ReactNode;
    description?: ReactNode;
    form?: SavableForm;
    actions?: ReactNode;
    children?: ReactNode;
    className?: string;
}) {
    return (
        <section className={`srv-surface overflow-hidden ${className}`}>
            <header className="flex flex-wrap items-start justify-between gap-3 px-5 pt-5">
                <div className="min-w-0 max-w-2xl">
                    <h2 className="srv-config-card-title">{title}</h2>
                    {description && <p className="srv-config-desc">{description}</p>}
                </div>
                {(actions || form) && (
                    <div className="flex items-center gap-2">
                        {actions}
                        {form && <SettingsCardSave form={form} />}
                    </div>
                )}
            </header>
            <div className="flex flex-col gap-4 p-5">{children}</div>
        </section>
    );
}
