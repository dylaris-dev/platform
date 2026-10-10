/**
 * Per-key trailing debounce with a single in-flight run.
 *
 * Built for re-reading a server's mod list on server_mods.changed: "Update all"
 * over thirty mods emits two frames per mod, and each frame used to cost its own
 * GETs. Calls for one key within delayMs collapse into one run; a call while a
 * run is in flight queues exactly one more after it, so the last change is
 * never missed and two runs for a key never overlap.
 */
export function createCoalescer(delayMs: number) {
    type Slot = { timer?: ReturnType<typeof setTimeout>; running: boolean; again: boolean; fn: () => Promise<unknown> };
    const slots = new Map<string | number, Slot>();

    const schedule = (key: string | number, s: Slot) => {
        if (s.timer) clearTimeout(s.timer);
        s.timer = setTimeout(() => fire(key, s), delayMs);
    };

    const fire = (key: string | number, s: Slot) => {
        s.timer = undefined;
        if (s.running) { s.again = true; return; }
        s.running = true;
        Promise.resolve()
            .then(s.fn)
            .catch(() => { /* the caller's fn owns its errors */ })
            .finally(() => {
                s.running = false;
                if (s.again) {
                    s.again = false;
                    schedule(key, s);
                } else if (!s.timer) {
                    slots.delete(key);
                }
            });
    };

    return (key: string | number, fn: () => Promise<unknown>) => {
        let s = slots.get(key);
        if (!s) {
            s = { running: false, again: false, fn };
            slots.set(key, s);
        }
        s.fn = fn;
        schedule(key, s);
    };
}
