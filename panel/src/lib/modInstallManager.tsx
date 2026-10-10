"use client";

import React, { createContext, useCallback, useContext, useEffect, useRef, useState } from 'react';
import { installMod, listInstalledMods, type InstalledMod, type InstallModPayload } from '@/lib/api/modrinth';
import { systemEvents } from '@/lib/systemEvents';
import { toast } from '@/components/ui/Toast';
import { adopt as adoptRows, afterPost, expire, isPending, reconcile, type InstallJob } from '@/lib/modInstallJobs';
import { createCoalescer } from '@/lib/coalesce';

// Mod installs live here, above the router, so leaving the Content tab neither
// loses nor stops following one. Completion arrives as server_mods.changed
// (Core publishes it when the node's report lands); the poll is the safety net
// for a dropped SSE stream or an older Core that does not publish it yet.
const POLL_MS = 5000;
const COALESCE_MS = 300;

interface ModInstallCtx {
    jobs: InstallJob[];
    /** POSTs the install and follows it. Resolves once Core has answered. */
    start: (serverId: number, serverName: string, payload: InstallModPayload) => Promise<InstallJob>;
    /** The newest job for this project on this server, if any. */
    jobFor: (serverId: number, projectId: string) => InstallJob | undefined;
    /** Follows rows still "installing" that no job here tracks, e.g. after a reload. */
    adopt: (serverId: number, serverName: string, rows: readonly InstalledMod[]) => void;
    dismiss: (id: string) => void;
    clearFinished: () => void;
}

const Ctx = createContext<ModInstallCtx | null>(null);

let seq = 0;

export function ModInstallProvider({ children }: { children: React.ReactNode }) {
    const [jobs, setJobs] = useState<InstallJob[]>([]);
    const jobsRef = useRef(jobs);
    jobsRef.current = jobs;

    const read = useCallback(async (serverId: number) => {
        let rows;
        let forbidden = false;
        try {
            rows = await listInstalledMods(serverId);
        } catch (e) {
            rows = null;
            forbidden = (e as { status?: number })?.status === 403;
        }
        const now = Date.now();
        setJobs(prev => {
            const next = prev.map(j => {
                if (j.serverId !== serverId) return j;
                return rows ? reconcile(j, rows, now) : expire(j, now, forbidden);
            });
            // Only servers with a job are read here, so one of them has the name.
            const name = prev.find(j => j.serverId === serverId)?.serverName;
            return rows && name ? adoptRows(next, serverId, name, rows, now) : next;
        });
    }, []);
    // A bulk update emits two frames per mod; one read per server per burst.
    const coalesce = useRef(createCoalescer(COALESCE_MS)).current;
    const refresh = useCallback((serverId: number) => coalesce(serverId, () => read(serverId)), [coalesce, read]);

    const start = useCallback<ModInstallCtx['start']>(async (serverId, serverName, payload) => {
        const job: InstallJob = {
            id: `mod-${Date.now()}-${++seq}`,
            serverId,
            serverName,
            projectId: payload.projectId,
            versionId: payload.versionId,
            title: payload.title || payload.fileName,
            state: 'sending',
            startedAt: Date.now(),
        };
        // One live entry per project and server: a second click replaces the
        // first, the same way Core's row does.
        setJobs(prev => [...prev.filter(j => !(j.serverId === serverId && j.projectId === job.projectId)), job]);
        const res = await installMod(serverId, payload);
        const next = afterPost(job, res, Date.now());
        setJobs(prev => prev.map(j => (j.id === job.id ? next : j)));
        return next;
    }, []);

    // Toast a job that went wrong, once, wherever the reader is in the panel.
    // Successes only change the button and the widget: "Update all" can finish
    // thirty at once. Kept out of the state updaters, which React may run twice.
    const announced = useRef(new Set<string>());
    useEffect(() => {
        for (const j of jobs) {
            if ((j.state !== 'failed' && j.state !== 'unknown') || announced.current.has(j.id)) continue;
            announced.current.add(j.id);
            toast(`${j.title} on ${j.serverName}: ${j.message ?? 'the install failed'}`, false);
        }
    }, [jobs]);

    useEffect(() => systemEvents.on('server_mods.changed', evt => {
        const sid = Number((evt.payload as { serverId?: unknown } | undefined)?.serverId);
        if (jobsRef.current.some(j => j.serverId === sid && j.state === 'installing')) refresh(sid);
    }), [refresh]);

    const following = jobs.some(j => j.state === 'installing');
    useEffect(() => {
        if (!following) return;
        const t = setInterval(() => {
            const ids = new Set(jobsRef.current.filter(j => j.state === 'installing').map(j => j.serverId));
            ids.forEach(id => { refresh(id); });
        }, POLL_MS);
        return () => clearInterval(t);
    }, [following, refresh]);

    const jobFor = useCallback<ModInstallCtx['jobFor']>((serverId, projectId) => {
        for (let i = jobs.length - 1; i >= 0; i--) {
            if (jobs[i].serverId === serverId && jobs[i].projectId === projectId) return jobs[i];
        }
        return undefined;
    }, [jobs]);

    const adopt = useCallback<ModInstallCtx['adopt']>((serverId, serverName, rows) => {
        setJobs(prev => adoptRows(prev, serverId, serverName, rows, Date.now()));
    }, []);

    const dismiss = useCallback((id: string) => setJobs(prev => prev.filter(j => j.id !== id)), []);
    const clearFinished = useCallback(() => setJobs(prev => prev.filter(isPending)), []);

    return (
        <Ctx.Provider value={{ jobs, start, jobFor, adopt, dismiss, clearFinished }}>
            {children}
        </Ctx.Provider>
    );
}

const noop: ModInstallCtx = {
    jobs: [],
    start: async (serverId, serverName, payload) => {
        const job: InstallJob = {
            id: 'noop', serverId, serverName, projectId: payload.projectId, versionId: payload.versionId,
            title: payload.title, state: 'sending', startedAt: Date.now(),
        };
        return afterPost(job, await installMod(serverId, payload), Date.now());
    },
    jobFor: () => undefined,
    adopt: () => { /* noop */ },
    dismiss: () => { /* noop */ },
    clearFinished: () => { /* noop */ },
};

export function useModInstalls(): ModInstallCtx {
    return useContext(Ctx) ?? noop;
}
