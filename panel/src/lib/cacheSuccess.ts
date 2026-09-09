/**
 * cacheSuccess memoises an async step for as long as it is RUNNING or has
 * SUCCEEDED, and forgets a failed one so the next caller starts over.
 *
 * The distinction is the whole point, and getting it wrong is quiet. The
 * obvious shape - `if (!promise) promise = run(); return promise` - caches the
 * ATTEMPT, so the first failure is the answer everybody gets for the rest of
 * the session. That is what left the Beam app saying "not logged in" on every
 * server after one handshake failed: nothing retried, because something had
 * already tried.
 *
 * Failure is what the caller says it is, not what the promise does. The step
 * this was written for reports a failed handshake by RESOLVING with a reason -
 * a rejection would be a second thing to handle at every call site - so
 * `succeeded` decides, and a rejection counts as failure too.
 *
 * Concurrent callers share the in-flight attempt rather than each starting one:
 * the step it guards tears down live state, so running it twice at once is not
 * the same as running it twice in a row. That holds for a retry too - the
 * second attempt is shared exactly like the first.
 *
 * Clearing needs no "is this still the current attempt" check, and that is
 * worth saying because the check looks obviously necessary. An attempt is put
 * into `inFlight` synchronously, before any callback of its own can run, and a
 * new one is only ever created while `inFlight` is null - so when a callback
 * runs, what it is clearing can only be itself.
 */
export function cacheSuccess<T>(
    run: () => Promise<T>,
    succeeded: (result: T) => boolean,
): () => Promise<T> {
    let inFlight: Promise<T> | null = null;

    return () => {
        if (inFlight) return inFlight;

        inFlight = run().then(
            result => {
                if (!succeeded(result)) inFlight = null;
                return result;
            },
            err => {
                inFlight = null;
                throw err;
            },
        );

        return inFlight;
    };
}
