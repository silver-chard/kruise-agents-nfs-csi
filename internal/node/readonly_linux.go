//go:build linux

package node

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// CSI may implement RO in its userspace client while publishing an RW bind.
// Tighten only the wrapper-owned source mount before exposing a consumer. This
// also makes legacy RO injection safe on kernels without mount_setattr.
func ensureJuiceFSSourceAccess(stage string, readOnly bool) error {
	sourceFD, err := openDirectoryNoFollow(stage, "", false, 0)
	if err != nil {
		return err
	}
	defer unix.Close(sourceFD)
	mounted, sourceRO, err := inspectMountFDAccess("/proc/thread-self/mountinfo", stage, sourceFD)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("JuiceFS CSI source is not a mount point; no target was injected")
	}
	if sourceRO == readOnly {
		return nil
	}
	if !readOnly {
		return fmt.Errorf("JuiceFS CSI source is not writable; no target was injected")
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(sourceFD, &fs); err != nil {
		return err
	}
	// MS_BIND remount changes this mount's flags, not the shared FUSE
	// superblock or another Pod's source. Preserve existing security flags.
	flags := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
	for _, pair := range [][2]uint64{
		{unix.ST_NOSUID, unix.MS_NOSUID}, {unix.ST_NODEV, unix.MS_NODEV},
		{unix.ST_NOEXEC, unix.MS_NOEXEC}, {unix.ST_NOATIME, unix.MS_NOATIME},
		{unix.ST_NODIRATIME, unix.MS_NODIRATIME}, {unix.ST_RELATIME, unix.MS_RELATIME},
	} {
		if uint64(fs.Flags)&pair[0] != 0 {
			flags |= uintptr(pair[1])
		}
	}
	if err := unix.Mount("", fmt.Sprintf("/proc/self/fd/%d", sourceFD), "", flags, ""); err != nil {
		return fmt.Errorf("enforce read-only JuiceFS source: %w", err)
	}
	if matched, err := mountFDMatchesAccessMode("/proc/thread-self/mountinfo", stage, sourceFD, true); err != nil {
		return err
	} else if !matched {
		return fmt.Errorf("JuiceFS source remains writable after enforcement; no target was injected")
	}
	return nil
}

func mountFDMatchesAccessMode(mountInfo, target string, fd int, readOnly bool) (bool, error) {
	mounted, actualRO, err := inspectMountFDAccess(mountInfo, target, fd)
	return mounted && actualRO == readOnly, err
}

// Multiple mounts can have the same printed path, including mounts beneath
// covered ancestors. Neither mountinfo order nor the greatest ID identifies
// the mount reached by a pathname. Inspect the securely opened FD instead.
func inspectMountFDAccess(mountInfo, target string, fd int) (bool, bool, error) {
	info, err := os.ReadFile(fmt.Sprintf("/proc/thread-self/fdinfo/%d", fd))
	if err != nil {
		return false, false, fmt.Errorf("inspect mount descriptor: %w", err)
	}
	mountID, err := mountIDFromFDInfo(string(info))
	if err != nil {
		return false, false, err
	}
	mounted, readOnly, err := mountAccessModeForID(mountInfo, target, mountID)
	if err != nil || !mounted {
		return mounted, readOnly, err
	}
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		return false, false, fmt.Errorf("inspect mount descriptor access mode: %w", err)
	}
	if actualRO := fs.Flags&unix.ST_RDONLY != 0; actualRO != readOnly {
		return false, false, fmt.Errorf("mount access mode changed during inspection for target %s", target)
	}
	return true, readOnly, nil
}

func mountIDFromFDInfo(info string) (uint64, error) {
	var mountID uint64
	for _, line := range strings.Split(info, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "mnt_id:" {
			continue
		}
		if len(fields) != 2 || mountID != 0 {
			return 0, fmt.Errorf("invalid mount descriptor ID")
		}
		id, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || id == 0 {
			return 0, fmt.Errorf("invalid mount descriptor ID")
		}
		mountID = id
	}
	if mountID == 0 {
		return 0, fmt.Errorf("mount descriptor ID is unavailable")
	}
	return mountID, nil
}

// Only the FD's exact mount at the exact target is accepted. A writable mount
// must be writable both at the mount and at the superblock.
func mountAccessModeForID(mountInfo, target string, mountID uint64) (bool, bool, error) {
	f, err := os.Open(mountInfo)
	if err != nil {
		return false, false, err
	}
	defer f.Close()
	cleanTarget := filepath.Clean(target)
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 1 || fields[0] != strconv.FormatUint(mountID, 10) {
			continue
		}
		if len(fields) < 6 {
			return false, false, fmt.Errorf("malformed mountinfo for target %s", target)
		}
		if decodeMountInfoPath(fields[4]) != cleanTarget {
			return false, false, nil
		}
		separator := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				separator = i
				break
			}
		}
		if separator < 0 || len(fields) != separator+4 {
			return false, false, fmt.Errorf("malformed mountinfo for target %s", target)
		}
		mountFlags := "," + fields[5] + ","
		superFlags := "," + fields[separator+3] + ","
		mountRO, mountRW := strings.Contains(mountFlags, ",ro,"), strings.Contains(mountFlags, ",rw,")
		superRO, superRW := strings.Contains(superFlags, ",ro,"), strings.Contains(superFlags, ",rw,")
		if mountRO == mountRW || superRO == superRW {
			return false, false, fmt.Errorf("invalid mount access flags for target %s", target)
		}
		return true, mountRO || superRO, nil
	}
	return false, false, s.Err()
}

// The proc-root link is trusted. Every component beneath it is opened with
// O_NOFOLLOW, so a sandbox-controlled target symlink cannot escape its root.
func openContainerDirectory(procRoot string, pid int, target string, create bool) (int, error) {
	segments, err := sourceSubPathSegments(strings.TrimPrefix(target, "/"))
	if err != nil {
		return -1, err
	}
	fd, err := unix.Open(filepath.Join(procRoot, strconv.Itoa(pid), "root"), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, segment := range segments {
		next, err := unix.Openat(fd, segment, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err := unix.Mkdirat(fd, segment, 0755); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(fd)
				return -1, err
			}
			next, err = unix.Openat(fd, segment, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		unix.Close(fd)
		if err != nil {
			return -1, fmt.Errorf("target contains an unavailable, symlink or non-directory component: %w", err)
		}
		fd = next
	}
	return fd, nil
}

func injectReadOnlyDirectory(procRoot string, pid int, stage, subpath, target string) error {
	return injectDirectory(procRoot, pid, stage, subpath, target, true)
}

func injectDirectory(procRoot string, pid int, stage, subpath, target string, readOnly bool) (result error) {
	if mounted, err := isMountPoint(filepath.Join(procRoot, strconv.Itoa(pid), "mountinfo"), target); err != nil {
		return err
	} else if mounted {
		return fmt.Errorf("target is already a mount point")
	}
	source, err := openDirectoryNoFollow(stage, subpath, false, 0)
	if err != nil {
		return err
	}
	defer unix.Close(source)
	targetFD, err := openContainerDirectory(procRoot, pid, target, true)
	if err != nil {
		return err
	}
	defer unix.Close(targetFD)
	// Once a new mount is exposed, every failure must try to unmount it. A
	// lazy detach could leave open writable FDs after the lease is forgotten;
	// failed ordinary cleanup must therefore retain ownership for later retry.
	injected := false
	defer func() {
		if result != nil && injected {
			if err := unmountJuiceFSTarget(procRoot, pid, target); err != nil {
				result = errors.Join(result, ErrMountCleanupPending, fmt.Errorf("unmount failed directory injection: %w", err))
			}
		}
	}()
	tree, treeErr := unix.OpenTree(source, "", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC|unix.AT_EMPTY_PATH)
	modern := treeErr == nil
	if treeErr != nil && !errors.Is(treeErr, unix.ENOSYS) {
		return fmt.Errorf("clone source mount: %w", treeErr)
	}
	modernAccessMode := false
	rootFD := -1
	selfRootFD := -1
	selfCWDfd := -1
	if modern {
		defer unix.Close(tree)
		attrs := unix.MountAttr{Attr_clr: unix.MOUNT_ATTR_RDONLY}
		if readOnly {
			attrs = unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}
		}
		if err := unix.MountSetattr(tree, "", unix.AT_EMPTY_PATH, &attrs); err != nil {
			if !errors.Is(err, unix.ENOSYS) {
				return fmt.Errorf("set directory access mode before injection: %w", err)
			}
			if err := requireSafeLegacySource(source, readOnly); err != nil {
				return err
			}
			rootFD, err = unix.Open(filepath.Join(procRoot, strconv.Itoa(pid), "root"), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("open target root for modern remount: %w", err)
			}
			defer unix.Close(rootFD)
			selfRootFD, err = unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("open wrapper root for modern remount: %w", err)
			}
			defer unix.Close(selfRootFD)
			selfCWDfd, err = unix.Open(".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("open wrapper cwd for modern remount: %w", err)
			}
			defer unix.Close(selfCWDfd)
		} else {
			modernAccessMode = true
		}
	} else if err := requireSafeLegacySource(source, readOnly); err != nil {
		return err
	}
	remountFlags := uintptr(unix.MS_BIND | unix.MS_REMOUNT)
	if readOnly {
		remountFlags |= unix.MS_RDONLY
	}
	if modern {
		result = inMountNamespace(procRoot, pid, func() error {
			if !modernAccessMode {
				if err := enterRoot(rootFD); err != nil {
					return err
				}
				defer restoreRoot(selfRootFD, selfCWDfd)
			}
			if err := unix.MoveMount(tree, "", targetFD, "", unix.MOVE_MOUNT_F_EMPTY_PATH|unix.MOVE_MOUNT_T_EMPTY_PATH); err != nil {
				return err
			}
			injected = true
			if modernAccessMode {
				return nil
			}
			if err := unix.Mount("", target, "", remountFlags, ""); err != nil {
				return fmt.Errorf("enforce directory access mode on modern bind: %w", err)
			}
			return nil
		})
	} else {
		// Older kernels use bind/remount through the securely opened source
		// and target FDs. A read-only request only reaches this path when
		// the source is already read-only, so no writable view is exposed.
		injected, result = legacyDirectoryBind(procRoot, pid, source, targetFD, target, remountFlags)
	}
	if result != nil {
		return result
	}
	// targetFD predates the attachment and still refers to the covered directory.
	// Reopen through the container root to inspect the newly visible mount.
	mountedTargetFD, err := openContainerDirectory(procRoot, pid, target, false)
	if err != nil {
		return err
	}
	defer unix.Close(mountedTargetFD)
	if ok, err := mountFDMatchesAccessMode(filepath.Join(procRoot, strconv.Itoa(pid), "mountinfo"), target, mountedTargetFD, readOnly); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("injected target does not match requested readOnly=%t", readOnly)
	}
	return nil
}

// Without mount_setattr, enforcing RO after attaching an RW source would expose
// a writable view briefly. Only an already read-only source is safe to attach.
func requireSafeLegacySource(sourceFD int, readOnly bool) error {
	if !readOnly {
		return nil
	}
	var stat unix.Statfs_t
	if err := unix.Fstatfs(sourceFD, &stat); err != nil {
		return fmt.Errorf("inspect source access mode for legacy injection: %w", err)
	}
	if stat.Flags&unix.ST_RDONLY == 0 {
		return fmt.Errorf("read-only injection from a writable source requires kernel mount_setattr support")
	}
	return nil
}

func legacyDirectoryBind(procRoot string, pid, sourceFD, targetFD int, target string, remountFlags uintptr) (bool, error) {
	injected := false
	err := inMountNamespace(procRoot, pid, func() error {
		if _, err := os.Stat(fmt.Sprintf("/proc/self/fd/%d", sourceFD)); err != nil {
			return fmt.Errorf("source fd %d is unavailable in target namespace: %w", sourceFD, err)
		}
		src := fmt.Sprintf("/proc/self/fd/%d", sourceFD)
		targetInContainerRoot := fmt.Sprintf("/proc/self/fd/%d", targetFD)
		if err := unix.Mount(src, targetInContainerRoot, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("legacy bind: %w", err)
		}
		injected = true
		// The original target FD still refers to the covered directory. Open
		// the newly attached mount without following container-owned symlinks.
		mountedTargetFD, err := openContainerDirectory(procRoot, pid, target, false)
		if err != nil {
			return fmt.Errorf("open new legacy bind for remount: %w", err)
		}
		defer unix.Close(mountedTargetFD)
		mountedTarget := fmt.Sprintf("/proc/self/fd/%d", mountedTargetFD)
		if err := unix.Mount("", mountedTarget, "", remountFlags, ""); err != nil {
			return fmt.Errorf("enforce directory access mode on legacy bind: %w", err)
		}
		return nil
	})
	return injected, err
}

func unmountJuiceFSTarget(procRoot string, pid int, target string) error {
	return unmountJuiceFSTargetWithFlags(procRoot, pid, target, 0)
}

func unmountJuiceFSTargetWithFlags(procRoot string, pid int, target string, flags int) error {
	rootFD, err := unix.Open(filepath.Join(procRoot, strconv.Itoa(pid), "root"), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD)
	selfRootFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(selfRootFD)
	selfCWDfd, err := unix.Open(".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(selfCWDfd)
	return inMountNamespace(procRoot, pid, func() error {
		if err := enterRoot(rootFD); err != nil {
			return err
		}
		defer restoreRoot(selfRootFD, selfCWDfd)
		return unix.Unmount(target, flags)
	})
}

// If namespace restoration fails, the goroutine exits while locked so Go
// destroys this OS thread; it must never return to the scheduler contaminated.
func inMountNamespace(procRoot string, pid int, fn func() error) error {
	ch := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		unlock := true
		defer func() {
			if unlock {
				runtime.UnlockOSThread()
			}
		}()
		if err := unshareFS(); err != nil {
			ch <- err
			return
		}
		self, err := os.Open(filepath.Join(procRoot, "thread-self/ns/mnt"))
		if err != nil {
			ch <- err
			return
		}
		defer self.Close()
		ns, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "ns/mnt"))
		if err != nil {
			ch <- err
			return
		}
		defer ns.Close()
		if err := setns(int(ns.Fd()), syscall.CLONE_NEWNS); err != nil {
			ch <- err
			return
		}
		result := fn()
		if err := setns(int(self.Fd()), syscall.CLONE_NEWNS); err != nil {
			unlock = false
			result = errors.Join(result, fmt.Errorf("restore mount namespace: %w", err))
		}
		ch <- result
	}()
	return <-ch
}
