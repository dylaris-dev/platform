import type { RoutingMode } from './api/types';

export const DEFAULT_CONTAINER_PORT = 25565;

/**
 * Host and container port are an admin's to change, and only where players
 * reach the server by IP:port. With routing on 'gateway' alone nothing binds
 * the host port, so an edit there would restart the server for nothing.
 */
export const portsEditable = (isAdmin: boolean, routingMode: RoutingMode) =>
    isAdmin && routingMode !== 'gateway';

/**
 * The Advanced section starts closed unless it holds something set on purpose:
 * a RAM headroom override, or ports that differ from the automatic ones.
 */
export function advancedStartsOpen(o: {
    paddingText: string;
    canEditPorts: boolean;
    hostPort: number;
    containerPort: number;
}): boolean {
    if (o.paddingText.trim() !== '') return true;
    return o.canEditPorts && (o.hostPort !== 0 || o.containerPort !== DEFAULT_CONTAINER_PORT);
}
