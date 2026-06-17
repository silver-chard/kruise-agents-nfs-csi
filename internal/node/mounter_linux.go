//go:build linux

package node

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type linuxMounter struct {
	cfg Config
}

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

	stagePath := filepath.Join(m.cfg.StagingRoot, plan.PV.Name)
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
	return bindMountIntoContainerNamespace(m.cfg.HostProcRoot, pid, stagePath, plan.TargetPath)
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

func bindMountIntoContainerNamespace(procRoot string, targetPID int, stagePath, targetPath string) error {
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

	sourceFD, err := unix.OpenTree(unix.AT_FDCWD, stagePath, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("clone staged mount %s: %w", stagePath, err)
	}
	defer unix.Close(sourceFD)

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
		return fmt.Errorf("bind mount pv staging path to %s: %w", targetPath, err)
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
