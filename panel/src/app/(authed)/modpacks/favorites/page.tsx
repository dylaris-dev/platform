'use client';
import { useEffect } from 'react';
import { useRouter } from 'next/navigation';
import Link from 'next/link';
export default function LegacyFavoritesPage() {
    const router = useRouter();
    useEffect(() => router.replace('/favorites'), [router]);
    return <p className="p-6 text-sm">Opening your personal library. <Link href="/favorites">Continue to Favorites</Link></p>;
}
