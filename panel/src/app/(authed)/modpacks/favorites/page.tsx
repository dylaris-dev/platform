'use client';
import { ModrinthLibraryPanel } from '@/components/mods/ModrinthLibrary';
import styles from '@/components/modpacks/workspace.module.css';
export default function FavoritesPage() {
    return (
        <div className={styles.scroll}>
            <div className="max-w-4xl mx-auto">
                <ModrinthLibraryPanel />
            </div>
        </div>
    );
}
