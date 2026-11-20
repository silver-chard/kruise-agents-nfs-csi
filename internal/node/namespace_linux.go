//go:build linux

package node

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

func inMountNamespace(procRoot string, pid int, fn func() error) error {
	ns, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "ns/mnt"))
	if err != nil {
		return fmt.Errorf("open target mount namespace for pid %d: %w", pid, err)
	}
	defer ns.Close()

	return onMountThread(func() error {
		if err := unshareFS(); err != nil {
			return fmt.Errorf("unshare filesystem attributes: %w", err)
		}
		if err := setns(int(ns.Fd()), unix.CLONE_NEWNS); err != nil {
			return fmt.Errorf("enter target mount namespace for pid %d: %w", pid, err)
		}
		return fn()
	})
}

// The callback may change its mount namespace, root and cwd. Let the locked
// goroutine exit so Go retires the OS thread on both success and failure.
func onMountThread(fn func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if unix.Gettid() == os.Getpid() {
			// Keep the process leader in the wrapper namespace. While it waits
			// here, the nested worker is guaranteed to run on another thread.
			defer runtime.UnlockOSThread()
			done <- onMountThread(fn)
			return
		}
		done <- fn()
	}()
	return <-done
}
