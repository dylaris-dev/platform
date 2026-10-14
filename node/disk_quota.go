package main

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// QuotaProvider uses Linux filesystem project quotas (xfs or ext4) for
// efficient disk usage tracking instead of expensive `du` scans.
type QuotaProvider struct {
	fsType     string // "xfs", "ext4", or "" (unavailable)
	basePath   string // absolute path to dylaris_data/servers/
	mountPoint string
	available  bool
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
		log.Printf("quota: %s is present but this mount will not accept a project id (%v) — "+
			"disk limits cannot be enforced here; usage will be measured with du instead", qp.fsType, err)
		return qp
	}

	qp.available = true
	log.Printf("quota: %s project quotas available and verified", qp.fsType)

	// Reconcile: assign quotas to all existing server directories
	qp.reconcileExisting()

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
	switch qp.fsType {
	case "xfs":
		cmd := exec.Command("xfs_quota", "-x", "-c",
			fmt.Sprintf("project -s -p %s %d", dir, pid), qp.mountPoint)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
		}
	case "ext4":
		cmd := exec.Command("chattr", "+P", "-p", fmt.Sprintf("%d", pid), dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
		}
	}
	return nil
}

// IsAvailable returns true if quota-based disk tracking can be used.
func (qp *QuotaProvider) IsAvailable() bool {
	return qp.available
}

// reconcileExisting assigns quotas to all existing server directories that
// were created before quota support was enabled.
func (qp *QuotaProvider) reconcileExisting() {
	entries, err := os.ReadDir(qp.basePath)
	if err != nil {
		return
	}
	var count int
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		uuid := e.Name()
		if err := qp.AssignQuota(uuid); err != nil {
			log.Printf("quota: reconcile %s: %v", uuid, err)
		} else {
			count++
		}
	}
	if count > 0 {
		log.Printf("quota: reconciled %d existing server(s)", count)
	}
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

	switch qp.fsType {
	case "xfs":
		cmd := exec.Command("xfs_quota", "-x", "-c",
			fmt.Sprintf("project -s -p %s %d", serverPath, pid), qp.mountPoint)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("quota: xfs_quota project setup: %s: %w", string(out), err)
		}
	case "ext4":
		cmd := exec.Command("chattr", "+P", "-p", fmt.Sprintf("%d", pid), serverPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("quota: chattr setup: %s: %w", string(out), err)
		}
	}

	return nil
}

// GetDiskUsage returns disk usage for a server via quota reporting.
// Returns nil if quotas are not available (caller should fall back to du).
func (qp *QuotaProvider) GetDiskUsage(uuid string) *DiskUsagePayload {
	if !qp.available {
		return nil
	}

	pid := projectID(uuid)
	var totalBytes, limitBytes int64

	// Parse usage and limit from a single subprocess call
	switch qp.fsType {
	case "xfs":
		totalBytes, limitBytes = qp.xfsGetUsageAndLimit(uuid, pid)
	case "ext4":
		totalBytes, limitBytes = qp.ext4GetUsageAndLimit(pid)
	}

	if totalBytes < 0 {
		return nil
	}

	// Quotas give total only; the sub-server breakdown is a readdir, shared with
	// the du path so both report the same shape (see scanSubServerSizes).
	return &DiskUsagePayload{
		Total:      totalBytes,
		Limit:      limitBytes,
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

	pid := projectID(uuid)

	if limitMB <= 0 {
		// Remove limit (set to 0 = unlimited)
		switch qp.fsType {
		case "xfs":
			cmd := exec.Command("xfs_quota", "-x", "-c",
				fmt.Sprintf("limit -p bhard=0 %d", pid), qp.mountPoint)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("quota: xfs remove limit: %s: %w", string(out), err)
			}
		case "ext4":
			cmd := exec.Command("setquota", "-P",
				fmt.Sprintf("%d", pid), "0", "0", "0", "0", qp.mountPoint)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("quota: ext4 remove limit: %s: %w", string(out), err)
			}
		}
		log.Printf("quota: removed disk limit for %s (project %d)", uuid, pid)
		return nil
	}

	limitKB := limitMB * 1024

	switch qp.fsType {
	case "xfs":
		cmd := exec.Command("xfs_quota", "-x", "-c",
			fmt.Sprintf("limit -p bhard=%dk %d", limitKB, pid), qp.mountPoint)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("quota: xfs set limit: %s: %w", string(out), err)
		}
	case "ext4":
		cmd := exec.Command("setquota", "-P",
			fmt.Sprintf("%d", pid), "0", fmt.Sprintf("%d", limitKB), "0", "0", qp.mountPoint)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("quota: ext4 set limit: %s: %w", string(out), err)
		}
	}

	log.Printf("quota: set disk limit %d MB for %s (project %d)", limitMB, uuid, pid)
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

// xfsGetUsageAndLimit parses xfs_quota report for usage and hard limit in one call.
// Returns (usedBytes, limitBytes). usedBytes=-1 on error, limitBytes=0 means unlimited.
// XFS report shows project name (uuid) or #projid depending on /etc/projid presence.
func (qp *QuotaProvider) xfsGetUsageAndLimit(uuid string, pid uint32) (int64, int64) {
	out, err := exec.Command("xfs_quota", "-x", "-c",
		"report -p -N -b", qp.mountPoint).Output()
	if err != nil {
		return -1, 0
	}

	pidStr := fmt.Sprintf("#%d", pid)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// Fields: name/projid used soft hard ...
		// XFS shows project name (uuid) if /etc/projid has entry, otherwise #projid
		if len(fields) >= 4 && (fields[0] == uuid || fields[0] == pidStr) {
			var used, limit int64
			if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				used = kb * 1024
			} else {
				return -1, 0
			}
			if kb, err := strconv.ParseInt(fields[3], 10, 64); err == nil && kb > 0 {
				limit = kb * 1024
			}
			return used, limit
		}
	}
	return -1, 0
}

// ext4GetUsageAndLimit parses repquota output for usage and hard limit in one call.
// Returns (usedBytes, limitBytes). usedBytes=-1 on error, limitBytes=0 means unlimited.
func (qp *QuotaProvider) ext4GetUsageAndLimit(pid uint32) (int64, int64) {
	out, err := exec.Command("repquota", "-Ps", qp.mountPoint).Output()
	if err != nil {
		return -1, 0
	}

	pidStr := fmt.Sprintf("%d", pid)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// Fields: projid status used soft hard ...
		if len(fields) >= 5 && fields[0] == pidStr {
			var used, limit int64
			if kb, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
				used = kb * 1024
			} else {
				return -1, 0
			}
			if kb, err := strconv.ParseInt(fields[4], 10, 64); err == nil && kb > 0 {
				limit = kb * 1024
			}
			return used, limit
		}
	}
	return -1, 0
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
		// Check if this mount point is a prefix of our path and is the longest match
		if strings.HasPrefix(path, mp) && len(mp) > len(bestMount) {
			if fs == "xfs" || fs == "ext4" {
				bestMount = mp
				bestFS = fs
			}
		}
	}

	return bestFS, bestMount
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
