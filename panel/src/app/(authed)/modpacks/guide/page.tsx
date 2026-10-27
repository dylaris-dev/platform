'use client';
import Link from 'next/link';
import styles from '@/components/modpacks/workspace.module.css';
export default function PackGuide() {
    return (
        <div className={styles.scroll}>
            <article className="max-w-4xl mx-auto space-y-6">
                <header>
                    <p className="mono-label text-(--accent-light)">Modpack authoring</p>
                    <h1 className="text-2xl font-display font-semibold mt-2">
                        From the first pack to a playable build
                    </h1>
                    <p className="text-sm text-(--base-06) mt-3">
                        A pack is the project. A build pins Minecraft, loader, mods and config files. Create a new draft
                        for changes; keep published releases reproducible.
                    </p>
                </header>
                {[
                    [
                        '1. Create or join a pack',
                        'Choose Modpacks and New pack. Give it a name and summary. Owners can invite existing users through Collaborators or Access. Read, edit/publish and delete rights are independent.',
                    ],
                    [
                        '2. Start a build',
                        'Choose New build. Start empty or select an existing build. Choose Minecraft, loader and loader version, then enter a pack version and description. Compatible loader choices are fetched automatically.',
                    ],
                    [
                        '3. Add mods and resources',
                        'Use Add content to search Modrinth and expand project details. Choose a compatible version. Required dependencies are resolved where possible; inspect warnings. Client and Server choose where each item is installed. At least one must remain selected.',
                    ],
                    [
                        '4. Edit files',
                        'Open Configs & scripts for the file browser/editor. Create folders and files, upload existing configs/KubeJS folders, and preserve paths relative to the Minecraft instance. Editing a copied draft does not modify the source.',
                    ],
                    [
                        '5. Change Minecraft or loader',
                        'In New build select the source, then change Minecraft or loader. The availability preview lists compatible and missing linked content. You must explicitly confirm dropping unavailable items. Local configs/scripts require manual review and are not automatically converted.',
                    ],
                    [
                        '6. Favorites and daily alerts',
                        'Star Modrinth projects in Server Content or this builder. Use the bell to choose target Minecraft releases and loader. Build alerts capture a fixed set of linked mods. Specific targets finish after one notification; future-release checks stay active. Reading never deletes the inbox message.',
                    ],
                    [
                        '7. Test a client and server',
                        'Use the build detail download buttons for installation guides. Import the client .mrpack in Modrinth App. In DYLARIS server setup select My modpacks and the build, or install the server ZIP manually with the matching Java, Minecraft and loader.',
                    ],
                    [
                        '8. Publish or share',
                        'Publish uploads through the owner’s stored Modrinth token. Modrinth controls moderation and visibility. After publication, Install in Modrinth App opens that exact uploaded version. Sharing creates separate revocable links, subject to the administrator’s feature setting.',
                    ],
                    [
                        'Where the bytes live',
                        'The operator chooses Settings > Modpacks: local paths, a named S3-compatible connection, or Core storage. Linked mods are stored as pinned references until rendering. Uploaded files/configs and rendered server archives use that storage provider. Draft downloads do not publish or freeze a build. A database backup alone does not back up object storage.',
                    ],
                ].map(([title, body]) => (
                    <section key={title} className="card p-5">
                        <h2 className="font-medium text-base">{title}</h2>
                        <p className="text-sm text-(--base-07) leading-relaxed mt-2">{body}</p>
                    </section>
                ))}
                <div className="flex gap-2">
                    <Link className="btn btn-primary" href="/modpacks?create=1">
                        Create a pack
                    </Link>
                    <Link className="btn btn-secondary" href="/modpacks/favorites">
                        Favorites & alerts
                    </Link>
                </div>
            </article>
        </div>
    );
}
