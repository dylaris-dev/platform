package main

import (
	"bufio"
	"context"
	"fmt"
	"hash/crc32"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// QuotaProvider uses Linux filesystem project quotas (xfs or ext4) for
// efficient disk usage tracking instead of expensive `du` scans.
type QuotaProvider struct {
	fsType     string // "xfs", "ext4", or "" (unavailable)
	basePath   string // absolute path to dylaris_data/servers/
	mountPoint string
	available  bool
	rdb        *redis.Client // keeps inode anchors; nil keeps none
}

// NewQuotaProvider detects the filesystem type and quota support for basePath.
func NewQuotaProvider(basePath string) *QuotaProvider {
	qp := &QuotaProvider{basePath: basePath}

	abs, err := filepath.Abs(basePath)
	if err != nil {
		log.Printf("quota: cannot resolve basePath: %v", err)
		return qp
	}
	qp.basePath = abs

	// Ensure base directory exists
	os.MkdirAll(abs, 0755)

	// Ensure /etc/projects and /etc/projid exist (bind-mounted from host)
	for _, f := range []string{"/etc/projects", "/etc/projid"} {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			if err := os.WriteFile(f, []byte(""), 0644); err != nil {
				log.Printf("quota: cannot create %s: %v", f, err)
			}
		}
	}

	qp.fsType, qp.mountPoint = detectFSType(abs)
	if qp.fsType == "" {
		log.Println("quota: filesystem is not ext4 or xfs – disk usage tracking disabled")
		return qp
	}

	log.Printf("quota: detected %s on %s for %s", qp.fsType, qp.mountPoint, abs)

	// Verify quota tools are available
	switch qp.fsType {
	case "xfs":
		if _, err := exec.LookPath("xfs_quota"); err != nil {
			log.Println("quota: xfs_quota not found – disk usage tracking disabled")
			return qp
		}
	case "ext4":
		if _, err := exec.LookPath("repquota"); err != nil {
			log.Println("quota: repquota not found – disk usage tracking disabled")
			return qp
		}
	}

	// Prove it rather than infer it. The two checks above establish that the
	// filesystem is a type that CAN carry project quotas and that the tool to set
	// them is installed - neither says the kernel and this particular mount will
	// accept one. A Docker Desktop bind mount from a Windows host reports ext4
	// (the VM's own disk) and has chattr, and every attempt to set a project id
	// on it fails with "Not supported".
	//
	// Before this probe the node concluded "ext4 project quotas available",
	// happily accepted every limit, and only discovered the truth once per server
	// in a log line nobody reads. Usage still worked (the du fallback), but the
	// limit was pure decoration and nothing above ever learned that.
	if err := qp.probeProjectSupport(); err != nil {
		log.Printf("quota: %s is present but this mount will not take a project quota (%v) — "+
			"disk limits cannot be enforced here; usage will be measured with du instead", qp.fsType, err)
		return qp
	}

	qp.available = true
	log.Printf("quota: %s project quotas available and verified", qp.fsType)

	return qp
}

// probeProjectSupport applies a project id to a scratch directory and removes it
// again, so availability is a demonstrated fact rather than an inference from
// the filesystem type. Returns nil when quotas really work here.
func (qp *QuotaProvider) probeProjectSupport() error {
	dir := filepath.Join(qp.basePath, ".dylaris-quota-probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("probe dir: %w", err)
	}
	defer os.RemoveAll(dir)

	pid := projectID("dylaris-quota-probe")
	if err := tagProjectTree(dir, pid); err != nil {
		return err
	}
	// A project id is not a limit. Setting the LIMIT is a separate quotactl
	// that needs CAP_SYS_ADMIN and a mount with project quotas enforced, and a
	// node that could tag directories but not limit them reported limits as
	// enforced while every one was decoration. Set one on the probe id and
	// take it off again.
	if err := qp.setProjectLimit(pid, 1024, projectInodeLimit(1024)); err != nil {
		return fmt.Errorf("setting a limit failed: %w", err)
	}
	// xfs is read back rather than trusted from the exit code: xfs_quota
	// reports a refused command on stderr and can still exit 0. setquota
	// (ext4) exits non-zero on a refusal.
	got := int64(1024 << 20)
	if qp.fsType == "xfs" {
		_, got, _, _ = qp.xfsGetUsageAndLimit("", pid)
	}
	if err := qp.setProjectLimit(pid, 0, 0); err != nil {
		return fmt.Errorf("removing the probe limit failed: %w", err)
	}
	if got != 1024<<20 {
		return fmt.Errorf("a 1024 MB limit was accepted but reads back as %d bytes", got)
	}
	return nil
}

// projectInodeLimit is the inode hard limit that goes with a disk limit (owner
// decision 2026-11-26): 64 per MB, at least 1M so a real modpack fits. Empty
// files cost no blocks, so without it one tenant could use up the inodes of
// the filesystem every server on the path shares. No disk limit, no inode limit.
func projectInodeLimit(limitMB int64) int64 {
	if limitMB <= 0 {
		return 0
	}
	return max(1000000, 64*limitMB)
}

// projectInodeLimitFor is projectInodeLimit for a project already holding
// inodes files. One holding more than the formula allows (a map render of
// millions of tiles) keeps a quarter of headroom over them instead (owner
// ruling 2026-11-26): put below them, the node update that brought the inode
// limit failed its every create and the guard stopped a running server.
// Granted once, not on every SetLimit: see inodeAnchor.
func projectInodeLimitFor(limitMB, inodes int64) int64 {
	l := projectInodeLimit(limitMB)
	if l > 0 && inodes > l {
		return (inodes*5 + 3) / 4
	}
	return l
}

// inodeAnchorKeyFmt holds the file cap a server was granted over its formula,
// 0 for none (R60-04/05). Node-scoped next to the disk limit, so it survives a
// restart and a storage move, and not in .node_config.json: the server's
// directory is the tenant's /data.
const inodeAnchorKeyFmt = "node:%s:server:%s:inode_anchor"

func inodeAnchorKey(uuid string) string { return fmt.Sprintf(inodeAnchorKeyFmt, nodeID, uuid) }

// inodeAnchor is the file cap a project keeps over its formula; files and
// ihard are what it holds and its inode limit before this SetLimit. Granted
// from the files only once, when there is no anchor yet and the project has
// no inode limit (one limited before inode limits existed). A lift's put-back
// or a resize reuses it: files made while the limit was off never raise it.
// Without Redis's answer it is the limit the project has, never a new count.
func (qp *QuotaProvider) inodeAnchor(uuid string, limitMB, files, ihard int64) int64 {
	if qp.rdb == nil {
		return ihard
	}
	ctx := context.Background()
	v, err := qp.rdb.Get(ctx, inodeAnchorKey(uuid)).Int64()
	if err == nil {
		return v
	}
	if err != redis.Nil {
		log.Printf("quota: %s: inode anchor not read (%v) - keeping its inode limit", uuid, err)
		return ihard
	}
	anchor := ihard
	if ihard == 0 {
		anchor = projectInodeLimitFor(limitMB, files)
	}
	if anchor <= projectInodeLimit(limitMB) {
		anchor = 0
	}
	if ok, err := qp.rdb.SetNX(ctx, inodeAnchorKey(uuid), anchor, 0).Result(); err != nil {
		log.Printf("quota: %s: inode anchor not saved: %v", uuid, err)
	} else if !ok {
		// A concurrent SetLimit granted it first; that one stands.
		if v, err := qp.rdb.Get(ctx, inodeAnchorKey(uuid)).Int64(); err == nil {
			return v
		}
	}
	return anchor
}

// projectLimitCommand is the command that sets a project's block hard limit to
// limitMB and its inode hard limit to inodes; 0 clears either.
func (qp *QuotaProvider) projectLimitCommand(pid uint32, limitMB, inodes int64) ([]string, error) {
	limitMB, inodes = max(limitMB, 0), max(inodes, 0)
	switch qp.fsType {
	case "xfs":
		return []string{"xfs_quota", "-x", "-c",
			fmt.Sprintf("limit -p bhard=%dk ihard=%d %d", limitMB*1024, inodes, pid), qp.mountPoint}, nil
	case "ext4":
		return []string{"setquota", "-P",
			fmt.Sprintf("%d", pid), "0", fmt.Sprintf("%d", limitMB*1024), "0", fmt.Sprintf("%d", inodes), qp.mountPoint}, nil
	}
	return nil, fmt.Errorf("no project quotas on %q", qp.fsType)
}

// quotaReportOutput runs a quota report and quotaLimitOutput a limit command;
// vars so tests can stand in for the tools. A report's stderr stays out of
// what is parsed.
var (
	quotaReportOutput = func(args ...string) ([]byte, error) { return exec.Command(args[0], args[1:]...).Output() }
	quotaLimitOutput  = func(args ...string) ([]byte, error) { return exec.Command(args[0], args[1:]...).CombinedOutput() }
)

// setProjectLimit sets (or with 0 removes) the hard block and inode limits of
// a project.
func (qp *QuotaProvider) setProjectLimit(pid uint32, limitMB, inodes int64) error {
	args, err := qp.projectLimitCommand(pid, limitMB, inodes)
	if err != nil {
		return err
	}
	if out, err := quotaLimitOutput(args...); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// report is the whole mount's quota report, one subprocess for every project
// on it; usageIn reads one project from it.
func (qp *QuotaProvider) report() (string, error) {
	var args []string
	switch qp.fsType {
	case "xfs":
		args = []string{"xfs_quota", "-x", "-c", "report -p -N -b -i", qp.mountPoint}
	case "ext4":
		// Not -s: that prints the block columns in human units ("1024M") that do
		// not parse as numbers. Without it they are 1 KiB quota blocks. -n prints
		// every project as "#<id>"; by default repquota prints the name from
		// /etc/projid (the server UUID), so a bare id never matched any line.
		args = []string{"repquota", "-P", "-n", qp.mountPoint}
	default:
		return "", fmt.Errorf("no project quotas on %q", qp.fsType)
	}
	out, err := quotaReportOutput(args...)
	return string(out), err
}

// usageIn is (usedBytes, limitBytes, inodes, inodeLimit) of one project in a
// report; usedBytes -1 when the project is not in it.
func (qp *QuotaProvider) usageIn(report, uuid string, pid uint32) (int64, int64, int64, int64) {
	if qp.fsType == "xfs" {
		return parseXFSReport(report, uuid, pid)
	}
	return parseRepquota(report, pid)
}

// backfillInodeLimits gives every server on this mount that has a disk limit
// but not its inode limit one: a server limited before inode limits existed
// had none, and a counted server's boot reaches no SetLimit. One report for
// the whole mount, run after the boot rather than before the heartbeat: a
// usage read per server ran a du over every sub-server as well (R59-05). The
// project's own block limit, not the known one: during a grace it is the
// ceiling. Once set, the next run finds it there and does nothing.
//
// ponytail: an update_resources landing between the report and the SetLimit
// here can have its new limit replaced by the one read; a per-server limit
// lock closes it, as for the recount's.
func (qp *QuotaProvider) backfillInodeLimits(rdb *redis.Client, storage *StorageManager) {
	if !qp.available {
		return
	}
	report, err := qp.report()
	if err != nil {
		log.Printf("quota: inode limits on %s not checked, the report failed: %v", qp.mountPoint, err)
		return
	}
	ctx := context.Background()
	for _, uuid := range qp.serverDirs(storage) {
		used, limit, inodes, inodeLimit := qp.usageIn(report, uuid, projectID(uuid))
		if used < 0 || limit <= 0 || inodeLimit >= projectInodeLimit(limit>>20) {
			continue
		}
		// A recount or a restore holds the limit off; its put-back carries the
		// inode limit, and a limit set here mid-tag refused the tag at bhard.
		if diskLiftHeld(uuid) || (rdb != nil && rdb.Exists(ctx, diskLiftedKey(uuid)).Val() != 0) {
			continue
		}
		if err := qp.setLimit(uuid, limit>>20, inodes, inodeLimit); err != nil {
			log.Printf("quota: add the inode limit of %s: %v", uuid, err)
		}
	}
}

// IsAvailable returns true if quota-based disk tracking can be used.
func (qp *QuotaProvider) IsAvailable() bool {
	return qp.available
}

// reconcileExisting assigns quotas to all existing server directories that
// were created before quota support was enabled, or whose data was never
// counted (ext4 before the recursive tag). Once per server: see reconcileServer.
func (qp *QuotaProvider) reconcileExisting(rdb *redis.Client, storage *StorageManager) {
	if !qp.available {
		return
	}
	var count int
	for _, uuid := range qp.serverDirs(storage) {
		if counted, err := reconcileServer(context.Background(), rdb, qp, uuid); err != nil {
			log.Printf("quota: reconcile %s: %v", uuid, err)
		} else if counted {
			count++
		}
	}
	if count > 0 {
		log.Printf("quota: reconciled %d existing server(s)", count)
	}
	// The recount above skips counted servers, so without this pass archives
	// an older node charged to the server stayed charged until that server's
	// next node-local backup, which a paused or S3 job never makes.
	qp.tagBackupDirs(storage)
}

// tagBackupDirs re-decides the project of every server's backup folder on
// this path for the current share mode.
func (qp *QuotaProvider) tagBackupDirs(storage *StorageManager) {
	if !qp.available {
		return
	}
	var tagged, failed int
	for _, uuid := range qp.serverDirs(storage) {
		if _, err := os.Lstat(filepath.Join(qp.basePath, uuid, backupDirName)); err != nil {
			continue
		}
		if err := qp.TagBackupDir(uuid); err != nil {
			log.Printf("%v", err)
			failed++
			continue
		}
		tagged++
	}
	if tagged+failed > 0 {
		log.Printf("quota: backup folders on %s tagged for share mode %t: %d ok, %d failed", qp.basePath, backupShareQuota(), tagged, failed)
	}
}

// serverDirs lists the servers that live on this provider's path. A copy a
// storage move left on another path is not the server: counting it set the
// once-marker, and the real tree was never counted.
func (qp *QuotaProvider) serverDirs(storage *StorageManager) []string {
	entries, err := os.ReadDir(qp.basePath)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if storage != nil && storage.GetServerPath(e.Name()) != qp.basePath {
			continue
		}
		out = append(out, e.Name())
	}
	return out
}

// projectID generates a deterministic project ID from a server UUID using CRC32.
func projectID(uuid string) uint32 {
	return crc32.ChecksumIEEE([]byte(uuid))
}

// AssignQuota sets up project quota tracking for a server directory.
func (qp *QuotaProvider) AssignQuota(uuid string) error {
	if !qp.available {
		return nil
	}

	serverPath := filepath.Join(qp.basePath, uuid)
	pid := projectID(uuid)

	// Write to /etc/projects and /etc/projid
	if err := ensureProjectEntry(pid, serverPath); err != nil {
		return fmt.Errorf("quota: failed to write project entry: %w", err)
	}
	if err := ensureProjIDEntry(pid, uuid); err != nil {
		return fmt.Errorf("quota: failed to write projid entry: %w", err)
	}

	// Everything already in the tree too, not only the directory: data a move,
	// a restore or the first start with quotas put there counts from now on.
	// The walk is fd-based because the tenant writes to this tree while it
	// runs; see walkProjectTree.
	if err := tagProjectTree(serverPath, pid); err != nil {
		return fmt.Errorf("quota: project setup (%s): %w", qp.fsType, err)
	}
	// Logged, not returned: the server's own tree is counted either way.
	if err := qp.TagBackupDir(uuid); err != nil {
		log.Printf("%v", err)
	}
	return nil
}

// setBackupDirProject is setBackupTree; a var so tests can see the calls.
var setBackupDirProject = setBackupTree

// isNodeLocalArchive reports whether name looks like a node-local archive or a
// run's partial: every storage key Core makes ends in .tar.gz.
func isNodeLocalArchive(name string) bool {
	return strings.HasSuffix(strings.TrimSuffix(name, partialBackupSuffix), ".tar.gz")
}

// backupEntryProject is the project of an entry in a server's backup folder,
// depth 0 being the folder itself. In separate-budget mode only the node's own
// archives come off the server: a tenant that is root in its container
// (MC_RUN_AS=0) writes, renames or hard-links into the folder too, and a walk
// that untagged everything there took its world out of the disk limit (R53-03).
// A linked file keeps the server's project, as its other name is in the world.
//
// ponytail: a root tenant can still park data under an archive-shaped name;
// matching against Core's run list closes that if it shows up.
func backupEntryProject(name string, depth int, dir, multiLinked bool, serverPid uint32, share bool) uint32 {
	if share {
		return serverPid
	}
	if depth == 0 || (depth == 1 && !dir && !multiLinked && isNodeLocalArchive(name)) {
		return 0
	}
	return serverPid
}

// TagBackupDir charges the server's node-local backup folder to the server's
// project only while Core shares the server's quota with backups. Otherwise
// it is a separate budget, held by Core's per-server backup quota: charged to
// the server, the archives filled its disk limit, the kernel refused its world
// saves and the disk guard stopped it. Archives an older node tagged come off
// here too: at boot and on a share-mode change for every server (see
// tagBackupDirs), and before each node-local archive.
//
// The folder is a flat list of archives, so the walk is short. Only the
// archives come off the server; see backupEntryProject.
func (qp *QuotaProvider) TagBackupDir(uuid string) error {
	if !qp.available {
		return nil
	}
	dir := filepath.Join(qp.basePath, uuid, backupDirName)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	}
	if err := setBackupDirProject(dir, projectID(uuid), backupShareQuota()); err != nil {
		return fmt.Errorf("quota: backup folder project (%s): %w", qp.fsType, err)
	}
	return nil
}

// TagsMoved reports whether anything in the server's tree left its project;
// see retagIfMoved. A walk error is logged: past a hit it is beside the point,
// short of one it means entries went unread.
func (qp *QuotaProvider) TagsMoved(uuid string) bool {
	if !qp.available {
		return false
	}
	moved, err := projectTagsMoved(filepath.Join(qp.basePath, uuid), backupDirName, projectID(uuid))
	if err != nil && !moved {
		log.Printf("quota: tag check %s: %v", uuid, err)
	}
	if moved {
		return true
	}
	dir := filepath.Join(qp.basePath, uuid, backupDirName)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return false
	}
	moved, err = backupTagsMoved(dir, projectID(uuid), backupShareQuota())
	if err != nil && !moved {
		log.Printf("quota: backup folder tag check %s: %v", uuid, err)
	}
	return moved
}

// GetDiskUsage returns disk usage for a server via quota reporting.
// Returns nil if quotas are not available (caller should fall back to du).
func (qp *QuotaProvider) GetDiskUsage(uuid string) *DiskUsagePayload {
	if !qp.available {
		return nil
	}

	pid := projectID(uuid)
	var totalBytes, limitBytes, inodes, inodeLimit int64

	// Parse usage and limit from a single subprocess call
	switch qp.fsType {
	case "xfs":
		totalBytes, limitBytes, inodes, inodeLimit = qp.xfsGetUsageAndLimit(uuid, pid)
	case "ext4":
		totalBytes, limitBytes, inodes, inodeLimit = qp.ext4GetUsageAndLimit(pid)
	}

	if totalBytes < 0 {
		return nil
	}

	// Quotas give total only; the sub-server breakdown is a readdir, shared with
	// the du path so both report the same shape (see scanSubServerSizes).
	return &DiskUsagePayload{
		Total:      totalBytes,
		Limit:      limitBytes,
		Inodes:     inodes,
		InodeLimit: inodeLimit,
		SubServers: subServerSizesFresh(filepath.Join(qp.basePath, uuid)),
	}
}

// SetLimit sets a hard disk quota limit for a server. limitMB=0 removes the limit.
func (qp *QuotaProvider) SetLimit(uuid string, limitMB int64) error {
	if !qp.available {
		if limitMB > 0 {
			log.Printf("quota: cannot enforce disk limit for %s – quotas not available", uuid)
		}
		return nil
	}

	inodes, inodeLimit := int64(-1), int64(0)
	if limitMB > 0 {
		// The files the project holds now and its inode limit, for its
		// anchor; see inodeAnchor.
		if report, err := qp.report(); err != nil {
			log.Printf("quota: %s: files not read (%v) - setting its disk limit without an inode limit", uuid, err)
		} else {
			_, _, inodes, inodeLimit = qp.usageIn(report, uuid, projectID(uuid))
		}
	}
	return qp.setLimit(uuid, limitMB, inodes, inodeLimit)
}

// setLimit is SetLimit for a project holding inodes files under inodeLimit;
// inodes -1 is not known, which sets no inode limit rather than one that may
// be below them.
func (qp *QuotaProvider) setLimit(uuid string, limitMB, inodes, inodeLimit int64) error {
	pid := projectID(uuid)
	ihard := int64(0)
	if inodes >= 0 && limitMB > 0 {
		ihard = max(projectInodeLimit(limitMB), qp.inodeAnchor(uuid, limitMB, inodes, inodeLimit))
	}
	if err := qp.setProjectLimit(pid, limitMB, ihard); err != nil {
		return fmt.Errorf("quota: %s set limit %d MB: %w", qp.fsType, limitMB, err)
	}
	if limitMB <= 0 {
		log.Printf("quota: removed disk limit for %s (project %d)", uuid, pid)
		return nil
	}
	if ihard > projectInodeLimit(limitMB) {
		log.Printf("quota: %s was granted %d files, more than the %d its disk limit allows (it holds %d)", uuid, ihard, projectInodeLimit(limitMB), inodes)
	}
	log.Printf("quota: set disk limit %d MB, %d inodes for %s (project %d)", limitMB, ihard, uuid, pid)
	return nil
}

// RemoveQuota cleans up project entries for a deleted server.
func (qp *QuotaProvider) RemoveQuota(uuid string) {
	if !qp.available {
		return
	}
	pid := projectID(uuid)
	removeProjectEntry(pid)
	removeProjIDEntry(pid)
}

// xfsGetUsageAndLimit parses xfs_quota report for usage and hard limits in one call.
// Returns (usedBytes, limitBytes, inodes, inodeLimit). usedBytes=-1 on error, a
// limit of 0 means unlimited (for inodes also: not read).
func (qp *QuotaProvider) xfsGetUsageAndLimit(uuid string, pid uint32) (int64, int64, int64, int64) {
	out, err := qp.report()
	if err != nil {
		return -1, 0, 0, 0
	}
	return parseXFSReport(out, uuid, pid)
}

// parseXFSReport reads one project's line from `report -p -N -b -i` output.
// XFS report shows project name (uuid) or #projid depending on /etc/projid presence.
func parseXFSReport(out, uuid string, pid uint32) (int64, int64, int64, int64) {
	pidStr := fmt.Sprintf("#%d", pid)
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// Fields: name/projid used soft hard warns [grace] used soft hard warns [grace]
		// XFS shows project name (uuid) if /etc/projid has entry, otherwise #projid
		if len(fields) >= 4 && (fields[0] == uuid || fields[0] == pidStr) {
			var used, limit int64
			if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				used = kb * 1024
			} else {
				return -1, 0, 0, 0
			}
			if kb, err := strconv.ParseInt(fields[3], 10, 64); err == nil && kb > 0 {
				limit = kb * 1024
			}
			// The block grace is one bracketed column of one or more words
			// ("[--------]", "[7 days]"), so the inode columns start after
			// the first word that closes it.
			var inodes, inodeLimit int64
			for i := 5; i+3 < len(fields); i++ {
				if strings.HasSuffix(fields[i], "]") {
					inodes, inodeLimit = parseInodeColumns(fields[i+1], fields[i+3])
					break
				}
			}
			return used, limit, inodes, inodeLimit
		}
	}
	return -1, 0, 0, 0
}

// parseInodeColumns reads a report's inode used and hard columns. Unparsable
// reads as 0, 0: no inode limit, so never full.
func parseInodeColumns(usedField, hardField string) (int64, int64) {
	used, err1 := strconv.ParseInt(usedField, 10, 64)
	hard, err2 := strconv.ParseInt(hardField, 10, 64)
	if err1 != nil || err2 != nil || used < 0 || hard < 0 {
		return 0, 0
	}
	return used, hard
}

// ext4GetUsageAndLimit parses repquota output for usage and hard limits in one call.
// Returns what xfsGetUsageAndLimit returns.
func (qp *QuotaProvider) ext4GetUsageAndLimit(pid uint32) (int64, int64, int64, int64) {
	out, err := qp.report()
	if err != nil {
		return -1, 0, 0, 0
	}
	return parseRepquota(out, pid)
}

// parseRepquota reads one project's line from `repquota -P -n` output.
// Returns (usedBytes, limitBytes, inodes, inodeLimit) like ext4GetUsageAndLimit.
func parseRepquota(out string, pid uint32) (int64, int64, int64, int64) {
	pidStr := fmt.Sprintf("#%d", pid)
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// Fields: #projid flags used soft hard [grace] files soft hard [grace].
		// The block grace column is empty under the block soft limit, which
		// the first flag ("+-") marks, so only then does it shift the files.
		if len(fields) >= 5 && fields[0] == pidStr {
			var used, limit int64
			if kb, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
				used = kb * 1024
			} else {
				return -1, 0, 0, 0
			}
			if kb, err := strconv.ParseInt(fields[4], 10, 64); err == nil && kb > 0 {
				limit = kb * 1024
			}
			files := 5
			if strings.HasPrefix(fields[1], "+") {
				files = 6
			}
			var inodes, inodeLimit int64
			if files+2 < len(fields) {
				inodes, inodeLimit = parseInodeColumns(fields[files], fields[files+2])
			}
			return used, limit, inodes, inodeLimit
		}
	}
	return -1, 0, 0, 0
}

// detectFSType finds the filesystem type and mount point for a given path.
func detectFSType(path string) (fsType, mountPoint string) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", ""
	}
	defer f.Close()

	var bestMount string
	var bestFS string

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mp := fields[1]
		fs := fields[2]
		if mountCovers(mp, path) && len(mp) > len(bestMount) {
			if fs == "xfs" || fs == "ext4" {
				bestMount = mp
				bestFS = fs
			}
		}
	}

	return bestFS, bestMount
}

// mountCovers reports whether path lies on the mount at mp. A plain prefix
// bound /mnt/data2 to the /mnt/data mount and its quota database.
func mountCovers(mp, path string) bool {
	return path == mp || strings.HasPrefix(path, strings.TrimSuffix(mp, "/")+"/")
}

// ensureProjectEntry adds a line to /etc/projects if not present.
func ensureProjectEntry(pid uint32, path string) error {
	line := fmt.Sprintf("%d:%s", pid, path)
	return ensureLine("/etc/projects", line, fmt.Sprintf("%d:", pid))
}

// ensureProjIDEntry adds a line to /etc/projid if not present.
func ensureProjIDEntry(pid uint32, name string) error {
	line := fmt.Sprintf("%s:%d", name, pid)
	return ensureLine("/etc/projid", line, fmt.Sprintf(":%d", pid))
}

func ensureLine(filePath, line, matchSuffix string) error {
	// Read existing lines
	existing, _ := os.ReadFile(filePath)
	for _, l := range strings.Split(string(existing), "\n") {
		if strings.Contains(l, matchSuffix) {
			return nil // already present
		}
	}

	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

func removeProjectEntry(pid uint32) {
	removeLine("/etc/projects", fmt.Sprintf("%d:", pid))
}

func removeProjIDEntry(pid uint32) {
	removeLine("/etc/projid", fmt.Sprintf(":%d", pid))
}

func removeLine(filePath, match string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" && !strings.Contains(line, match) {
			kept = append(kept, line)
		}
	}
	os.WriteFile(filePath, []byte(strings.Join(kept, "\n")+"\n"), 0644)
}
