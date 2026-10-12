import { describe, expect, it } from 'vitest';
import { addressesOf } from './NodesTab';

// A host lists one private address per Docker bridge and tunnel, so ms01 showed
// eleven. Only how the machine is reached stays in view; the rest waits behind
// "Show all". The kind comes from the node, never from the range.
describe('addressesOf', () => {
    const node = {
        address: '203.0.113.5',
        publicIp: '203.0.113.5',
        privateIps: ['10.10.10.8', '172.20.5.9', '172.17.0.1', '10.20.0.7'],
    };

    it('shows the address and the LAN, folds Docker and the overlay', () => {
        const out = addressesOf({
            ...node,
            privateIpKinds: { '10.10.10.8': 'lan', '172.20.5.9': 'lan', '172.17.0.1': 'docker', '10.20.0.7': 'overlay' },
        });
        expect(out.filter(a => a.primary).map(a => `${a.label} ${a.value}`))
            .toEqual(['Address 203.0.113.5', 'LAN 10.10.10.8', 'LAN 172.20.5.9']);
        expect(out.filter(a => !a.primary).map(a => a.label)).toEqual(['Docker', 'Overlay']);
    });

    it('folds every private address of a node that does not say what they are', () => {
        const out = addressesOf(node);
        expect(out.filter(a => a.primary).map(a => a.label)).toEqual(['Address']);
        expect(out.filter(a => !a.primary)).toHaveLength(4);
        expect(new Set(out.filter(a => !a.primary).map(a => a.label))).toEqual(new Set(['Private']));
    });
});
