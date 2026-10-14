// singleFlight wraps an async refresh so one run happens at a time. A call
// made while a run is in flight does not start a second one: it marks that
// the run must go once more when it ends, and gets that run's promise - so a
// caller that awaits after a change still sees data fetched after it. A
// failed pass does not cancel the one owed; only the last pass's error is
// passed on.
export function singleFlight(fn: () => Promise<void>): () => Promise<void> {
    let flight: Promise<void> | null = null;
    let again = false;
    return () => {
        if (flight) {
            again = true;
            return flight;
        }
        const run = async () => {
            for (;;) {
                again = false;
                try {
                    await fn();
                } catch (err) {
                    if (!again) throw err;
                }
                if (!again) return;
            }
        };
        flight = run().finally(() => { flight = null; });
        return flight;
    };
}
