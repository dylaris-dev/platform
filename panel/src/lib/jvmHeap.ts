/**
 * The heap the node gives a server, mirroring jvmHeapMB in
 * platform/node/launch.go exactly (Go integer division included). The rest of
 * the booked RAM is left for the JVM's own off-heap memory, so a display of
 * -Xmx equal to the booked RAM would show a flag the server does not run with.
 */
export function jvmHeapMB(bookedMB: number): number {
    if (bookedMB <= 2048) return bookedMB;
    const reserve = Math.min(Math.max(Math.trunc((bookedMB * 15) / 100), 512), 2048);
    return Math.max(bookedMB - reserve, 2048);
}
