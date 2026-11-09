//go:build linux

package node

// This backend invokes the official JuiceFS CSI NodePublishVolume RPC. It
// intentionally uses an operator-owned publish root, never a fabricated
// kubelet Pod path. Mount Pod mode uses an explicitly configured CSI extension
// with real PodUID references; process mode retains its legacy target layout.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type juiceFSLease struct {
	Version          int               `json:"version"`
	PodUID           string            `json:"pod_uid"`
	PVName           string            `json:"pv_name"`
	VolumeHandle     string            `json:"volume_handle"`
	Target           string            `json:"target"`
	Fingerprint      string            `json:"fingerprint"`
	Consumers        map[string]string `json:"consumers"`
	SourceReadOnly   bool              `json:"source_read_only"`
	ConsumerReadOnly map[string]bool   `json:"consumer_read_only"`
}

func juiceFSKey(plan MountPlan) string {
	// Keep the v1 key format for in-place upgrades: existing r13 lease files
	// and publish targets must remain discoverable and must not become orphans.
	// This function is JuiceFS-specific, and PV names are cluster-unique.
	sum := sha256.Sum256([]byte(plan.PodUID + "\x00" + plan.PV.Name))
	return hex.EncodeToString(sum[:])
}
func juiceFSConsumer(plan MountPlan) string { return plan.ContainerName + "\x00" + plan.TargetPath }
func juiceFSFingerprint(plan MountPlan) string {
	b, _ := json.Marshal(struct {
		H, F string
		A    map[string]string
		O    []string
	}{plan.PV.VolumeHandle, plan.PV.FSType, plan.PV.Attributes, plan.PV.MountOptions})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (m *linuxMounter) juiceFSStagePath(plan MountPlan) string {
	if m.cfg.JuiceFSMountMode == "mountpod" {
		return juiceFSMountPodTarget(m.cfg.JuiceFSPublishRoot, plan.PodUID, plan.PV.Name)
	}
	return filepath.Join(m.cfg.JuiceFSPublishRoot, juiceFSKey(plan))
}
func (m *linuxMounter) juiceFSClient(ctx context.Context) (csi.NodeClient, func() error, error) {
	if m.cfg.JuiceFSSocket == "" {
		return nil, nil, fmt.Errorf("JuiceFS CSI socket is not configured")
	}
	conn, err := grpc.DialContext(ctx, "unix://"+m.cfg.JuiceFSSocket, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		return nil, nil, fmt.Errorf("connect JuiceFS CSI socket: %w", err)
	}
	return csi.NewNodeClient(conn), conn.Close, nil
}
func (m *linuxMounter) readJuiceFSLease(plan MountPlan) (*juiceFSLease, error) {
	b, err := os.ReadFile(filepath.Join(m.cfg.JuiceFSStateDir, juiceFSKey(plan)+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l juiceFSLease
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	if (l.Version != 1 && l.Version != 2) || l.PodUID != plan.PodUID || l.PVName != plan.PV.Name || l.Target != m.juiceFSStagePath(plan) || l.Consumers == nil {
		return nil, fmt.Errorf("JuiceFS source lease identity mismatch")
	}
	if l.Version == 1 {
		// Older wrappers only created RO sources/targets. Preserve that mode
		// even if the PV itself permits writes; never upgrade an active source.
		l.Version = 2
		l.SourceReadOnly = true
		l.ConsumerReadOnly = make(map[string]bool, len(l.Consumers))
		for consumer := range l.Consumers {
			l.ConsumerReadOnly[consumer] = true
		}
	}
	if l.ConsumerReadOnly == nil || len(l.ConsumerReadOnly) != len(l.Consumers) {
		return nil, fmt.Errorf("JuiceFS source lease is missing consumer access modes")
	}
	for consumer := range l.Consumers {
		mode, ok := l.ConsumerReadOnly[consumer]
		if !ok || (l.SourceReadOnly && !mode) {
			return nil, fmt.Errorf("JuiceFS source lease has inconsistent consumer access modes")
		}
	}
	return &l, nil
}
func (m *linuxMounter) writeJuiceFSLease(plan MountPlan, l *juiceFSLease) error {
	if err := os.MkdirAll(m.cfg.JuiceFSStateDir, 0700); err != nil {
		return err
	}
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.cfg.JuiceFSStateDir, ".lease-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(m.cfg.JuiceFSStateDir, juiceFSKey(plan)+".json")); err != nil {
		return err
	}
	dir, err := os.Open(m.cfg.JuiceFSStateDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func juiceFSPublishRequest(plan MountPlan, target string, sourceReadOnly bool) *csi.NodePublishVolumeRequest {
	a := map[string]string{}
	for k, v := range plan.PV.Attributes {
		a[k] = v
	}
	a["csi.storage.k8s.io/pod.name"] = plan.PodName
	a["csi.storage.k8s.io/pod.namespace"] = plan.PodNamespace
	a["csi.storage.k8s.io/pod.uid"] = plan.PodUID
	a["csi.storage.k8s.io/serviceAccount.name"] = plan.ServiceAccount
	// Preserve CSI's precedence: volumeAttributes first, then spec.mountOptions.
	options := append([]string{a["mountOptions"]}, plan.PV.MountOptions...)
	delete(a, "mountOptions")
	flags := make([]string, 0, len(options)+1)
	for _, option := range options {
		for _, part := range strings.Split(option, ",") {
			part = strings.TrimSpace(part)
			key, _, _ := strings.Cut(part, "=")
			if part != "" && key != "rw" && key != "ro" && key != "read-only" {
				flags = append(flags, part)
			}
		}
	}
	accessMode := csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	if sourceReadOnly {
		flags = append(flags, "ro")
		accessMode = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
	} else {
		flags = append(flags, "rw")
	}
	return &csi.NodePublishVolumeRequest{VolumeId: plan.PV.VolumeHandle, TargetPath: target, Readonly: sourceReadOnly, VolumeCapability: &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: plan.PV.FSType, MountFlags: flags}}, AccessMode: &csi.VolumeCapability_AccessMode{Mode: accessMode}}, VolumeContext: a, Secrets: plan.PV.Secrets}
}

func validateJuiceFSLeaseAccess(plan MountPlan, l *juiceFSLease) error {
	if !plan.ReadOnly && l.SourceReadOnly {
		return fmt.Errorf("JuiceFS source is already read-only; release all consumers before establishing a writable source")
	}
	if JuiceFSSourceReadOnly(plan.PV) && !l.SourceReadOnly {
		return fmt.Errorf("JuiceFS PV read-only policy changed for an active writable source; unmount its consumers first")
	}
	consumer := juiceFSConsumer(plan)
	if _, exists := l.Consumers[consumer]; exists && l.ConsumerReadOnly[consumer] != plan.ReadOnly {
		return fmt.Errorf("JuiceFS target access mode changed; unmount the existing target first")
	}
	return nil
}

func (m *linuxMounter) mountJuiceFS(ctx context.Context, plan MountPlan) (result error) {
	if m.cfg.JuiceFSMountMode == "mountpod" && (!juiceFSPathSegment(plan.PodUID) || !juiceFSPathSegment(plan.PV.Name)) {
		return fmt.Errorf("invalid JuiceFS Mount Pod target identity")
	}
	if !plan.ReadOnly && JuiceFSSourceReadOnly(plan.PV) {
		return fmt.Errorf("JuiceFS PV is configured read-only; cannot mount a writable target")
	}
	if plan.PodUID == "" || plan.PV.Name == "" || plan.PV.VolumeHandle == "" || plan.ContainerID == "" {
		return fmt.Errorf("JuiceFS mount identity is incomplete")
	}
	if !filepath.IsAbs(m.cfg.JuiceFSPublishRoot) || !filepath.IsAbs(m.cfg.JuiceFSStateDir) {
		return fmt.Errorf("absolute JuiceFS publish and state roots are required")
	}
	lock := m.stagingLock(juiceFSKey(plan))
	lock.Lock()
	defer lock.Unlock()
	l, err := m.readJuiceFSLease(plan)
	if err != nil {
		return err
	}
	stage := m.juiceFSStagePath(plan)
	if l == nil {
		if mounted, err := isMountPoint("/proc/thread-self/mountinfo", stage); err != nil {
			return err
		} else if mounted {
			return fmt.Errorf("refusing to adopt an unowned JuiceFS publish target")
		}
		if err := os.MkdirAll(stage, 0750); err != nil {
			return err
		}
		l = &juiceFSLease{Version: 2, PodUID: plan.PodUID, PVName: plan.PV.Name, VolumeHandle: plan.PV.VolumeHandle, Target: stage, Fingerprint: juiceFSFingerprint(plan), Consumers: map[string]string{}, SourceReadOnly: JuiceFSSourceReadOnly(plan.PV), ConsumerReadOnly: map[string]bool{}}
	} else if l.VolumeHandle != plan.PV.VolumeHandle || l.Fingerprint != juiceFSFingerprint(plan) {
		return fmt.Errorf("JuiceFS PV definition changed for an active source")
	}
	if err := validateJuiceFSLeaseAccess(plan, l); err != nil {
		return err
	}
	c := juiceFSConsumer(plan)
	old, existed := l.Consumers[c]
	if existed && old != plan.SourceSubPath {
		return fmt.Errorf("JuiceFS source subpath changed for an existing target")
	}
	l.Consumers[c] = plan.SourceSubPath
	l.ConsumerReadOnly[c] = plan.ReadOnly
	if err := m.writeJuiceFSLease(plan, l); err != nil {
		return err
	}
	defer func() {
		if result != nil && !existed && !errors.Is(result, ErrMountCleanupPending) {
			// A failed injection must not leave an untracked source reference.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result = errors.Join(result, m.releaseJuiceFSLocked(cleanupCtx, plan))
		}
	}()
	client, closeConn, err := m.juiceFSClient(ctx)
	if err != nil {
		return err
	}
	defer closeConn()
	publish := func() error {
		if _, err := client.NodePublishVolume(ctx, juiceFSPublishRequest(plan, stage, l.SourceReadOnly)); err != nil {
			return fmt.Errorf("JuiceFS NodePublishVolume failed; inspect trusted CSI node logs")
		}
		return nil
	}
	// A JuiceFS FUSE client can become a stale "transport endpoint" after the
	// CSI node process restarts. Probe the shared source root before the
	// idempotent NodePublish call. Do not repair it destructively here: other
	// consumers may still reference the same superblock.
	stageMounted, stageMountErr := isMountPoint("/proc/thread-self/mountinfo", stage)
	if stageMountErr != nil {
		return stageMountErr
	}
	root, rootProbeErr := openDirectoryNoFollow(stage, "", false, 0)
	if rootProbeErr == nil {
		_ = unix.Close(root)
	}
	if stageMounted && rootProbeErr != nil {
		return fmt.Errorf("JuiceFS source is stale (%v); refusing destructive recovery while lease consumers may still reference it", rootProbeErr)
	}
	if err := publish(); err != nil {
		// Do not unpublish on an ordinary RPC error. This source may already be
		// serving other consumers in the same lease; tearing it down here would
		// break all of them.
		return err
	}
	if err := ensureJuiceFSSourceAccess(stage, l.SourceReadOnly); err != nil {
		return err
	}
	source, sourceErr := openDirectoryNoFollow(stage, plan.SourceSubPath, false, 0)
	if sourceErr != nil {
		return fmt.Errorf("JuiceFS source subpath is unavailable: %w", sourceErr)
	}
	_ = unix.Close(source)
	pid, err := findContainerPID(m.cfg.HostProcRoot, plan.PodUID, plan.ContainerID)
	if err != nil {
		return err
	}
	if mounted, err := isMountPoint(filepath.Join(m.cfg.HostProcRoot, strconv.Itoa(pid), "mountinfo"), plan.TargetPath); err != nil {
		return err
	} else if mounted {
		if !existed {
			return fmt.Errorf("refusing to replace an existing mount at unowned JuiceFS target %s", plan.TargetPath)
		}
		// An existing registered consumer can be rebound idempotently after the
		// container mount disappeared or changed. Unregistered mounts were rejected
		// above and are never replaced.
		if err := unmountJuiceFSTarget(m.cfg.HostProcRoot, pid, plan.TargetPath); err != nil {
			return fmt.Errorf("replace stale JuiceFS target %s: %w", plan.TargetPath, err)
		}
	}
	return injectDirectory(m.cfg.HostProcRoot, pid, stage, plan.SourceSubPath, plan.TargetPath, plan.ReadOnly)
}
func (m *linuxMounter) unmountJuiceFS(ctx context.Context, plan MountPlan) error {
	pid, err := findContainerPID(m.cfg.HostProcRoot, plan.PodUID, plan.ContainerID)
	if err != nil {
		return fmt.Errorf("find JuiceFS target container process: %w", err)
	}
	if mounted, err := isMountPoint(filepath.Join(m.cfg.HostProcRoot, strconv.Itoa(pid), "mountinfo"), plan.TargetPath); err != nil {
		return fmt.Errorf("inspect JuiceFS target mountinfo: %w", err)
	} else if mounted {
		if err := unmountJuiceFSTarget(m.cfg.HostProcRoot, pid, plan.TargetPath); err != nil {
			return fmt.Errorf("unmount JuiceFS target %s: %w", plan.TargetPath, err)
		}
	}
	lock := m.stagingLock(juiceFSKey(plan))
	lock.Lock()
	defer lock.Unlock()
	l, err := m.readJuiceFSLease(plan)
	if err != nil {
		return fmt.Errorf("read JuiceFS lease: %w", err)
	}
	if l == nil {
		return nil
	}
	delete(l.Consumers, juiceFSConsumer(plan))
	delete(l.ConsumerReadOnly, juiceFSConsumer(plan))
	if len(l.Consumers) > 0 {
		return m.writeJuiceFSLease(plan, l)
	}
	client, closeConn, err := m.juiceFSClient(ctx)
	if err != nil {
		return fmt.Errorf("connect JuiceFS CSI for unpublish: %w", err)
	}
	defer closeConn()
	if _, err := client.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: l.VolumeHandle, TargetPath: l.Target}); err != nil {
		return fmt.Errorf("JuiceFS NodeUnpublishVolume failed; inspect trusted CSI node logs")
	}
	if err := os.Remove(filepath.Join(m.cfg.JuiceFSStateDir, juiceFSKey(plan)+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (m *linuxMounter) isMountedJuiceFS(ctx context.Context, plan MountPlan) (bool, error) {
	pid, err := findContainerPID(m.cfg.HostProcRoot, plan.PodUID, plan.ContainerID)
	if err != nil {
		return false, err
	}
	target, err := openContainerDirectory(m.cfg.HostProcRoot, pid, plan.TargetPath, false)
	if err != nil {
		return false, nil
	}
	defer unix.Close(target)
	ok, err := mountFDMatchesAccessMode(filepath.Join(m.cfg.HostProcRoot, strconv.Itoa(pid), "mountinfo"), plan.TargetPath, target, plan.ReadOnly)
	if err != nil || !ok {
		return false, err
	}
	l, err := m.readJuiceFSLease(plan)
	if err != nil {
		return false, err
	}
	if l == nil {
		return false, fmt.Errorf("JuiceFS target has no matching source lease")
	}
	subpath, exists := l.Consumers[juiceFSConsumer(plan)]
	if !exists || subpath != plan.SourceSubPath {
		return false, fmt.Errorf("JuiceFS target source lease mismatch")
	}
	if l.ConsumerReadOnly[juiceFSConsumer(plan)] != plan.ReadOnly {
		return false, fmt.Errorf("JuiceFS target access mode differs from its source lease")
	}
	if err := validateJuiceFSLeaseAccess(plan, l); err != nil {
		return false, err
	}
	// MS_REMOUNT bind flags are namespace-local and may not survive wrapper
	// Pod replacement through mount propagation. A healthy existing RO consumer
	// must not be unmounted/rebound merely to tighten the new wrapper's source
	// view. Repair that view in place; keep the consumer's mount and open FDs.
	if err := ensureJuiceFSSourceAccess(l.Target, l.SourceReadOnly); err != nil {
		return false, err
	}
	sourceRoot, err := openDirectoryNoFollow(l.Target, "", false, 0)
	if err != nil {
		return false, nil
	}
	defer unix.Close(sourceRoot)
	if ok, err := mountFDMatchesAccessMode("/proc/thread-self/mountinfo", l.Target, sourceRoot, l.SourceReadOnly); err != nil || !ok {
		return false, err
	}
	source, err := openDirectoryNoFollow(l.Target, plan.SourceSubPath, false, 0)
	if err != nil {
		// A missing/unavailable source needs attention. mountJuiceFS refuses
		// destructive recovery of a stale source still referenced by consumers.
		return false, nil
	}
	defer unix.Close(source)
	var src, dst unix.Stat_t
	if err := unix.Fstat(source, &src); err != nil {
		return false, err
	}
	if err := unix.Fstat(target, &dst); err != nil {
		return false, err
	}
	if src.Dev != dst.Dev || src.Ino != dst.Ino {
		return false, nil
	}
	return true, nil
}

// Release is used by the node reconciler after a Pod disappeared before an
// explicit unmount request could be delivered.
func (m *linuxMounter) Release(ctx context.Context, plan MountPlan) error {
	if plan.PV.Driver != JuiceFSDriver {
		return nil
	}
	lock := m.stagingLock(juiceFSKey(plan))
	lock.Lock()
	defer lock.Unlock()
	return m.releaseJuiceFSLocked(ctx, plan)
}

func (m *linuxMounter) releaseJuiceFSLocked(ctx context.Context, plan MountPlan) error {
	l, err := m.readJuiceFSLease(plan)
	if err != nil || l == nil {
		return err
	}
	delete(l.Consumers, juiceFSConsumer(plan))
	delete(l.ConsumerReadOnly, juiceFSConsumer(plan))
	if len(l.Consumers) > 0 {
		return m.writeJuiceFSLease(plan, l)
	}
	client, closeConn, err := m.juiceFSClient(ctx)
	if err != nil {
		return err
	}
	defer closeConn()
	if _, err := client.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{VolumeId: l.VolumeHandle, TargetPath: l.Target}); err != nil {
		return fmt.Errorf("JuiceFS NodeUnpublishVolume failed; inspect trusted CSI node logs")
	}
	if err := os.Remove(filepath.Join(m.cfg.JuiceFSStateDir, juiceFSKey(plan)+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
