'use client';

import { Slider, SliderControl, SliderIndicator, SliderThumb, SliderTrack } from '@ui-sdk/react/slider';
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
            <Slider
                min={0}
                max={stops.length - 1}
                step={1}
                value={sliderPosition(stops, value, scale.unlimitedAtEnd)}
                onValueChange={v => onChange(stops[v as number])}
                format={pos => show(stops[pos])}
            >
                <SliderControl className="flex h-5 items-center touch-none cursor-pointer data-disabled:cursor-not-allowed">
                    <SliderTrack className="relative h-1.5 w-full rounded-full bg-(--base-04)">
                        <SliderIndicator className="rounded-full bg-(--accent)" />
                        <SliderThumb
                            aria-label={label}
                            className="size-4 rounded-full bg-(--accent-light) border-2 border-(--base-00) has-focus-visible:[box-shadow:var(--focus-ring)] data-dragging:scale-110"
                        />
                    </SliderTrack>
                </SliderControl>
            </Slider>
            <div className="flex justify-between text-[11px] font-mono text-(--base-06)">
                <span>{show(stops[0])}</span>
                <span>{show(stops[stops.length - 1])}</span>
            </div>
        </div>
    );
}
