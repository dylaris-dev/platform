'use client';

import { useCallback, useEffect } from 'react';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';

/**
 * A page's tab, held in the URL.
 *
 * The settings search points at a SETTING, and half of them live behind a tab.
 * Landing on the right page with the wrong tab selected leaves the operator
 * exactly where they were: looking for something on a screen that does not show
 * it.
 *
 * The tab is DERIVED from the query string rather than mirrored into state. The
 * first version read it once on mount, which worked for a deep link and failed
 * for the case the search actually produces: searching from a page you are
 * already on replaces the URL without remounting anything, so the tab never
 * moved. Deriving it also makes back and forward work, at no cost - a tab is a
 * place, and the URL is where places live.
 */
export function useTabParam<T extends string>(valid: readonly T[], fallback: T): [T, (t: T) => void] {
    const router = useRouter();
    const pathname = usePathname();
    const params = useSearchParams();

    const wanted = params.get('tab');
    const tab = (wanted && (valid as readonly string[]).includes(wanted) ? wanted : fallback) as T;

    const setTab = useCallback(
        (next: T) => {
            const q = new URLSearchParams(params.toString());
            q.set('tab', next);
            // replace, not push: the settings nav navigates the same way, and
            // nobody wants twenty back-button entries from clicking tabs.
            router.replace(`${pathname}?${q.toString()}`, { scroll: false });
        },
        [params, pathname, router],
    );

    // A #tab hash was a documented deep link form before this existed.
    // Promote it to the query form once, so there is only one representation to
    // reason about afterwards.
    useEffect(() => {
        if (typeof window === 'undefined' || wanted) return;
        const hash = window.location.hash.replace(/^#/, '');
        if (hash && (valid as readonly string[]).includes(hash)) {
            setTab(hash as T);
        }
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [wanted]);

    return [tab, setTab];
}
