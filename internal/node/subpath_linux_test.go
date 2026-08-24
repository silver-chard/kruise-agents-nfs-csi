//go:build linux

package node

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenDirectoryNoFollowDoesNotCreateWhenDisabled(t *testing.T) {
	stagePath := t.TempDir()

	fd, err := openDirectoryNoFollow(stagePath, "users/alice", false, 0)
	if fd >= 0 {
		_ = unix.Close(fd)
	}
	if !errors.Is(err, ErrBadSourceSubPath) {
		t.Fatalf("openDirectoryNoFollow error = %v, want ErrBadSourceSubPath", err)
	}
	if _, err := os.Stat(filepath.Join(stagePath, "users")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat disabled subpath parent error = %v, want os.ErrNotExist", err)
	}
}

func TestOpenDirectoryNoFollowCreatesNestedDirectories(t *testing.T) {
	stagePath := t.TempDir()

	fd, err := openDirectoryNoFollow(stagePath, "./users//alice/workspace", true, 0o700)
	if err != nil {
		t.Fatalf("openDirectoryNoFollow returned error: %v", err)
	}
	defer unix.Close(fd)

	for _, relativePath := range []string{"users", "users/alice", "users/alice/workspace"} {
		info, err := os.Stat(filepath.Join(stagePath, relativePath))
		if err != nil {
			t.Fatalf("stat created directory %s: %v", relativePath, err)
		}
		if !info.IsDir() {
			t.Fatalf("created path %s is not a directory", relativePath)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("created directory %s mode = %04o, want 0700", relativePath, got)
		}
	}
}

func TestOpenDirectoryNoFollowKeepsExistingDirectories(t *testing.T) {
	stagePath := t.TempDir()
	usersPath := filepath.Join(stagePath, "users")
	alicePath := filepath.Join(usersPath, "alice")
	if err := os.Mkdir(usersPath, 0o711); err != nil {
		t.Fatalf("create users directory: %v", err)
	}
	if err := os.Mkdir(alicePath, 0o700); err != nil {
		t.Fatalf("create alice directory: %v", err)
	}

	fd, err := openDirectoryNoFollow(stagePath, "users/alice", true, 0o777)
	if err != nil {
		t.Fatalf("openDirectoryNoFollow returned error: %v", err)
	}
	defer unix.Close(fd)

	for path, want := range map[string]os.FileMode{
		usersPath: 0o711,
		alicePath: 0o700,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat existing directory %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("existing directory %s mode = %04o, want %04o", path, got, want)
		}
	}
}

func TestOpenDirectoryNoFollowRejectsFileAndSymlink(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		stagePath := t.TempDir()
		if err := os.WriteFile(filepath.Join(stagePath, "blocked"), []byte("not a directory"), 0o600); err != nil {
			t.Fatalf("create blocking file: %v", err)
		}

		fd, err := openDirectoryNoFollow(stagePath, "blocked/child", true, 0o700)
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		if !errors.Is(err, ErrBadSourceSubPath) {
			t.Fatalf("file traversal error = %v, want ErrBadSourceSubPath", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		stagePath := t.TempDir()
		targetPath := t.TempDir()
		if err := os.Symlink(targetPath, filepath.Join(stagePath, "linked")); err != nil {
			t.Fatalf("create blocking symlink: %v", err)
		}

		fd, err := openDirectoryNoFollow(stagePath, "linked/child", true, 0o700)
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		if !errors.Is(err, ErrBadSourceSubPath) {
			t.Fatalf("symlink traversal error = %v, want ErrBadSourceSubPath", err)
		}
		if _, err := os.Stat(filepath.Join(targetPath, "child")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat symlink target child error = %v, want os.ErrNotExist", err)
		}
	})
}

func TestOpenDirectoryNoFollowRejectsParentTraversalBeforeCreating(t *testing.T) {
	stagePath := t.TempDir()

	fd, err := openDirectoryNoFollow(stagePath, "created/../escape", true, 0o700)
	if fd >= 0 {
		_ = unix.Close(fd)
	}
	if !errors.Is(err, ErrBadSourceSubPath) {
		t.Fatalf("parent traversal error = %v, want ErrBadSourceSubPath", err)
	}
	if _, err := os.Stat(filepath.Join(stagePath, "created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat rejected path prefix error = %v, want os.ErrNotExist", err)
	}
}

func TestOpenDirectoryNoFollowDoesNotRollbackCreatedParents(t *testing.T) {
	stagePath := t.TempDir()
	tooLongSegment := strings.Repeat("x", 4096)

	fd, err := openDirectoryNoFollow(stagePath, "created/"+tooLongSegment, true, 0o700)
	if fd >= 0 {
		_ = unix.Close(fd)
	}
	if !errors.Is(err, ErrBadSourceSubPath) {
		t.Fatalf("long path error = %v, want ErrBadSourceSubPath", err)
	}
	info, err := os.Stat(filepath.Join(stagePath, "created"))
	if err != nil {
		t.Fatalf("stat retained parent directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("retained parent path is not a directory")
	}
}

func TestOpenDirectoryNoFollowConcurrentCreation(t *testing.T) {
	stagePath := t.TempDir()
	const workers = 64

	start := make(chan struct{})
	workerErrors := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fd, err := openDirectoryNoFollow(stagePath, "shared/nested/path", true, 0o700)
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			workerErrors <- err
		}()
	}
	close(start)
	wg.Wait()
	close(workerErrors)

	for err := range workerErrors {
		if err != nil {
			t.Fatalf("concurrent openDirectoryNoFollow returned error: %v", err)
		}
	}
	info, err := os.Stat(filepath.Join(stagePath, "shared/nested/path"))
	if err != nil {
		t.Fatalf("stat concurrently created directory: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("concurrently created path is not a directory")
	}
}

func TestCreatedSubPathUnixMode(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
		want uint32
	}{
		{name: "default", want: 0o770},
		{
			name: "special bits",
			mode: 0o751 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky,
			want: 0o751 | unix.S_ISUID | unix.S_ISGID | unix.S_ISVTX,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := createdSubPathUnixMode(tt.mode); got != tt.want {
				t.Fatalf("createdSubPathUnixMode(%v) = %#o, want %#o", tt.mode, got, tt.want)
			}
		})
	}
}

func TestSourceSubPathErrorClassification(t *testing.T) {
	t.Run("bad path", func(t *testing.T) {
		for _, err := range []error{unix.ENOENT, unix.ENOTDIR, unix.ELOOP, unix.ENAMETOOLONG} {
			got := sourceSubPathOpenError("users/alice", "/staging/pv", err, true)
			if !errors.Is(got, ErrBadSourceSubPath) {
				t.Errorf("sourceSubPathOpenError(%v) = %v, want ErrBadSourceSubPath", err, got)
			}
		}
	})

	t.Run("runtime open failure", func(t *testing.T) {
		for _, err := range []error{unix.ENOENT, unix.EACCES, unix.EIO} {
			got := sourceSubPathOpenError("users/alice", "/staging/pv", err, false)
			if errors.Is(got, ErrBadSourceSubPath) {
				t.Errorf("sourceSubPathOpenError(%v) = %v, must not wrap ErrBadSourceSubPath", err, got)
			}
			if !errors.Is(got, err) {
				t.Errorf("sourceSubPathOpenError(%v) = %v, want wrapped syscall error", err, got)
			}
		}
	})

	t.Run("storage create failure", func(t *testing.T) {
		for _, err := range []error{unix.EACCES, unix.EROFS, unix.ENOSPC, unix.EDQUOT} {
			got := sourceSubPathCreateError("users/alice", "/staging/pv", err)
			if errors.Is(got, ErrBadSourceSubPath) {
				t.Errorf("sourceSubPathCreateError(%v) = %v, must not wrap ErrBadSourceSubPath", err, got)
			}
			if !errors.Is(got, err) {
				t.Errorf("sourceSubPathCreateError(%v) = %v, want wrapped syscall error", err, got)
			}
		}
	})
}
