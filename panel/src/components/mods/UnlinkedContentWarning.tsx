'use client';

import { TriangleAlert } from 'lucide-react';

/**
 * Says, in one place, the constraint that governs every version check: only
 * content Modrinth knows about can be checked or carried across a Minecraft
 * version.
 *
 * It is deliberately the same wording on the modpack builder, on a server, and
 * in the modpack creation dialog. An operator who reads it once at creation
 * time and again at migration time should recognise the same rule, not wonder
 * whether two different ones apply.
 */
export function UnlinkedContentWarning({
    count,
    context,
    action,
    className = '',
}: {
    /** Number of items that cannot be checked. Undefined renders the rule without a tally, for the creation dialog. */
    count?: number;
    context: 'pack' | 'server';
    action?: React.ReactNode;
    className?: string;
}) {
    const noun = context === 'pack' ? 'content' : 'jars';
    return (
        <div
            className={`srv-callout items-start gap-3 ${className}`}
            data-tone="warning"
            role="note"
        >
            <TriangleAlert size={16} className="text-(--warning-light) mt-0.5 shrink-0" />
            <div className="flex flex-col gap-1 min-w-0">
                <p className="text-sm text-(--base-09) font-medium">
                    {count === undefined
                        ? `Only Modrinth ${noun} can be version-checked`
                        : `${count} ${count === 1 ? 'item is' : 'items are'} not linked to Modrinth`}
                </p>
                <p className="text-xs text-(--base-07)">
                    {context === 'pack'
                        ? 'Files uploaded by hand carry no Modrinth identity, so nothing can be said about whether they work on another Minecraft version. A migration copies them over untouched and does not count them towards any verdict.'
                        : 'Jars placed by hand over SFTP or the file manager carry no Modrinth identity, so a version change cannot carry them and cannot say whether they would survive it.'}
                    {' '}
                    {context === 'pack'
                        ? 'Link them with "Replace with Modrinth" to include them in the check.'
                        : 'Identify them against Modrinth to include them, or remove them before moving.'}
                </p>
                {action && <div className="mt-1">{action}</div>}
            </div>
        </div>
    );
}

export default UnlinkedContentWarning;
