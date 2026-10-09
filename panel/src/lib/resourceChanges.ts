/**
 * What a resource edit will do to the server, so the dialog can say it before
 * the click rather than after.
 *
 * The node recreates the container - a restart, if it is running - for a RAM or
 * port change and for removing a CPU limit or pinning. Other CPU and disk
 * changes are applied to the running container, and auto-move is only a flag
 * Core stores.
 */
export interface ResourceValues {
    ram: number;
    cpuLimit: number;
    /** MB, what Core stores and receives. */
    diskMB: number;
    hostPort: number;
    containerPort: number;
    cpuMode: string;
    cpuset: string;
    autoMove: boolean;
}

export interface ResourceChangePlan {
    restartFields: string[];
    liveFields: string[];
    /** Saving will restart a running server: ask first. */
    needsRestart: boolean;
    buttonLabel: string;
    note: string;
}

/** GB as typed in the dialog -> whole MB, since Core decodes an integer. */
export const diskGBToMB = (gb: number) => (gb > 0 ? Math.round(gb * 1024) : 0);

export function joinFields(fields: string[]): string {
    if (fields.length <= 1) return fields.join('');
    return `${fields.slice(0, -1).join(', ')} and ${fields[fields.length - 1]}`;
}

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

/**
 * canEditPorts mirrors Core: ports are admin-only and ignored for anyone else,
 * and a port of 0 means "leave it as it is".
 */
export function planResourceChange(
    current: ResourceValues,
    edited: ResourceValues,
    running: boolean,
    canEditPorts: boolean,
): ResourceChangePlan {
    const restartFields: string[] = [];
    const liveFields: string[] = [];

    if (edited.ram !== current.ram) restartFields.push('RAM');
    if (canEditPorts && edited.hostPort > 0 && edited.hostPort !== current.hostPort) restartFields.push('host port');
    if (canEditPorts && edited.containerPort > 0 && edited.containerPort !== current.containerPort) restartFields.push('container port');

    // Docker cannot lift a limit on a running container, so REMOVING one is a
    // recreate on the node while changing it is not. Leaving manual pinning may
    // end in no cpuset at all ('auto' does when the node has no topology), so it
    // is counted as a removal: asking once too often beats a silent restart.
    if (edited.cpuLimit !== current.cpuLimit) {
        (current.cpuLimit > 0 && edited.cpuLimit <= 0 ? restartFields : liveFields).push('CPU limit');
    }
    if (edited.cpuMode !== current.cpuMode || (edited.cpuMode === 'manual' && edited.cpuset.trim() !== current.cpuset.trim())) {
        const pinningRemoved = current.cpuset.trim() !== '' && edited.cpuMode !== 'manual' && edited.cpuMode !== current.cpuMode
            && (edited.cpuMode === 'shared' || current.cpuMode === 'manual');
        (pinningRemoved ? restartFields : liveFields).push('CPU pinning');
    }
    if (edited.diskMB !== current.diskMB) liveFields.push('disk');
    if (edited.autoMove !== current.autoMove) liveFields.push('auto-move');

    const needsRestart = running && restartFields.length > 0;
    let note: string;
    if (restartFields.length === 0 && liveFields.length === 0) {
        note = 'No changes.';
    } else if (!running) {
        note = 'The server is stopped: the change takes effect on its next start.';
    } else if (needsRestart) {
        note = `${cap(joinFields(restartFields))} ${restartFields.length === 1 ? 'restarts' : 'restart'} the server.`;
        if (liveFields.length > 0) note += ` ${cap(joinFields(liveFields))} ${liveFields.length === 1 ? 'is' : 'are'} applied live.`;
    } else {
        note = 'Applied live, no restart.';
    }

    return {
        restartFields,
        liveFields,
        needsRestart,
        buttonLabel: needsRestart ? 'Save and restart' : 'Save',
        note,
    };
}
