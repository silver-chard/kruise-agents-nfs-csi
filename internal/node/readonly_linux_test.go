//go:build linux

package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountMatchesAccessMode(t *testing.T) {
	// Linux mountinfo format: optional propagation fields precede "-";
	// per-mount flags and superblock flags describe independent permissions.
	fixture := `24 1 8:2 / / rw,relatime - ext4 /dev/sda2 rw,errors=remount-ro
101 24 0:52 /user/alice /personal ro,nosuid,nodev,relatime shared:41 - fuse.juicefs drive rw,user_id=0,group_id=0
102 24 0:52 /workspace/team /workspace rw,nosuid,nodev,relatime master:12 propagate_from:9 - fuse.juicefs drive rw,user_id=0,group_id=0
103 24 0:53 /user/bob /superblock-ro rw,relatime - fuse.juicefs drive-ro ro,user_id=0,group_id=0
104 24 0:53 /workspace/archive /both-ro ro,relatime - fuse.juicefs drive-ro ro,user_id=0,group_id=0
105 24 0:52 /user/carol /space\040and\134slash ro,relatime - fuse.juicefs drive rw,user_id=0,group_id=0
106 24 0:52 /user/dave /tab\011and\012newline rw,relatime - fuse.juicefs drive rw,user_id=0,group_id=0
`
	mountInfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountInfo, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		target   string
		mountID  uint64
		readOnly bool
		want     bool
	}{
		{"readonly mount", "/personal", 101, true, true},
		{"readonly mount rejects write", "/personal", 101, false, false},
		{"writable mount", "/workspace", 102, false, true},
		{"writable mount rejects readonly", "/workspace", 102, true, false},
		{"readonly superblock", "/superblock-ro", 103, true, true},
		{"writable mount on readonly superblock rejects write", "/superblock-ro", 103, false, false},
		{"both layers readonly", "/both-ro", 104, true, true},
		{"both layers readonly reject write", "/both-ro", 104, false, false},
		{"missing readonly target", "/missing", 999, true, false},
		{"missing writable target", "/missing", 999, false, false},
		{"wrong descriptor mount", "/workspace", 101, false, false},
		{"child is not a mount point", "/workspace/child", 102, false, false},
		{"mount path prefix does not match", "/work", 102, false, false},
		{"clean target path", "/workspace/./", 102, false, true},
		{"escaped space and backslash", "/space and\\slash", 105, true, true},
		{"escaped tab and newline", "/tab\tand\nnewline", 106, false, true},
		{"option substring is not readonly", "/", 24, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mounted, actualRO, err := mountAccessModeForID(mountInfo, tc.target, tc.mountID)
			got := mounted && actualRO == tc.readOnly
			if err != nil || got != tc.want {
				t.Fatalf("mountMatchesAccessMode(%q, %t) = %t, %v; want %t, nil", tc.target, tc.readOnly, got, err, tc.want)
			}
		})
	}
}

func TestMountMatchesAccessModeRejectsMalformedTarget(t *testing.T) {
	for _, fixture := range []string{
		"101 24 0:52 /user/alice /personal rw,relatime",
		"101 24 0:52 /user/alice /personal rw,relatime - fuse.juicefs drive",
		"101 24 0:52 /user/alice /personal relatime - fuse.juicefs drive rw",
		"101 24 0:52 /user/alice /personal rw,ro - fuse.juicefs drive rw",
		"101 24 0:52 /user/alice /personal rw - fuse.juicefs drive user_id=0",
		"101 24 0:52 /user/alice /personal rw - fuse.juicefs drive rw,ro",
	} {
		t.Run(fixture, func(t *testing.T) {
			mountInfo := filepath.Join(t.TempDir(), "mountinfo")
			if err := os.WriteFile(mountInfo, []byte(fixture+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if mounted, _, err := mountAccessModeForID(mountInfo, "/personal", 101); err == nil || mounted {
				t.Fatalf("malformed target accepted: mounted %t, %v", mounted, err)
			}
		})
	}
}

func TestMountMatchesAccessModeReportsReadErrors(t *testing.T) {
	if got, _, err := mountAccessModeForID(filepath.Join(t.TempDir(), "missing"), "/personal", 101); got || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing mountinfo = %t, %v; want false, os.ErrNotExist", got, err)
	}
	mountInfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountInfo, []byte(strings.Repeat("x", 128*1024)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _, err := mountAccessModeForID(mountInfo, "/personal", 101); got || err == nil {
		t.Fatalf("scanner failure = %t, %v; want false, error", got, err)
	}
}

func TestRequireSafeLegacySource(t *testing.T) {
	// An ordinary writable directory must never be exposed before a legacy
	// remount for a read-only request. This check requires no mount privileges.
	fd, err := unix.Open(t.TempDir(), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := requireSafeLegacySource(fd, true); err == nil || !strings.Contains(err.Error(), "mount_setattr") {
		t.Fatalf("read-only injection from writable source = %v; want mount_setattr requirement", err)
	}
	if err := requireSafeLegacySource(fd, false); err != nil {
		t.Fatalf("writable injection from writable source: %v", err)
	}
	if err := requireSafeLegacySource(-1, true); !errors.Is(err, unix.EBADF) {
		t.Fatalf("uninspectable source = %v; want EBADF", err)
	}
}

func TestEnsureJuiceFSSourceAccessRejectsOrdinaryDirectory(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		if err := ensureJuiceFSSourceAccess(t.TempDir(), readOnly); err == nil {
			t.Fatalf("ordinary directory accepted as CSI source (readOnly=%t)", readOnly)
		}
	}
}

func TestMountAccessModeUsesDescriptorIDWithCoveredMounts(t *testing.T) {
	// The actual RO mount is in the middle: this matches the failure observed
	// with CSI propagation beneath several covered parent mounts. Sorting by
	// mount ID or taking the first/last printed path would select an RW mount.
	fixture := `12876 19387 0:712 / /source rw,relatime - fuse.juicefs drive rw
12873 19383 0:712 / /source ro,relatime - fuse.juicefs drive rw
12870 19379 0:712 / /source rw,relatime - fuse.juicefs drive rw
12867 19380 0:712 / /source rw,relatime - fuse.juicefs drive rw
`
	mountInfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountInfo, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{12876, 12873, 12870, 12867} {
		mounted, readOnly, err := mountAccessModeForID(mountInfo, "/source", id)
		if err != nil || !mounted || readOnly != (id == 12873) {
			t.Fatalf("mount %d: mounted=%t readOnly=%t err=%v", id, mounted, readOnly, err)
		}
	}
	if mounted, _, err := mountAccessModeForID(mountInfo, "/source", 999); mounted || err != nil {
		t.Fatalf("unlisted descriptor matched covered path: mounted=%t err=%v", mounted, err)
	}
}

func TestMountIDFromFDInfo(t *testing.T) {
	if id, err := mountIDFromFDInfo("pos:\t0\nflags:\t012000000\nmnt_id:\t12873\nino:\t1\n"); err != nil || id != 12873 {
		t.Fatalf("valid descriptor: id=%d err=%v", id, err)
	}
	for _, info := range []string{"", "ino: 12873", "mnt_id:", "mnt_id: 0", "mnt_id: -1", "mnt_id: abc", "mnt_id: 18446744073709551616", "mnt_id: 123 extra", "mnt_id: 123\nmnt_id: 124"} {
		if id, err := mountIDFromFDInfo(info); id != 0 || err == nil {
			t.Errorf("invalid descriptor %q: id=%d err=%v", info, id, err)
		}
	}
}

func TestMountFDMatchesAccessModeInspectsActualDescriptor(t *testing.T) {
	fd, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var fs unix.Statfs_t
	if err := unix.Fstatfs(fd, &fs); err != nil {
		t.Fatal(err)
	}
	readOnly := fs.Flags&unix.ST_RDONLY != 0
	for _, mode := range []bool{false, true} {
		got, err := mountFDMatchesAccessMode("/proc/thread-self/mountinfo", "/", fd, mode)
		if err != nil || got != (mode == readOnly) {
			t.Fatalf("root mode %t: matches=%t err=%v (actual RO=%t)", mode, got, err, readOnly)
		}
	}
	if _, _, err := inspectMountFDAccess("/proc/thread-self/mountinfo", "/", -1); err == nil {
		t.Fatal("invalid mount descriptor accepted")
	}

	info, err := os.ReadFile(fmt.Sprintf("/proc/thread-self/fdinfo/%d", fd))
	if err != nil {
		t.Fatal(err)
	}
	id, err := mountIDFromFDInfo(string(info))
	if err != nil {
		t.Fatal(err)
	}
	// A mountinfo snapshot with the opposite flags must not override the
	// descriptor's actual kernel flags, even when path and ID both match.
	flags := "ro"
	if readOnly {
		flags = "rw"
	}
	mountInfo := filepath.Join(t.TempDir(), "mountinfo")
	fixture := fmt.Sprintf("%d 1 0:1 / / %s - test test %s\n", id, flags, flags)
	if err := os.WriteFile(mountInfo, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	if mounted, _, err := inspectMountFDAccess(mountInfo, "/", fd); mounted || err == nil {
		t.Fatalf("inconsistent kernel and mountinfo flags accepted: mounted=%t err=%v", mounted, err)
	}
}
