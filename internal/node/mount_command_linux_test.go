//go:build linux

package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These tests only spawn this test binary. They never mount or access NFS.
func TestMountCommandCancelsInheritedPipeHelper(t *testing.T) {
	for _, groupMode := range []string{"pidfd", "unreaped_leader"} {
		for _, mode := range []string{"parent_wait", "parent_exit", "parent_error", "parent_closed_pipe"} {
			t.Run(groupMode+"/"+mode, func(t *testing.T) {
				usePidfd := groupMode == "pidfd"
				if usePidfd {
					if supported, err := supportsMountGroupCancellation(); err != nil || !supported {
						t.Skipf("kernel or sandbox lacks pidfd group cancellation: %v", err)
					}
				}
				// A different group must remain alive throughout cancellation.
				sentinel := exec.Command(os.Args[0], "-test.run=^TestMountCommandHelper$", "--", "child")
				sentinel.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				if err := sentinel.Start(); err != nil {
					t.Fatal(err)
				}
				defer func() {
					_ = sentinel.Process.Kill()
					_ = sentinel.Wait()
				}()

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				pidPath := filepath.Join(t.TempDir(), "child.pid")
				result := make(chan error, 1)
				go func() {
					_, err := runMountCommandWithGroupMode(ctx, usePidfd, os.Args[0], "-test.run=^TestMountCommandHelper$", "--", mode, pidPath)
					result <- err
				}()
				pid := waitForHelperPID(t, pidPath)
				fd, err := unix.PidfdOpen(pid, 0)
				if err != nil {
					t.Fatalf("open helper pidfd: %v", err)
				}
				defer unix.Close(fd)
				defer unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
				if err := os.WriteFile(pidPath+".release", nil, 0o600); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				if mode == "parent_wait" {
					cancel()
				}
				select {
				case err := <-result:
					if err == nil && (mode == "parent_wait" || mode == "parent_error" || (mode == "parent_exit" && usePidfd)) {
						t.Fatal("inherited-pipe command unexpectedly succeeded")
					}
					if mode == "parent_wait" && !errors.Is(err, context.Canceled) {
						t.Fatalf("error = %v, want context cancellation", err)
					}
					if mode == "parent_exit" && usePidfd && !errors.Is(err, exec.ErrWaitDelay) {
						t.Fatalf("error = %v, want inherited-pipe WaitDelay", err)
					}
					if mode == "parent_closed_pipe" && err != nil {
						t.Fatalf("exit-0 command error = %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("command stayed blocked on inherited pipe")
				}
				if time.Since(start) > 3*time.Second {
					t.Fatal("command exceeded cancellation bound")
				}
				poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
				if n, err := unix.Poll(poll, 1000); err != nil || n != 1 || poll[0].Revents&unix.POLLIN == 0 {
					t.Fatalf("helper still alive: poll = %d, %v, revents %d", n, err, poll[0].Revents)
				}
				if err := sentinel.Process.Signal(unix.Signal(0)); err != nil {
					t.Fatalf("unrelated sentinel group was affected: %v", err)
				}
			})
		}
	}
}

func TestMountCommandStartAndContextErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runMountCommand(ctx, "/command-that-does-not-exist"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled command error = %v", err)
	}
	if _, err := runMountCommand(context.Background(), "/command-that-does-not-exist"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing command error = %v", err)
	}
}

func waitForHelperPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("helper did not publish its child PID")
	return 0
}

func TestMountCommandHelper(t *testing.T) {
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		return
	}
	if args[0] == "child" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if len(args) != 2 {
		os.Exit(2)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestMountCommandHelper$", "--", "child")
	if args[0] != "parent_closed_pipe" {
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
	}
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(args[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		os.Exit(4)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(args[1] + ".release"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = child.Process.Kill()
			_ = child.Wait()
			os.Exit(6)
		}
		time.Sleep(10 * time.Millisecond)
	}
	switch args[0] {
	case "parent_exit", "parent_closed_pipe":
		os.Exit(0)
	case "parent_error":
		os.Exit(7)
	case "parent_wait":
		_ = child.Wait()
		os.Exit(0)
	default:
		_ = child.Process.Kill()
		_ = child.Wait()
		os.Exit(5)
	}
}
