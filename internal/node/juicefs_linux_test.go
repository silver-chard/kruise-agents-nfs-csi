//go:build linux

package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJuiceFSPublishRequestPreservesPVAndRO(t *testing.T) {
	plan := MountPlan{PV: PersistentVolume{VolumeHandle: "handle", Attributes: map[string]string{"name": "drive"}, MountOptions: []string{"rw", "subdir=/tenants/team"}, Secrets: map[string]string{"metaurl": "secret"}}, PodUID: "uid", PodName: "warm", PodNamespace: "demo", SourceSubPath: "users/alice"}
	r := juiceFSPublishRequest(plan, "/owned/publish", true)
	if r.VolumeId != "handle" || r.TargetPath != "/owned/publish" || !r.Readonly || r.Secrets["metaurl"] != "secret" {
		t.Fatalf("wrong CSI mapping")
	}
	if r.VolumeContext["subPath"] != "" || r.VolumeContext["csi.storage.k8s.io/pod.uid"] != "uid" {
		t.Fatal("business subpath leaked into CSI subPath or pod identity missing")
	}
	flags := strings.Join(r.VolumeCapability.GetMount().MountFlags, ",")
	if strings.Contains(flags, "rw") || !strings.Contains(flags, "subdir=/tenants/team") || !strings.HasSuffix(flags, "ro") {
		t.Fatal(flags)
	}
}

type releaseTestNode struct {
	csi.UnimplementedNodeServer
	calls        int
	publishCalls int
	publishErr   error
}

func (n *releaseTestNode) NodePublishVolume(context.Context, *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	n.publishCalls++
	return &csi.NodePublishVolumeResponse{}, n.publishErr
}

func (n *releaseTestNode) NodeUnpublishVolume(context.Context, *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	n.calls++
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func TestJuiceFSLeaseLastConsumerReleasesSource(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "csi.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	backend := &releaseTestNode{}
	csi.RegisterNodeServer(server, backend)
	go server.Serve(listener)
	defer server.Stop()
	m := &linuxMounter{cfg: Config{JuiceFSSocket: socket, JuiceFSStateDir: filepath.Join(dir, "state"), JuiceFSPublishRoot: filepath.Join(dir, "publish")}}
	a := MountPlan{PV: PersistentVolume{Name: "shared", Driver: JuiceFSDriver, VolumeHandle: "handle", Secrets: map[string]string{"password": "sensitive"}}, PodUID: "pod", ContainerName: "main", TargetPath: "/personal", SourceSubPath: "users/alice"}
	b := a
	b.TargetPath = "/workspace"
	b.SourceSubPath = "workspaces/engineering"
	l := &juiceFSLease{Version: 1, PodUID: a.PodUID, PVName: a.PV.Name, VolumeHandle: a.PV.VolumeHandle, Target: m.juiceFSStagePath(a), Fingerprint: juiceFSFingerprint(a), Consumers: map[string]string{juiceFSConsumer(a): a.SourceSubPath, juiceFSConsumer(b): b.SourceSubPath}}
	if err := m.writeJuiceFSLease(a, l); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(m.cfg.JuiceFSStateDir, juiceFSKey(a)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sensitive") {
		t.Fatal("secret persisted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.Release(ctx, a); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 0 {
		t.Fatal("source released while another consumer exists")
	}
	if err := m.Release(ctx, b); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 {
		t.Fatal("last consumer did not release source")
	}
	if err := m.Release(ctx, b); err != nil {
		t.Fatal(err)
	}
	if backend.calls != 1 {
		t.Fatal("release not idempotent")
	}
}

func TestJuiceFSKeySeparatesVolumesForOnePod(t *testing.T) {
	a := MountPlan{PV: PersistentVolume{Name: "volume-a", Driver: JuiceFSDriver}, PodUID: "pod"}
	b := MountPlan{PV: PersistentVolume{Name: "volume-b", Driver: JuiceFSDriver}, PodUID: "pod"}
	if juiceFSKey(a) == juiceFSKey(b) {
		t.Fatal("different PVs share one JuiceFS lease key")
	}
}

func TestJuiceFSKeyKeepsLegacyLeaseIdentity(t *testing.T) {
	plan := MountPlan{PV: PersistentVolume{Name: "shared", Driver: JuiceFSDriver}, PodUID: "pod"}
	want := sha256.Sum256([]byte("pod\x00shared"))
	if got := juiceFSKey(plan); got != hex.EncodeToString(want[:]) {
		t.Fatalf("lease key = %s, want legacy key %x", got, want)
	}
}

func TestJuiceFSPublishErrorDoesNotReleaseSharedSource(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "csi.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	backend := &releaseTestNode{publishErr: context.DeadlineExceeded}
	csi.RegisterNodeServer(server, backend)
	go server.Serve(listener)
	defer server.Stop()

	m := &linuxMounter{cfg: Config{JuiceFSSocket: socket, JuiceFSStateDir: filepath.Join(dir, "state"), JuiceFSPublishRoot: filepath.Join(dir, "publish")}}
	plan := MountPlan{PV: PersistentVolume{Name: "shared", Driver: JuiceFSDriver, VolumeHandle: "handle"}, PodUID: "pod", PodName: "pod", PodNamespace: "ns", ContainerName: "main", ContainerID: "container", TargetPath: "/new", SourceSubPath: "users/new", ReadOnly: true}
	existing := plan
	existing.TargetPath = "/existing"
	existing.SourceSubPath = "users/existing"
	lease := &juiceFSLease{Version: 1, PodUID: plan.PodUID, PVName: plan.PV.Name, VolumeHandle: plan.PV.VolumeHandle, Target: m.juiceFSStagePath(plan), Fingerprint: juiceFSFingerprint(plan), Consumers: map[string]string{juiceFSConsumer(existing): existing.SourceSubPath}}
	if err := m.writeJuiceFSLease(plan, lease); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.mountJuiceFS(ctx, plan); err == nil {
		t.Fatal("publish error was ignored")
	}
	if backend.calls != 0 {
		t.Fatalf("NodeUnpublishVolume called %d times for an ordinary publish error", backend.calls)
	}
	stored, err := m.readJuiceFSLease(plan)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.Consumers[juiceFSConsumer(existing)] != existing.SourceSubPath {
		t.Fatal("existing consumer was removed after another publish failed")
	}
}

func TestJuiceFSTargetWalkRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "1/root")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if fd, err := openContainerDirectory(dir, 1, "/escape/data", true); err == nil {
		unix.Close(fd)
		t.Fatal("target symlink accepted")
	}
	fd, err := openContainerDirectory(dir, 1, "/safe/data", true)
	if err != nil {
		t.Fatal(err)
	}
	unix.Close(fd)
	if fd, err := openContainerDirectory(dir, 1, "/safe/../escape", true); err == nil {
		unix.Close(fd)
		t.Fatal("parent traversal accepted")
	}
}
