"use client";

import React, { useEffect, useState } from 'react';
import { API_URL } from '@/lib/api/core';

// A player's head, served by Core (GET /api/avatar/{name}), which fetches and
// keeps it. The panel used to load cravatar.eu directly: when that service went
// down, every head broke at once and nothing fell back.
export function playerHeadURL(name: string): string {
    return `${API_URL}/avatar/${encodeURIComponent(name)}`;
}

interface PlayerHeadProps {
    name: string;
    /** Shown instead when no head can be had. */
    fallback: React.ReactNode;
    className?: string;
}

export default function PlayerHead({ name, fallback, className }: PlayerHeadProps) {
    const [failed, setFailed] = useState(false);
    useEffect(() => setFailed(false), [name]);
    if (failed) return <>{fallback}</>;
    return (
        <img
            src={playerHeadURL(name)}
            alt=""
            // A player list can hold hundreds of rows; only the visible ones
            // should cost Core an avatar fetch.
            loading="lazy"
            decoding="async"
            className={className}
            style={{ imageRendering: 'pixelated' }}
            onError={() => setFailed(true)}
        />
    );
}
