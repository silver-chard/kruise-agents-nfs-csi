//go:build linux

package node

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMountNamespaceWorkerIsolation(t *testing.T) {
	target := exec.Command("sleep", "30")
	target.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
	if err := target.Start(); err != nil {
		if errors.Is(err, unix.EPERM) {
			t.Skip("creating a test mount namespace requires CAP_SYS_ADMIN")
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Process.Kill(); _ = target.Wait() })

	parentNS := readMountNS(t, "/proc/self/ns/mnt")
	childNS := readMountNS(t, fmt.Sprintf("/proc/%d/ns/mnt", target.Process.Pid))
	if childNS == parentNS {
		t.Fatal("test process must have a distinct mount namespace")
	}
	root, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	parentRoot, err := os.Stat("/")
	if err != nil {
		t.Fatal(err)
	}
	fail := errors.New("failure after entering container root")
	for _, result := range []error{nil, fail} {
		var tid int
		err := inMountNamespace("/proc", target.Process.Pid, func() error {
			tid = unix.Gettid()
			ns, err := os.Readlink("/proc/thread-self/ns/mnt")
			if err != nil || ns != childNS {
				return fmt.Errorf("worker namespace %q, expected %q: %v", ns, childNS, err)
			}
			if err := enterRoot(int(root.Fd())); err != nil {
				return err
			}
			return result
		})
		if err != result {
			t.Fatalf("callback returned %v, expected %v", err, result)
		}
		if tid == os.Getpid() {
			t.Fatal("process leader ran the callback")
		}
		deadline := time.Now().Add(3 * time.Second)
		for {
			_, err := os.Stat("/proc/self/task/" + strconv.Itoa(tid))
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil || time.Now().After(deadline) {
				t.Fatalf("mount thread %d was not retired: %v", tid, err)
			}
			time.Sleep(time.Millisecond)
		}
		if got := readMountNS(t, "/proc/self/ns/mnt"); got != parentNS {
			t.Fatalf("wrapper namespace changed from %s to %s", parentNS, got)
		}
		if got, err := os.Stat("/"); err != nil || !os.SameFile(parentRoot, got) {
			t.Fatalf("wrapper root changed: %v", err)
		}
	}
}

func readMountNS(t *testing.T, path string) string {
	t.Helper()
	ns, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}
