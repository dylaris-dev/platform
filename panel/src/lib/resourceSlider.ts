/**
 * The stops of a stepped slider over a resource value (RAM in MB, cores, disk
 * in GB). With unlimitedAtEnd the stop after max means "no limit", which the
 * resource API spells 0.
 */
export interface SliderScale {
    min: number;
    step: number;
    max: number;
    unlimitedAtEnd?: boolean;
}

export function sliderStops(scale: SliderScale, value: number): number[] {
    // A value above max (typed into the field, or an older server) widens the
    // scale instead of being shown at a stop it does not have.
    // Bounded, so a typed 1e9 is not a billion stops on every keystroke.
    const top = Math.min(Math.max(scale.max, value > 0 ? Math.ceil(value / scale.step) * scale.step : 0), scale.max * 4);
    const stops: number[] = [];
    for (let v = scale.min; v <= top; v += scale.step) stops.push(v);
    if (scale.unlimitedAtEnd) stops.push(0);
    return stops;
}

/**
 * The stop nearest to value. Only drawing reads it: a value between stops is
 * kept as typed until the slider itself is moved.
 */
export function sliderPosition(stops: number[], value: number, unlimitedAtEnd?: boolean): number {
    if (unlimitedAtEnd && value <= 0) return stops.length - 1;
    const last = unlimitedAtEnd ? stops.length - 2 : stops.length - 1;
    let best = 0;
    for (let i = 1; i <= last; i++) {
        if (Math.abs(stops[i] - value) < Math.abs(stops[best] - value)) best = i;
    }
    return best;
}

// Without the node's capacity at hand the slider stops here; the field still
// takes any value.
const FALLBACK_RAM_MB = 32768;
const FALLBACK_CORES = 16;

export function ramScale(nodeRamMb?: number): SliderScale {
    const max = nodeRamMb && nodeRamMb >= 512 ? Math.floor(nodeRamMb / 512) * 512 : FALLBACK_RAM_MB;
    return { min: 512, step: 512, max };
}

export function cpuScale(nodeCores?: number): SliderScale {
    const max = nodeCores && nodeCores >= 1 ? Math.floor(nodeCores) : FALLBACK_CORES;
    return { min: 1, step: 1, max, unlimitedAtEnd: true };
}

export const DISK_SCALE_GB: SliderScale = { min: 10, step: 10, max: 100, unlimitedAtEnd: true };
