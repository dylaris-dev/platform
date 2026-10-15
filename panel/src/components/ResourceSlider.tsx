'use client';

import { SliderScale, sliderPosition, sliderStops } from '@/lib/resourceSlider';

interface ResourceSliderProps {
    label: string;
    value: number;
    onChange: (value: number) => void;
    scale: SliderScale;
    format: (value: number) => string;
}

/** A stepped slider under a resource field; the field stays the exact value. */
export default function ResourceSlider({ label, value, onChange, scale, format }: ResourceSliderProps) {
    const stops = sliderStops(scale, value);
    const show = (v: number) => (scale.unlimitedAtEnd && v <= 0 ? 'Unlimited' : format(v));
    return (
        <div className="flex flex-col gap-1">
            <input
                type="range"
                min={0}
                max={stops.length - 1}
                step={1}
                value={sliderPosition(stops, value, scale.unlimitedAtEnd)}
                onChange={e => onChange(stops[Number(e.target.value)])}
                aria-label={label}
                aria-valuetext={show(value)}
                className="w-full accent-(--accent) cursor-pointer"
            />
            <div className="flex justify-between text-[11px] font-mono text-(--base-06)">
                <span>{show(stops[0])}</span>
                <span>{show(stops[stops.length - 1])}</span>
            </div>
        </div>
    );
}
