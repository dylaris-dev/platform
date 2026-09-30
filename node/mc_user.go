package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The Minecraft container runs as uid 1000, not as root.
//
// Everything inside it is software the TENANT chose - a plugin, a mod, a
// modpack from the internet. It used to run as root, which bought that software
// two things it has no business having: write access to the files the node keeps
// BESIDE the world in the same bind mount (.active_server, .dylaris.json,
// .dylaris-backups), and uid 0 on the host the moment a container escape works.
//
// The bind is the whole server directory, and the working directory is the
// sub-server inside it:
//
//	/data/                    root  - the node's, read-only to the tenant
//	/data/.active_server      root  - which sub-server runs
//	/data/.dylaris-backups/   root  - archives the panel serves back out
//	/data/<sub>/              1000  - the world, the tenant's to write
//
// So the split is one chown of the sub-server directory, and NOT of its parent.
// A tenant that cannot write /data cannot create an entry there either, which is
// what closes the symlink hazard grpc_backup.go documents: a plugin could plant
// "ln -s /whatever /data/.dylaris-backups/x" and the download RPC would follow
// it. That defence stays; this removes the ability to plant the link at all.

// mcUID is the uid/gid the Minecraft container runs as. It matches `dylaris` in
// the Core and Node image, which is 1000 there too.
//
// MC_RUN_AS overrides it, and exists for one case: an operator whose server data
// is already owned by some other uid and who would rather keep it than have the
// node rewrite ownership across every world on first start. Setting it to 0
// restores the old root behaviour, which is a decision an operator can make and
// the node should not silently prevent.
const defaultMCUser = 1000

func mcUser() int {
	if v := strings.TrimSpace(os.Getenv("MC_RUN_AS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
		log.Printf("mc-user: MC_RUN_AS %q is not a uid; using %d", v, defaultMCUser)
	}
	return defaultMCUser
}

// mcUserSpec is what Docker's User field wants, or "" to leave the image's own
// USER in force. Both are set on purpose: the image carries USER 1000 so a
// hand-run container is non-root too, and this is what an operator overrides.
func mcUserSpec() string {
	u := mcUser()
	if u == 0 {
		return "" // explicit opt-out: fall back to the image, which is root only on an old one
	}
	return fmt.Sprintf("%d:%d", u, u)
}

// ensureSubServerOwnership hands the sub-server directory to the uid the
// container runs as, and leaves everything above it alone.
//
// Called on the way into a container start, which makes it both the migration
// for existing data and the repair for anything the node wrote as root since the
// last start - an SFTP upload, a restored backup, an installed modpack.
//
// It walks only when it has to. The common case is a directory that already has
// the right owner, and that costs one Lstat rather than a walk of a world with
// two hundred thousand files in it.
func ensureSubServerOwnership(subDir string) error {
	if mcUser() == 0 {
		return nil // operator opted out; leave ownership exactly as it is
	}
	uid := mcUser()

	fi, err := os.Lstat(subDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Either nothing is installed yet, or the caller handed over a path
			// from the wrong space - the node sees /app/dylaris_data/..., the
			// host's daemon sees /var/lib/docker/volumes/... and the two look
			// equally plausible in a log. Said out loud because the failure is
			// otherwise perfectly silent: the container starts as uid 1000
			// against a root-owned world and the server exits a few seconds
			// later with status 0.
			log.Printf("mc-user: %s does not exist here; nothing to hand over (a server with nothing installed, or a path from the host's namespace)", subDir)
			return nil
		}
		return fmt.Errorf("stat %s: %w", subDir, err)
	}
	if ownedBy(fi, uid) {
		return nil
	}

	log.Printf("mc-user: handing %s to uid %d (first start after the non-root switch, or files written as root since the last one)", subDir, uid)
	n := 0
	err = filepath.WalkDir(subDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A single unreadable entry must not stop the server from starting.
			// It is reported and skipped: the world below it may still be fine,
			// and refusing to boot over one file would be the worse failure.
			log.Printf("mc-user: skipping %s: %v", p, err)
			return nil
		}
		// Lchown, not Chown: a symlink inside a world is the tenant's, and
		// following it would change the owner of whatever it points AT - which
		// on a link to /etc is exactly the escalation this change is closing.
		if err := os.Lchown(p, uid, uid); err != nil {
			log.Printf("mc-user: cannot chown %s: %v", p, err)
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", subDir, err)
	}
	log.Printf("mc-user: %d paths under %s now belong to uid %d", n, subDir, uid)
	return nil
}
