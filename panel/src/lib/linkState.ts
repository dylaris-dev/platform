/**
 * What to tell a customer about the Link that carries their players.
 *
 * Kept away from the components because it is the only part worth testing and
 * because both halves of "My infrastructure" have to say the same thing: a
 * route-only kit row showed a green shield whether the link had ever booted or
 * not, and the machines tab showed the node's own state and nothing about the
 * Link beside it. In gateway routing the Link is the only way in, so "the
 * machine is online and its Link is not connected" is the state that looks
 * healthy and serves nobody.
 *
 * `online === undefined` means Core could not ask (no Redis, a failed read). It
 * is NOT "not connected": saying that because our own lookup failed sends a
 * customer to debug a machine that is fine.
 */
export type LinkTone = 'ok' | 'bad' | 'unknown';

export type LinkState = {
    tone: LinkTone;
    /** Short label for the badge. */
    label: string;
    /** One sentence saying what it means, and what to do when it is wrong. */
    hint: string;
};

export function linkState(online: boolean | undefined | null, suspended = false): LinkState {
    if (online === undefined || online === null) {
        return {
            tone: 'unknown',
            label: 'No answer',
            hint: 'We could not read whether this link is connected. This says nothing about your machine.',
        };
    }
    if (online) {
        return {
            tone: 'ok',
            label: 'Connected',
            hint: 'The link is holding its tunnel to us, so players can reach what it points at.',
        };
    }
    // Suspension is OUR doing: billing drops the link's credential, so it stops
    // holding its tunnel. Telling that customer to go and start something would
    // send them to debug a machine we switched off.
    if (suspended) {
        return {
            tone: 'bad',
            label: 'Not connected',
            hint: 'Your account is suspended, so we stopped this link. It comes back when the account is active again.',
        };
    }
    // Rolling the key is deliberately NOT named here: a roll replaces only the
    // warp key, while the tunnel token derives from the link id, so a link that
    // was not redeployed keeps running and reads as connected.
    return {
        tone: 'bad',
        label: 'Not connected',
        hint: 'Nobody can reach your server through this address. Start the file on your machine. A link that just started takes a few seconds to show up here.',
    };
}

/**
 * The same answer for a machine, where the node's own state is known too.
 *
 * The order matters: a machine that is not connected at all explains itself, so
 * saying "the Link is not connected" beside it is noise. It is the machine that
 * is ONLINE while its Link is not that needs the sentence, because nothing else
 * on the screen says why nobody can join.
 */
export function nodeLinkState(nodeOnline: boolean, online: boolean | undefined | null, suspended = false): LinkState {
    if (!nodeOnline) {
        return {
            tone: 'unknown',
            label: 'Link unknown',
            hint: 'The machine itself is not connected, so we cannot say anything about its link.',
        };
    }
    const s = linkState(online, suspended);
    if (s.tone === 'bad') {
        return {
            tone: 'bad',
            label: 'Link not connected',
            // A suspended account keeps the sentence it already has: we stopped
            // this link, so sending them to redeploy it would be a lie.
            hint: suspended
                ? s.hint
                : 'The machine is online but its link is not, so nobody can join the servers on it. Take this machine\'s deploy file again and redeploy it. A link that just started takes a few seconds to show up here.',
        };
    }
    if (s.tone === 'ok') return { ...s, label: 'Link connected' };
    return { ...s, label: 'Link: no answer' };
}
