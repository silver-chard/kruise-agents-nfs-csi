//go:build linux

package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

type linuxMounter struct {
	cfg          Config
	stagingLocks [stagingLockStripes]sync.Mutex
}

const stagingLockStripes = 128

func NewMounter(cfg Config) Mounter {
	return &linuxMounter{cfg: cfg}
}

func (m *linuxMounter) Mount(ctx context.Context, plan MountPlan) error {
	if !m.cfg.EnableMount {
		return ErrMountDisabled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.PV.Server == "" || plan.PV.Share == "" {
		return fmt.Errorf("pv %s is missing nfs server or share attributes", plan.PV.Name)
	}
	if plan.ContainerID == "" {
		return fmt.Errorf("container %s has empty container id", plan.ContainerName)
	}

	lock := m.stagingLock(plan.PV.Name)
	lock.Lock()
	defer lock.Unlock()

	stagePath := filepath.Join(m.cfg.StagingRoot, plan.PV.Name)
	if m.cfg.UnstageAfterMount {
		defer warnCleanupStagingPath(stagePath)
	}

	if err := os.MkdirAll(stagePath, 0o750); err != nil {
		return fmt.Errorf("create staging path %s: %w", stagePath, err)
	}
	if mounted, err := isMountPoint("/proc/self/mountinfo", stagePath); err != nil {
		return err
	} else if !mounted {
		if err := mountNFS(ctx, plan.PV, stagePath); err != nil {
			return err
		}
	}
	pid, err := findContainerPID(m.cfg.HostProcRoot, plan.PodUID, plan.ContainerID)
	if err != nil {
		return err
	}

	if err := m.bindMountWithModernAPI(pid, stagePath, plan.SourceSubPath, plan.TargetPath); err != nil {
		if !errors.Is(err, unix.ENOSYS) {
			return err
		}
		return m.bindMountWithLegacyAPI(pid, stagePath, plan.SourceSubPath, plan.TargetPath)
	}
	return nil
}

func (m *linuxMounter) Unmount(ctx context.Context, plan MountPlan) error {
	if !m.cfg.EnableMount {
		return ErrMountDisabled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.ContainerID == "" {
		return fmt.Errorf("container %s has empty container id", plan.ContainerName)
	}

	pid, err := findContainerPID(m.cfg.HostProcRoot, plan.PodUID, plan.ContainerID)
	if err != nil {
		return err
	}
	return unmountInContainerNamespace(m.cfg.HostProcRoot, pid, plan.TargetPath)
}

func (m *linuxMounter) IsMounted(ctx context.Context, plan MountPlan) (bool, error) {
	if !m.cfg.EnableMount {
		return false, ErrMountDisabled
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if plan.ContainerID == "" {
		return false, fmt.Errorf("container %s has empty container id", plan.ContainerName)
	}

	pid, err := findContainerPID(m.cfg.HostProcRoot, plan.PodUID, plan.ContainerID)
	if err != nil {
		return false, err
	}
	return isMountPoint(filepath.Join(m.cfg.HostProcRoot, strconv.Itoa(pid), "mountinfo"), plan.TargetPath)
}

func (m *linuxMounter) bindMountWithModernAPI(targetPID int, stagePath, sourceSubPath, targetPath string) error {
	sourceFD, sourceDescription, err := openTreeMountSource(stagePath, sourceSubPath)
	if err != nil {
		return err
	}
	defer unix.Close(sourceFD)
	return moveMountIntoContainerNamespace(m.cfg.HostProcRoot, targetPID, sourceFD, sourceDescription, targetPath)
}

func (m *linuxMounter) bindMountWithLegacyAPI(targetPID int, stagePath, sourceSubPath, targetPath string) error {
	source, err := openLegacyMountSource(stagePath, sourceSubPath)
	if err != nil {
		return err
	}
	defer source.Close()
	return legacyBindMountIntoContainerNamespace(m.cfg.HostProcRoot, targetPID, source.Path(), source.Description, targetPath)
}

func (m *linuxMounter) stagingLock(key string) *sync.Mutex {
	hash := uint32(2166136261)
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= 16777619
	}
	return &m.stagingLocks[hash%stagingLockStripes]
}

func warnCleanupStagingPath(stagePath string) {
	if err := cleanupStagingPath(stagePath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: cleanup staging path %s: %v\n", stagePath, err)
	}
}

func cleanupStagingPath(stagePath string) error {
	mounted, err := isMountPoint("/proc/self/mountinfo", stagePath)
	if err != nil {
		return err
	}
	if mounted {
		if err := unix.Unmount(stagePath, 0); err != nil {
			return fmt.Errorf("unmount staged path: %w", err)
		}
	}
	if err := os.Remove(stagePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove staged path: %w", err)
	}
	return nil
}

// CleanupStagingRoot unmounts stale per-PV staging mounts left by older wrapper runs.
func CleanupStagingRoot(stagingRoot string) error {
	mounts, err := mountPointsUnderRoot("/proc/self/mountinfo", stagingRoot)
	if err != nil {
		return err
	}

	var cleanupErr error
	for _, mountPath := range mounts {
		if err := unix.Unmount(mountPath, 0); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("unmount %s: %w", mountPath, err))
		}
	}

	entries, err := os.ReadDir(stagingRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cleanupErr
		}
		return errors.Join(cleanupErr, fmt.Errorf("read staging root %s: %w", stagingRoot, err))
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(stagingRoot, entry.Name())
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove staging path %s: %w", path, err))
		}
	}
	return cleanupErr
}

func mountPointsUnderRoot(mountInfoPath, root string) ([]string, error) {
	file, err := os.Open(mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("open mountinfo %s: %w", mountInfoPath, err)
	}
	defer file.Close()

	cleanRoot := filepath.Clean(root)
	prefix := cleanRoot + string(os.PathSeparator)
	var mounts []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mountPath := decodeMountInfoPath(fields[4])
		if strings.HasPrefix(mountPath, prefix) {
			mounts = append(mounts, mountPath)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan mountinfo %s: %w", mountInfoPath, err)
	}

	sort.Slice(mounts, func(i, j int) bool {
		return len(mounts[i]) > len(mounts[j])
	})
	return mounts, nil
}

func mountNFS(ctx context.Context, pv PersistentVolume, target string) error {
	remotePath := pv.Share
	if pv.SubDir != "" && pv.SubDir != "/" {
		remotePath = strings.TrimRight(remotePath, "/") + "/" + strings.TrimLeft(pv.SubDir, "/")
	}
	source := pv.Server + ":" + remotePath
	data := strings.Join(pv.MountOptions, ",")
	args := []string{"-t", "nfs"}
	if data != "" {
		args = append(args, "-o", data)
	}
	args = append(args, source, target)

	output, err := exec.CommandContext(ctx, "mount", args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail != "" {
			return fmt.Errorf("mount nfs source for pv %s: %w: %s", pv.Name, err, detail)
		}
		return fmt.Errorf("mount nfs source for pv %s: %w", pv.Name, err)
	}
	return nil
}

func openTreeMountSource(stagePath, sourceSubPath string) (int, string, error) {
	if sourceSubPath == "" {
		fd, err := unix.OpenTree(unix.AT_FDCWD, stagePath, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
		if err != nil {
			return -1, "", fmt.Errorf("clone staged mount %s: %w", stagePath, err)
		}
		return fd, stagePath, nil
	}

	dirFD, err := unix.Open(stagePath, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("open staged mount %s: %w", stagePath, err)
	}
	defer unix.Close(dirFD)

	currentFD := dirFD
	for _, segment := range strings.Split(sourceSubPath, "/") {
		if segment == "" || segment == "." {
			continue
		}
		nextFD, err := unix.Openat(currentFD, segment, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return -1, "", fmt.Errorf("%w: open source_sub_path %s under staged mount %s: %v", ErrBadSourceSubPath, sourceSubPath, stagePath, err)
		}
		if currentFD != dirFD {
			_ = unix.Close(currentFD)
		}
		currentFD = nextFD
	}
	if currentFD != dirFD {
		defer unix.Close(currentFD)
	}

	fd, err := unix.OpenTree(currentFD, "", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_EMPTY_PATH)
	if err != nil {
		return -1, "", fmt.Errorf("clone staged source_sub_path %s under %s: %w", sourceSubPath, stagePath, err)
	}
	return fd, filepath.Join(stagePath, sourceSubPath), nil
}

type legacyMountSource struct {
	FD          int
	Description string
}

func (s legacyMountSource) Close() {
	_ = unix.Close(s.FD)
}

func (s legacyMountSource) Path() string {
	return filepath.Join("/proc/self/fd", strconv.Itoa(s.FD))
}

func openLegacyMountSource(stagePath, sourceSubPath string) (legacyMountSource, error) {
	sourcePath := stagePath
	if sourceSubPath != "" {
		sourcePath = filepath.Join(stagePath, sourceSubPath)
	}

	fd, err := openDirectoryNoFollow(stagePath, sourceSubPath)
	if err != nil {
		return legacyMountSource{}, err
	}
	return legacyMountSource{FD: fd, Description: sourcePath}, nil
}

func openDirectoryNoFollow(stagePath, sourceSubPath string) (int, error) {
	dirFD, err := unix.Open(stagePath, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open staged mount %s: %w", stagePath, err)
	}
	if sourceSubPath == "" {
		return dirFD, nil
	}

	currentFD := dirFD
	for _, segment := range strings.Split(sourceSubPath, "/") {
		if segment == "" || segment == "." {
			continue
		}
		nextFD, err := unix.Openat(currentFD, segment, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if currentFD != dirFD {
				_ = unix.Close(currentFD)
			}
			_ = unix.Close(dirFD)
			return -1, fmt.Errorf("%w: open source_sub_path %s under staged mount %s: %v", ErrBadSourceSubPath, sourceSubPath, stagePath, err)
		}
		if currentFD != dirFD {
			_ = unix.Close(currentFD)
		}
		currentFD = nextFD
	}
	if currentFD == dirFD {
		return dirFD, nil
	}
	_ = unix.Close(dirFD)
	return currentFD, nil
}

func findContainerPID(procRoot, podUID, containerID string) (int, error) {
	normalizedID := normalizeContainerID(containerID)
	if normalizedID == "" {
		return 0, fmt.Errorf("container id is empty")
	}
	podNeedles := []string{podUID, strings.ReplaceAll(podUID, "-", "_")}
	containerNeedles := []string{normalizedID}
	if len(normalizedID) > 12 {
		containerNeedles = append(containerNeedles, normalizedID[:12])
	}

	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("read proc root %s: %w", procRoot, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isNumeric(entry.Name()) {
			continue
		}
		cgroupPath := filepath.Join(procRoot, entry.Name(), "cgroup")
		data, err := os.ReadFile(cgroupPath)
		if err != nil {
			continue
		}
		cgroup := string(data)
		if containsAny(cgroup, podNeedles) && containsAny(cgroup, containerNeedles) {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			return pid, nil
		}
	}
	return 0, fmt.Errorf("cannot find process for pod uid %s container %s", podUID, planSafeContainerID(containerID))
}

func moveMountIntoContainerNamespace(procRoot string, targetPID, sourceFD int, sourceDescription, targetPath string) error {
	targetMountInfo := filepath.Join(procRoot, strconv.Itoa(targetPID), "mountinfo")
	if mounted, err := isMountPoint(targetMountInfo, targetPath); err != nil {
		return err
	} else if mounted {
		return fmt.Errorf("target path %s is already a mount point", targetPath)
	}

	targetInContainerRoot := filepath.Join(procRoot, strconv.Itoa(targetPID), "root", strings.TrimPrefix(targetPath, "/"))
	if err := os.MkdirAll(targetInContainerRoot, 0o755); err != nil {
		return fmt.Errorf("create target path %s: %w", targetPath, err)
	}
	if mounted, err := isMountPoint(targetMountInfo, targetPath); err != nil {
		return err
	} else if mounted {
		return fmt.Errorf("target path %s is already a mount point", targetPath)
	}

	targetFD, err := unix.Open(targetInContainerRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open target path %s: %w", targetPath, err)
	}
	defer unix.Close(targetFD)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := unshareFS(); err != nil {
		return fmt.Errorf("unshare filesystem attributes: %w", err)
	}

	selfMountNS, err := os.Open(filepath.Join(procRoot, "self", "ns", "mnt"))
	if err != nil {
		return fmt.Errorf("open current mount namespace: %w", err)
	}
	defer selfMountNS.Close()

	targetMountNS, err := os.Open(filepath.Join(procRoot, strconv.Itoa(targetPID), "ns", "mnt"))
	if err != nil {
		return fmt.Errorf("open target mount namespace for pid %d: %w", targetPID, err)
	}
	defer targetMountNS.Close()

	if err := setns(int(targetMountNS.Fd()), syscall.CLONE_NEWNS); err != nil {
		return fmt.Errorf("enter target mount namespace for pid %d: %w", targetPID, err)
	}
	defer setns(int(selfMountNS.Fd()), syscall.CLONE_NEWNS)

	if err := unix.MoveMount(sourceFD, "", targetFD, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
		return fmt.Errorf("bind mount pv source %s to %s: %w", sourceDescription, targetPath, err)
	}
	return nil
}

func unmountInContainerNamespace(procRoot string, targetPID int, targetPath string) error {
	targetMountInfo := filepath.Join(procRoot, strconv.Itoa(targetPID), "mountinfo")
	if mounted, err := isMountPoint(targetMountInfo, targetPath); err != nil {
		return err
	} else if !mounted {
		return fmt.Errorf("target path %s is not a mount point", targetPath)
	}

	targetRoot, err := unix.Open(filepath.Join(procRoot, strconv.Itoa(targetPID), "root"), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open target container root for pid %d: %w", targetPID, err)
	}
	defer unix.Close(targetRoot)

	selfRoot, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open current root: %w", err)
	}
	defer unix.Close(selfRoot)

	selfCWD, err := unix.Open(".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open current working directory: %w", err)
	}
	defer unix.Close(selfCWD)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := unshareFS(); err != nil {
		return fmt.Errorf("unshare filesystem attributes: %w", err)
	}

	selfMountNS, err := os.Open(filepath.Join(procRoot, "self", "ns", "mnt"))
	if err != nil {
		return fmt.Errorf("open current mount namespace: %w", err)
	}
	defer selfMountNS.Close()

	targetMountNS, err := os.Open(filepath.Join(procRoot, strconv.Itoa(targetPID), "ns", "mnt"))
	if err != nil {
		return fmt.Errorf("open target mount namespace for pid %d: %w", targetPID, err)
	}
	defer targetMountNS.Close()

	if err := setns(int(targetMountNS.Fd()), syscall.CLONE_NEWNS); err != nil {
		return fmt.Errorf("enter target mount namespace for pid %d: %w", targetPID, err)
	}
	defer setns(int(selfMountNS.Fd()), syscall.CLONE_NEWNS)

	if err := enterRoot(targetRoot); err != nil {
		return fmt.Errorf("enter target root for pid %d: %w", targetPID, err)
	}
	defer restoreRoot(selfRoot, selfCWD)

	if err := unix.Unmount(targetPath, 0); err != nil {
		return fmt.Errorf("unmount target path %s: %w", targetPath, err)
	}
	return nil
}

func enterRoot(rootFD int) error {
	if err := unix.Fchdir(rootFD); err != nil {
		return fmt.Errorf("change directory to root fd: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("change directory to /: %w", err)
	}
	return nil
}

func restoreRoot(rootFD, cwdFD int) {
	if err := unix.Fchdir(rootFD); err != nil {
		fmt.Fprintf(os.Stderr, "warning: restore root directory: %v\n", err)
		return
	}
	if err := unix.Chroot("."); err != nil {
		fmt.Fprintf(os.Stderr, "warning: restore root: %v\n", err)
		return
	}
	if err := unix.Fchdir(cwdFD); err != nil {
		fmt.Fprintf(os.Stderr, "warning: restore working directory: %v\n", err)
	}
}

func legacyBindMountIntoContainerNamespace(procRoot string, targetPID int, sourcePath, sourceDescription, targetPath string) error {
	targetMountInfo := filepath.Join(procRoot, strconv.Itoa(targetPID), "mountinfo")
	if mounted, err := isMountPoint(targetMountInfo, targetPath); err != nil {
		return err
	} else if mounted {
		return fmt.Errorf("target path %s is already a mount point", targetPath)
	}

	targetInContainerRoot := filepath.Join(procRoot, strconv.Itoa(targetPID), "root", strings.TrimPrefix(targetPath, "/"))
	if err := os.MkdirAll(targetInContainerRoot, 0o755); err != nil {
		return fmt.Errorf("create target path %s: %w", targetPath, err)
	}
	if mounted, err := isMountPoint(targetMountInfo, targetPath); err != nil {
		return err
	} else if mounted {
		return fmt.Errorf("target path %s is already a mount point", targetPath)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := unshareFS(); err != nil {
		return fmt.Errorf("unshare filesystem attributes: %w", err)
	}

	selfMountNS, err := os.Open(filepath.Join(procRoot, "self", "ns", "mnt"))
	if err != nil {
		return fmt.Errorf("open current mount namespace: %w", err)
	}
	defer selfMountNS.Close()

	targetMountNS, err := os.Open(filepath.Join(procRoot, strconv.Itoa(targetPID), "ns", "mnt"))
	if err != nil {
		return fmt.Errorf("open target mount namespace for pid %d: %w", targetPID, err)
	}
	defer targetMountNS.Close()

	if err := setns(int(targetMountNS.Fd()), syscall.CLONE_NEWNS); err != nil {
		return fmt.Errorf("enter target mount namespace for pid %d: %w", targetPID, err)
	}
	defer setns(int(selfMountNS.Fd()), syscall.CLONE_NEWNS)

	if err := unix.Mount(sourcePath, targetInContainerRoot, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mount pv source %s to %s with legacy mount API: %w", sourceDescription, targetPath, err)
	}
	return nil
}

func setns(fd int, nstype int) error {
	_, _, errno := syscall.RawSyscall(sysSetns, uintptr(fd), uintptr(nstype), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func unshareFS() error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_UNSHARE, uintptr(syscall.CLONE_FS), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func isMountPoint(mountInfoPath, target string) (bool, error) {
	file, err := os.Open(mountInfoPath)
	if err != nil {
		return false, fmt.Errorf("open mountinfo %s: %w", mountInfoPath, err)
	}
	defer file.Close()

	cleanTarget := filepath.Clean(target)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		if decodeMountInfoPath(fields[4]) == cleanTarget {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, fmt.Errorf("scan mountinfo %s: %w", mountInfoPath, err)
	}
	return false, nil
}

func decodeMountInfoPath(value string) string {
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(value)
}

func normalizeContainerID(containerID string) string {
	_, value, found := strings.Cut(containerID, "://")
	if found {
		return value
	}
	return containerID
}

func containsAny(value string, needles []string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func isNumeric(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func planSafeContainerID(containerID string) string {
	normalized := normalizeContainerID(containerID)
	if len(normalized) <= 12 {
		return normalized
	}
	return normalized[:12] + "..."
}
