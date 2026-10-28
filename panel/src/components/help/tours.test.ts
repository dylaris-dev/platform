import { describe, expect, it } from 'vitest';
import { contextualTour, tours } from './tours';

describe('guided example navigation', () => {
    it('covers deployment models and both authoring workflows without runnable commands', () => {
        expect(new Set(tours.map(tour => tour.id))).toEqual(new Set(['servers', 'byon', 'routeonly', 'content', 'custom-tabs', 'modpacks', 'favorites']));
        for (const tour of tours) {
            expect(tour.steps.length).toBeGreaterThan(1);
            expect(tour.access).toBeTruthy();
            for (const step of tour.steps) {
                expect(step.fields.length).toBeGreaterThan(1);
                expect(step.note).toBeTruthy();
                expect(JSON.stringify(step)).not.toMatch(/curl |wget |Bearer |https?:\/\//);
            }
        }
    });
    it('opens the closest workflow without navigating the real workspace', () => {
        expect(contextualTour('/modpacks/12/builds/23')).toBe('modpacks');
        expect(contextualTour('/servers/12/content')).toBe('content');
        expect(contextualTour('/favorites')).toBe('favorites');
        expect(contextualTour('/servers')).toBe('servers');
    });
});
