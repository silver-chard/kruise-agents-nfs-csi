package wrapper

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/config"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
)

func TestReconcileMountRestoresMountAfterContainerRestart(t *testing.T) {
	server, store, kubeClient, mounter := newReconcileTestServer(t)
	desired := desiredMount{
		Request:     testMountRequest(),
		ContainerID: "containerd://old-container",
	}
	if err := store.Put(desired); err != nil {
		t.Fatalf("put desired mount: %v", err)
	}
	kubeClient.pod.Status.ContainerStatuses[0].ContainerID = "containerd://new-container"

	if err := server.reconcileMount(context.Background(), desired); err != nil {
		t.Fatalf("reconcileMount returned error: %v", err)
	}
	if len(mounter.mountCalls) != 1 {
		t.Fatalf("mount calls = %d, want 1", len(mounter.mountCalls))
	}
	if got := mounter.mountCalls[0].ContainerID; got != "containerd://new-container" {
		t.Fatalf("mounted container id = %q, want new container", got)
	}
	stored, ok := store.Get(desired.key())
	if !ok {
		t.Fatalf("desired mount disappeared after reconcile")
	}
	if stored.ContainerID != "containerd://new-container" {
		t.Fatalf("stored container id = %q, want new container", stored.ContainerID)
	}
}

func TestPodWatchEventRestoresMountAfterContainerRestart(t *testing.T) {
	server, store, kubeClient, mounter := newReconcileTestServer(t)
	desired := desiredMount{
		Request:     testMountRequest(),
		ContainerID: "containerd://old-container",
	}
	if err := store.Put(desired); err != nil {
		t.Fatalf("put desired mount: %v", err)
	}
	eventPod := *kubeClient.pod
	eventPod.Status.ContainerStatuses = append([]kube.ContainerStatus(nil), kubeClient.pod.Status.ContainerStatuses...)
	eventPod.Status.ContainerStatuses[0].ContainerID = "containerd://new-container"

	server.reconcilePodEvent(context.Background(), kube.PodWatchEvent{Type: "MODIFIED", Pod: eventPod})
	if len(mounter.mountCalls) != 1 {
		t.Fatalf("mount calls = %d, want 1", len(mounter.mountCalls))
	}
	if got := mounter.mountCalls[0].ContainerID; got != "containerd://new-container" {
		t.Fatalf("mounted container id = %q, want new container", got)
	}
}

func TestReconcileMountDoesNotStackMountInCurrentContainer(t *testing.T) {
	server, store, _, mounter := newReconcileTestServer(t)
	desired := desiredMount{
		Request:     testMountRequest(),
		ContainerID: "containerd://old-container",
	}
	if err := store.Put(desired); err != nil {
		t.Fatalf("put desired mount: %v", err)
	}
	mounter.mounted[mounterKey(desired.ContainerID, desired.Request.TargetPath)] = true

	if err := server.reconcileMount(context.Background(), desired); err != nil {
		t.Fatalf("reconcileMount returned error: %v", err)
	}
	if len(mounter.mountCalls) != 0 {
		t.Fatalf("mount calls = %d, want 0", len(mounter.mountCalls))
	}
}

func TestUnmountDeletesDesiredMountAndPreventsReconcile(t *testing.T) {
	server, store, _, mounter := newReconcileTestServer(t)
	desired := desiredMount{
		Request:     testMountRequest(),
		ContainerID: "containerd://old-container",
	}
	if err := store.Put(desired); err != nil {
		t.Fatalf("put desired mount: %v", err)
	}
	mounter.mounted[mounterKey(desired.ContainerID, desired.Request.TargetPath)] = true

	request := api.UnmountRequest{
		APIVersion:    desired.Request.APIVersion,
		DriverName:    desired.Request.DriverName,
		Namespace:     desired.Request.Namespace,
		PodName:       desired.Request.PodName,
		PodUID:        desired.Request.PodUID,
		PVName:        desired.Request.PVName,
		TargetPath:    desired.Request.TargetPath,
		ContainerName: desired.Request.ContainerName,
	}
	if _, err := server.unmount(context.Background(), "valid-token", request); err != nil {
		t.Fatalf("unmount returned error: %v", err)
	}
	if _, ok := store.Get(desired.key()); ok {
		t.Fatalf("desired mount still exists after unmount")
	}
	if len(mounter.unmountCalls) != 1 {
		t.Fatalf("unmount calls = %d, want 1", len(mounter.unmountCalls))
	}

	server.reconcileAll(context.Background())
	if len(mounter.mountCalls) != 0 {
		t.Fatalf("mount calls after explicit unmount = %d, want 0", len(mounter.mountCalls))
	}
}

func TestReconcileAllDeletesStateForDeletedPod(t *testing.T) {
	server, store, kubeClient, _ := newReconcileTestServer(t)
	desired := desiredMount{
		Request:     testMountRequest(),
		ContainerID: "containerd://old-container",
	}
	if err := store.Put(desired); err != nil {
		t.Fatalf("put desired mount: %v", err)
	}
	kubeClient.podErr = &kube.APIError{Method: http.MethodGet, Path: "/api/v1/pods/pod-a", StatusCode: http.StatusNotFound, Status: "404 Not Found"}

	server.reconcileAll(context.Background())
	if _, ok := store.Get(desired.key()); ok {
		t.Fatalf("desired mount still exists after pod deletion")
	}
}

func TestFileMountStateStoreReloadsDesiredMount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mount-state")
	store, err := newFileMountStateStore(dir)
	if err != nil {
		t.Fatalf("newFileMountStateStore: %v", err)
	}
	desired := desiredMount{Request: testMountRequest(), ContainerID: "containerd://container-a"}
	if err := store.Put(desired); err != nil {
		t.Fatalf("put desired mount: %v", err)
	}

	reloaded, err := newFileMountStateStore(dir)
	if err != nil {
		t.Fatalf("reload mount state: %v", err)
	}
	got, ok := reloaded.Get(desired.key())
	if !ok {
		t.Fatalf("reloaded store did not contain desired mount")
	}
	if got != desired {
		t.Fatalf("reloaded desired mount = %#v, want %#v", got, desired)
	}
}

func TestFileMountStateStoreUsesOneProtectedFilePerMountAndPodIndex(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mount-state")
	store, err := newFileMountStateStore(dir)
	if err != nil {
		t.Fatalf("newFileMountStateStore: %v", err)
	}
	first := desiredMount{Request: testMountRequest(), ContainerID: "containerd://first"}
	second := first
	second.Request.Namespace = "other-ns"
	second.Request.PodName = "pod-b"
	second.Request.PodUID = "pod-b-uid"
	second.ContainerID = "containerd://second"
	for _, desired := range []desiredMount{first, second} {
		if err := store.Put(desired); err != nil {
			t.Fatalf("put desired mount: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read mount state directory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("mount state files = %d, want one per mount", len(entries))
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat mount state %s: %v", entry.Name(), err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("mount state %s mode = %#o, want 0600", entry.Name(), got)
		}
	}

	firstMounts := store.ListForPod(desiredPodKeyFor(first.Request))
	if len(firstMounts) != 1 || firstMounts[0] != first {
		t.Fatalf("first pod mounts = %#v, want only first mount", firstMounts)
	}
	first.ContainerID = "containerd://restarted"
	if err := store.Put(first); err != nil {
		t.Fatalf("update desired mount: %v", err)
	}
	entries, err = os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read updated mount state directory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("mount state files after update = %d, want 2", len(entries))
	}
	if err := store.Delete(first.key()); err != nil {
		t.Fatalf("delete desired mount: %v", err)
	}
	if mounts := store.ListForPod(desiredPodKeyFor(first.Request)); len(mounts) != 0 {
		t.Fatalf("first pod index retained deleted mount: %#v", mounts)
	}
}

func newReconcileTestServer(t *testing.T) (*Server, *fileMountStateStore, *reconcileTestKube, *reconcileTestMounter) {
	t.Helper()
	request := testMountRequest()
	kubeClient := &reconcileTestKube{
		tokenStatus: &kube.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{"test-audience"},
			User: kube.TokenReviewUser{
				Username: "system:serviceaccount:sandbox-ns:sandbox-sa",
			},
		},
		pod: &kube.Pod{
			Metadata: kube.ObjectMeta{Name: request.PodName, Namespace: request.Namespace, UID: request.PodUID},
			Spec: kube.PodSpec{
				ServiceAccountName: "sandbox-sa",
				NodeName:           "node-a",
				Containers:         []kube.Container{{Name: request.ContainerName}},
			},
			Status: kube.PodStatus{
				Phase: "Running",
				ContainerStatuses: []kube.ContainerStatus{{
					Name:        request.ContainerName,
					ContainerID: "containerd://old-container",
					Ready:       true,
				}},
			},
		},
		pv: &kube.PersistentVolume{
			Metadata: kube.ObjectMeta{Name: request.PVName},
			Spec: kube.PersistentVolumeSpec{
				ClaimRef: &kube.ObjectReference{Namespace: request.Namespace, Name: "pvc-a", UID: "pvc-uid"},
				CSI: &kube.CSIPersistentVolumeSource{
					Driver:       request.DriverName,
					VolumeHandle: "volume-a",
					VolumeAttributes: map[string]string{
						"server": "nfs.example.internal",
						"share":  "/exports/workloads",
					},
				},
			},
		},
		pvc: &kube.PersistentVolumeClaimResource{
			Metadata: kube.ObjectMeta{Name: "pvc-a", Namespace: request.Namespace, UID: "pvc-uid"},
		},
	}
	mounter := &reconcileTestMounter{mounted: make(map[string]bool)}
	store, err := newFileMountStateStore(filepath.Join(t.TempDir(), "mount-state"))
	if err != nil {
		t.Fatalf("newFileMountStateStore: %v", err)
	}
	server := newServerWithState(config.WrapperConfig{
		DriverName:     request.DriverName,
		TokenAudience:  "test-audience",
		RequestTimeout: time.Second,
		NodeName:       "node-a",
	}, kubeClient, mounter, store, log.New(io.Discard, "", 0))
	return server, store, kubeClient, mounter
}

func testMountRequest() api.MountRequest {
	return api.MountRequest{
		APIVersion:    api.Version,
		DriverName:    "csi.nfs.zhida",
		Namespace:     "sandbox-ns",
		PodName:       "pod-a",
		PodUID:        "pod-uid",
		PVName:        "pv-a",
		SourceSubPath: "users/alice",
		TargetPath:    "/workspace/data",
		ContainerName: "main",
	}
}

type reconcileTestKube struct {
	tokenStatus *kube.TokenReviewStatus
	pod         *kube.Pod
	podErr      error
	pv          *kube.PersistentVolume
	pvc         *kube.PersistentVolumeClaimResource
}

func (k *reconcileTestKube) ReviewToken(context.Context, string, []string) (*kube.TokenReviewStatus, error) {
	return k.tokenStatus, nil
}

func (k *reconcileTestKube) GetPod(context.Context, string, string) (*kube.Pod, error) {
	return k.pod, k.podErr
}

func (k *reconcileTestKube) RunPodInformer(ctx context.Context, _ string, synced func([]kube.Pod), _ func(kube.PodWatchEvent)) error {
	if k.podErr != nil {
		return k.podErr
	}
	synced([]kube.Pod{*k.pod})
	<-ctx.Done()
	return ctx.Err()
}

func (k *reconcileTestKube) GetPersistentVolume(context.Context, string) (*kube.PersistentVolume, error) {
	return k.pv, nil
}

func (k *reconcileTestKube) GetPersistentVolumeClaim(context.Context, string, string) (*kube.PersistentVolumeClaimResource, error) {
	return k.pvc, nil
}

type reconcileTestMounter struct {
	mounted      map[string]bool
	mountCalls   []node.MountPlan
	unmountCalls []node.MountPlan
}

func (m *reconcileTestMounter) Mount(_ context.Context, plan node.MountPlan) error {
	m.mountCalls = append(m.mountCalls, plan)
	m.mounted[mounterKey(plan.ContainerID, plan.TargetPath)] = true
	return nil
}

func (m *reconcileTestMounter) Unmount(_ context.Context, plan node.MountPlan) error {
	m.unmountCalls = append(m.unmountCalls, plan)
	delete(m.mounted, mounterKey(plan.ContainerID, plan.TargetPath))
	return nil
}

func (m *reconcileTestMounter) IsMounted(_ context.Context, plan node.MountPlan) (bool, error) {
	return m.mounted[mounterKey(plan.ContainerID, plan.TargetPath)], nil
}

func mounterKey(containerID, targetPath string) string {
	return containerID + "\x00" + targetPath
}
