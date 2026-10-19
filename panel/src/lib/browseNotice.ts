// What the Content tab's result list says when it has no rows to show. Three
// different situations used to read the same "No projects match.": Modrinth
// not answering, the server-side filter hiding every hit, and a real empty
// result. Only the last one is about the search.

import { serverBucketLabel, type ServerBucketFilter } from './modSide';

/** The selected server-side button's name, or null when none is. */
export function envFilterName(f: ServerBucketFilter): string | null {
    return f ? serverBucketLabel(f) : null;
}

export interface BrowseNotice {
    kind: 'failed' | 'empty';
    message: string;
}

export function browseNotice(s: { failed: boolean; hits: number; visible: number; env: ServerBucketFilter }): BrowseNotice | null {
    if (s.failed) {
        return { kind: 'failed', message: 'Modrinth did not answer, so this is not an empty result. Try again in a moment.' };
    }
    if (s.visible > 0) return null;
    const name = envFilterName(s.env);
    if (s.hits > 0 && name) {
        return {
            kind: 'empty',
            message: `The ${name} filter under Environment hid all ${s.hits} results on this page. Turn it off, or search for something more specific.`,
        };
    }
    if (name) {
        return { kind: 'empty', message: `No projects match with the ${name} filter on. Turn it off under Environment to see more.` };
    }
    if (s.hits > 0) {
        // No button is on, so the only thing hiding rows is the client-only rule.
        return {
            kind: 'empty',
            message: `All ${s.hits} results on this page are client-only mods, which do nothing on a server. Search for something more specific.`,
        };
    }
    return { kind: 'empty', message: 'No projects match.' };
}
