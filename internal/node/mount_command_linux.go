//go:build linux

package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Linux 6.9 added this pidfd_send_signal scope. Using the old pid object
// instead of an integer PGID also protects cleanup after the leader is reaped.
const pidfdSignalProcessGroup = 1 << 2

const mountCommandWaitDelay = time.Second

func runMountCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	usePidfd, err := supportsMountGroupCancellation()
	if err != nil {
		return nil, err
	}
	return runMountCommandWithGroupMode(ctx, usePidfd, name, args...)
}

func runMountCommandWithGroupMode(ctx context.Context, usePidfd bool, name string, args ...string) ([]byte, error) {
	pidfd := -1
	cmd := exec.CommandContext(ctx, name, args...)
	// Go duplicates a caller-provided PidFD for os.Process, so this descriptor
	// remains ours even after Cmd.Wait reaps the leader.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if usePidfd {
		cmd.SysProcAttr.PidFD = &pidfd
	}
	cmd.WaitDelay = mountCommandWaitDelay
	var cancelOnce sync.Once
	var cancelErr error
	cmd.Cancel = func() error {
		cancelOnce.Do(func() {
			if usePidfd {
				cancelErr = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, pidfdSignalProcessGroup)
			} else {
				// Only used before Wait reaps our child. WNOWAIT retains the
				// leader, so this PGID cannot be reused until the gate closes.
				cancelErr = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
			}
			if errors.Is(cancelErr, unix.ESRCH) {
				cancelErr = os.ErrProcessDone
			}
		})
		return cancelErr
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		if pidfd >= 0 {
			_ = unix.Close(pidfd)
		}
		return nil, err
	}
	if pidfd >= 0 {
		defer unix.Close(pidfd)
	}

	var observeErr error
	if !usePidfd {
		var info unix.Siginfo
		for {
			observeErr = unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if !errors.Is(observeErr, unix.EINTR) {
				break
			}
		}
		if observeErr == nil {
			// Even an exit-0 leader can leave a helper holding a pipe. Stop
			// this group while the unreaped leader still pins its identity.
			_ = cmd.Cancel()
		} else {
			// An external reaper or blocked waitid makes the integer PGID
			// unsafe. Close its signal gate before Cmd.Wait can run.
			cancelOnce.Do(func() { cancelErr = os.ErrProcessDone })
			_ = cmd.Process.Kill()
			observeErr = fmt.Errorf("observe mount leader without reaping: %w", observeErr)
		}
	}
	err := errors.Join(observeErr, cmd.Wait())
	if usePidfd {
		// Wait may finish before Context cancellation, including ErrWaitDelay
		// or a helper that closes its pipe. The pidfd still pins the old group.
		_ = cmd.Cancel()
	}
	if cancelErr != nil && !errors.Is(cancelErr, os.ErrProcessDone) {
		err = errors.Join(err, fmt.Errorf("cancel mount process group: %w", cancelErr))
	}
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	// WaitDelay bounds inherited-pipe waits, not an uninterruptible kernel
	// syscall: a leader that cannot exit still keeps Cmd.Wait blocked.
	return output.Bytes(), err
}

func supportsMountGroupCancellation() (bool, error) {
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		if unsupportedMountGroupCancellation(err) {
			return false, nil
		}
		return false, fmt.Errorf("check mount process-group cancellation: %w", err)
	}
	defer unix.Close(fd)
	err = unix.PidfdSendSignal(fd, 0, nil, pidfdSignalProcessGroup)
	// This process need not be a group leader. ESRCH still proves that the
	// kernel accepted the group scope; signal 0 never delivers a signal.
	if err != nil && !errors.Is(err, unix.ESRCH) {
		if unsupportedMountGroupCancellation(err) {
			return false, nil
		}
		return false, fmt.Errorf("check mount process-group cancellation: %w", err)
	}
	return true, nil
}

func unsupportedMountGroupCancellation(err error) bool {
	return errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)
}
