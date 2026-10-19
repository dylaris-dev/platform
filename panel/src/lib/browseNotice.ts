// What the Content tab's result list says when it has no rows to show. Three
// different situations used to read the same "No projects match.": Modrinth
// not answering, the Environment filter hiding every hit, and a real empty
// result. Only the last one is about the search.

import { envParam, type EnvFilter } from './modSide';

/** The active Environment filter as the toggles read, or null when off. */
export function envFilterName(f: EnvFilter): string | null {
    switch (envParam(f)) {
        case 'client': return 'Client';
        case 'server': return 'Server';
        case 'both': return 'Client and Server';
        default: return null;
    }
}

export interface BrowseNotice {
    kind: 'failed' | 'empty';
    message: string;
}

export function browseNotice(s: { failed: boolean; hits: number; visible: number; env: EnvFilter }): BrowseNotice | null {
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
    return { kind: 'empty', message: 'No projects match.' };
}
