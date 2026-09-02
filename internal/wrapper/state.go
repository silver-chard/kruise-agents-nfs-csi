package wrapper

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
)

const (
	legacyMountStateVersion = 1
	mountStateVersion       = 2
)

type desiredMount struct {
	Request     api.MountRequest `json:"request"`
	ContainerID string           `json:"container_id,omitempty"`
	// legacyRootClassificationUnknown marks version 1 state, which did not
	// record whether its effective source selected the NFS export root.
	legacyRootClassificationUnknown bool `json:"-"`
	// ExportRootAuthorized records that this intent selected the NFS export
	// root and passed the policy active when it was accepted. The fingerprint
	// is present only when that policy required a key.
	ExportRootAuthorized     bool   `json:"export_root_authorized,omitempty"`
	ExportRootKeyFingerprint string `json:"export_root_key_fingerprint,omitempty"`
}

type desiredMountKey struct {
	PodUID        string
	ContainerName string
	TargetPath    string
}

type desiredPodKey struct {
	Namespace string
	Name      string
	UID       string
}

func (mount desiredMount) key() desiredMountKey {
	return desiredMountKey{
		PodUID:        mount.Request.PodUID,
		ContainerName: mount.Request.ContainerName,
		TargetPath:    mount.Request.TargetPath,
	}
}

func desiredMountKeyFor(request api.MountRequest) desiredMountKey {
	return desiredMount{Request: request}.key()
}

func desiredPodKeyFor(request api.MountRequest) desiredPodKey {
	return desiredPodKey{Namespace: request.Namespace, Name: request.PodName, UID: request.PodUID}
}

func sameMountIntent(left, right desiredMount) bool {
	return left.Request == right.Request &&
		left.ExportRootAuthorized == right.ExportRootAuthorized &&
		left.ExportRootKeyFingerprint == right.ExportRootKeyFingerprint
}

type mountStateStore interface {
	List() []desiredMount
	ListForPod(key desiredPodKey) []desiredMount
	Get(key desiredMountKey) (desiredMount, bool)
	Put(mount desiredMount) error
	Delete(key desiredMountKey) error
}

type fileMountStateStore struct {
	mu     sync.Mutex
	dir    string
	mounts map[desiredMountKey]desiredMount
	byPod  map[desiredPodKey]map[desiredMountKey]struct{}
}

type mountStateFile struct {
	Version int          `json:"version"`
	Mount   desiredMount `json:"mount"`
}

func newFileMountStateStore(dir string) (*fileMountStateStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("mount state directory is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create mount state directory %s: %w", dir, err)
	}
	store := &fileMountStateStore{
		dir:    dir,
		mounts: make(map[desiredMountKey]desiredMount),
		byPod:  make(map[desiredPodKey]map[desiredMountKey]struct{}),
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read mount state directory %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read mount state %s: %w", path, err)
		}
		var state mountStateFile
		if err := json.Unmarshal(data, &state); err != nil {
			return nil, fmt.Errorf("decode mount state %s: %w", path, err)
		}
		if state.Version != legacyMountStateVersion && state.Version != mountStateVersion {
			return nil, fmt.Errorf("mount state %s has unsupported version %d", path, state.Version)
		}
		if state.Version == legacyMountStateVersion {
			state.Mount.ExportRootAuthorized = false
			state.Mount.ExportRootKeyFingerprint = ""
			state.Mount.legacyRootClassificationUnknown = true
		}
		key := state.Mount.key()
		if entry.Name() != mountStateFilename(key) {
			return nil, fmt.Errorf("mount state filename %s does not match its mount identity", path)
		}
		store.putInMemory(state.Mount)
	}
	return store, nil
}

func (s *fileMountStateStore) ListForPod(key desiredPodKey) []desiredMount {
	s.mu.Lock()
	defer s.mu.Unlock()

	keys := s.byPod[key]
	mounts := make([]desiredMount, 0, len(keys))
	for mountKey := range keys {
		mounts = append(mounts, s.mounts[mountKey])
	}
	sortDesiredMounts(mounts)
	return mounts
}

func (s *fileMountStateStore) List() []desiredMount {
	s.mu.Lock()
	defer s.mu.Unlock()

	mounts := make([]desiredMount, 0, len(s.mounts))
	for _, mount := range s.mounts {
		mounts = append(mounts, mount)
	}
	sortDesiredMounts(mounts)
	return mounts
}

func (s *fileMountStateStore) Get(key desiredMountKey) (desiredMount, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mount, ok := s.mounts[key]
	return mount, ok
}

func (s *fileMountStateStore) Put(mount desiredMount) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeMountLocked(mount); err != nil {
		return err
	}
	s.putInMemory(mount)
	return nil
}

func (s *fileMountStateStore) Delete(key desiredMountKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.mounts[key]; !exists {
		return nil
	}
	path := filepath.Join(s.dir, mountStateFilename(key))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove mount state %s: %w", path, err)
	}
	if err := syncDirectory(s.dir); err != nil {
		return err
	}
	s.deleteInMemory(key)
	return nil
}

func (s *fileMountStateStore) putInMemory(mount desiredMount) {
	key := mount.key()
	if previous, exists := s.mounts[key]; exists {
		s.deleteInMemory(previous.key())
	}
	s.mounts[key] = mount
	podKey := desiredPodKeyFor(mount.Request)
	if s.byPod[podKey] == nil {
		s.byPod[podKey] = make(map[desiredMountKey]struct{})
	}
	s.byPod[podKey][key] = struct{}{}
}

func (s *fileMountStateStore) deleteInMemory(key desiredMountKey) {
	mount, exists := s.mounts[key]
	if !exists {
		return
	}
	delete(s.mounts, key)
	podKey := desiredPodKeyFor(mount.Request)
	delete(s.byPod[podKey], key)
	if len(s.byPod[podKey]) == 0 {
		delete(s.byPod, podKey)
	}
}

func (s *fileMountStateStore) writeMountLocked(mount desiredMount) error {
	state := mountStateFile{Version: mountStateVersion, Mount: mount}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode mount state: %w", err)
	}
	payload = append(payload, '\n')

	temporary, err := os.CreateTemp(s.dir, ".mount-state-*")
	if err != nil {
		return fmt.Errorf("create temporary mount state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("chmod temporary mount state: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary mount state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary mount state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary mount state: %w", err)
	}
	path := filepath.Join(s.dir, mountStateFilename(mount.key()))
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace mount state %s: %w", path, err)
	}
	return syncDirectory(s.dir)
}

func mountStateFilename(key desiredMountKey) string {
	hash := sha256.New()
	for _, value := range []string{key.PodUID, key.ContainerName, key.TargetPath} {
		hash.Write([]byte(value))
		hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x.json", hash.Sum(nil))
}

func sortDesiredMounts(mounts []desiredMount) {
	sort.Slice(mounts, func(i, j int) bool {
		left := mounts[i].key()
		right := mounts[j].key()
		if left.PodUID != right.PodUID {
			return left.PodUID < right.PodUID
		}
		if left.ContainerName != right.ContainerName {
			return left.ContainerName < right.ContainerName
		}
		return left.TargetPath < right.TargetPath
	})
}

func syncDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open mount state directory for sync: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync mount state directory: %w", err)
	}
	return nil
}
