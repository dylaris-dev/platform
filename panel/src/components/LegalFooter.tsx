"use client";

import { useEffect, useState } from 'react';
import { getRegistrationStatus } from '@/lib/api/registration';

// A thin strip at the bottom of every panel screen. The imprint (Section 5 DDG)
// and the privacy policy must be reachable from every page, and Mojang's usage
// guidelines ask for the "not official" line wherever the Minecraft name is
// used. Links appear only on the hosted panel (Core sends legalBaseUrl when it
// is connected to the dylaris.com store): a self-hosted panel is someone
// else's service, and our imprint is not theirs.

let legalBase: Promise<string> | null = null;

function loadLegalBase(): Promise<string> {
    if (!legalBase) {
        legalBase = getRegistrationStatus()
            .then((s) => (typeof s?.legalBaseUrl === 'string' ? s.legalBaseUrl : ''))
            .catch(() => '');
    }
    return legalBase;
}

const LINKS = [
    { path: '/legal/imprint', label: 'Imprint' },
    { path: '/legal/privacy', label: 'Privacy' },
    { path: '/legal/terms', label: 'Terms' },
];

export default function LegalFooter({ className = '' }: { className?: string }) {
    const [base, setBase] = useState('');
    useEffect(() => {
        let alive = true;
        loadLegalBase().then((b) => { if (alive) setBase(b); });
        return () => { alive = false; };
    }, []);

    return (
        <footer className={`flex flex-wrap items-center justify-center gap-x-4 gap-y-1 px-4 py-1.5 text-[11px] text-(--base-06) ${className}`}>
            {base && (
                <nav aria-label="Legal" className="flex items-center gap-4">
                    {LINKS.map((l) => (
                        <a
                            key={l.path}
                            href={`${base}${l.path}`}
                            target="_blank"
                            rel="noopener noreferrer"
                            className="hover:text-(--base-08) focus-visible:text-(--base-08) transition-colors"
                        >
                            {l.label}
                        </a>
                    ))}
                </nav>
            )}
            {/* Mojang's wording, in its capitals. */}
            <span className="tracking-wide">NOT AN OFFICIAL MINECRAFT SERVICE. NOT APPROVED BY OR ASSOCIATED WITH MOJANG OR MICROSOFT.</span>
        </footer>
    );
}
