package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

func TestMountExportRootWithoutConfiguredKeyPersistsUnkeyedRootIntent(t *testing.T) {
	server, store, _, mounter := newReconcileTestServer(t)
	request := testMountRequest()
	request.SourceSubPath = ""

	result, err := server.mount(context.Background(), "valid-token", "", request)
	if err != nil {
		t.Fatalf("mount export root without configured key: %v", err)
	}
	if !result.Mounted || len(mounter.mountCalls) != 1 {
		t.Fatalf("mount result=%#v calls=%d, want successful mount", result, len(mounter.mountCalls))
	}
	normalizedRequest := request
	normalizedRequest.ContainerName = "main"
	stored, exists := store.Get(desiredMountKeyFor(normalizedRequest))
	if !exists || !stored.ExportRootAuthorized {
		t.Fatalf("stored root intent = %#v, want NFS export root", stored)
	}
	if stored.ExportRootKeyFingerprint != "" {
		t.Fatalf("stored root authorization = %#v, want unkeyed state", stored)
	}
}

func TestMountExportRootWithConfiguredKeyRequiresKeyAndPersistsAuthorization(t *testing.T) {
	server, store, _, mounter := newReconcileTestServer(t)
	request := testMountRequest()
	request.SourceSubPath = ""

	key := "0123456789abcdef0123456789abcdef"
	server.exportRoot = testExportRootAuthorizer(t, key)
	if _, err := server.mount(context.Background(), "valid-token", "", request); err == nil {
		t.Fatal("mount export root without key succeeded")
	}
	if len(mounter.mountCalls) != 0 {
		t.Fatalf("mount calls after rejected root mount = %d, want 0", len(mounter.mountCalls))
	}

	result, err := server.mount(context.Background(), "valid-token", key, request)
	if err != nil {
		t.Fatalf("mount export root with key: %v", err)
	}
	if !result.Mounted || len(mounter.mountCalls) != 1 {
		t.Fatalf("mount result=%#v calls=%d, want successful mount", result, len(mounter.mountCalls))
	}
	normalizedRequest := request
	normalizedRequest.ContainerName = "main"
	stored, exists := store.Get(desiredMountKeyFor(normalizedRequest))
	if !exists || !stored.ExportRootAuthorized || stored.ExportRootKeyFingerprint == "" {
		t.Fatalf("stored root authorization = %#v, want authorized state", stored)
	}
	if stored.ExportRootKeyFingerprint != server.exportRoot.fingerprint {
		t.Fatalf("stored fingerprint = %q, want current authorizer fingerprint", stored.ExportRootKeyFingerprint)
	}
	data, err := os.ReadFile(filepath.Join(store.dir, mountStateFilename(desiredMountKeyFor(normalizedRequest))))
	if err != nil {
		t.Fatalf("read stored root mount: %v", err)
	}
	if strings.Contains(string(data), key) {
		t.Fatal("mount state contains the export root key")
	}
}

func TestReconcileExportRootUsesCurrentOptionalKeyPolicy(t *testing.T) {
	tests := []struct {
		name        string
		configured  bool
		legacy      bool
		authorized  bool
		fingerprint string
		wantBlocked bool
		wantUnkeyed bool
	}{
		{name: "unconfigured key restores unkeyed state", authorized: true, wantUnkeyed: true},
		{name: "unconfigured key clears old fingerprint", authorized: true, fingerprint: strings.Repeat("f", 64), wantUnkeyed: true},
		{name: "unconfigured key rejects unknown root intent", wantBlocked: true},
		{name: "unconfigured key resolves legacy root intent", legacy: true, wantUnkeyed: true},
		{name: "configured key rejects unknown root intent", configured: true, wantBlocked: true},
		{name: "configured key rejects legacy root intent", configured: true, legacy: true, wantBlocked: true},
		{name: "configured key rejects missing fingerprint", configured: true, authorized: true, wantBlocked: true},
		{name: "configured key rejects old fingerprint", configured: true, authorized: true, fingerprint: strings.Repeat("f", 64), wantBlocked: true},
		{name: "configured key restores current fingerprint", configured: true, authorized: true, fingerprint: "current"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, store, kubeClient, mounter := newReconcileTestServer(t)
			key := "0123456789abcdef0123456789abcdef"
			if test.configured {
				server.exportRoot = testExportRootAuthorizer(t, key)
			}
			fingerprint := test.fingerprint
			if fingerprint == "current" {
				fingerprint = server.exportRoot.fingerprint
			}
			desired := desiredMount{
				Request:                         testMountRequest(),
				ContainerID:                     "containerd://old-container",
				legacyRootClassificationUnknown: test.legacy,
				ExportRootAuthorized:            test.authorized,
				ExportRootKeyFingerprint:        fingerprint,
			}
			desired.Request.SourceSubPath = ""
			if err := store.Put(desired); err != nil {
				t.Fatalf("put desired mount: %v", err)
			}
			kubeClient.pod.Status.ContainerStatuses[0].ContainerID = "containerd://new-container"

			err := server.reconcileMount(context.Background(), desired)
			if test.wantBlocked {
				if !errors.Is(err, errDesiredMountUnauthorized) {
					t.Fatalf("reconcile error = %v, want blocked root authorization error", err)
				}
				if len(mounter.mountCalls) != 0 {
					t.Fatalf("mount calls = %d, want 0", len(mounter.mountCalls))
				}
				server.handleReconcileResult(desired, err)
				if _, exists := store.Get(desired.key()); !exists {
					t.Fatal("blocked desired mount was deleted")
				}
				return
			}
			if err != nil {
				t.Fatalf("reconcileMount returned error: %v", err)
			}
			if len(mounter.mountCalls) != 1 {
				t.Fatalf("mount calls = %d, want 1", len(mounter.mountCalls))
			}
			if test.wantUnkeyed {
				stored, exists := store.Get(desired.key())
				if !exists || stored.legacyRootClassificationUnknown || !stored.ExportRootAuthorized || stored.ExportRootKeyFingerprint != "" {
					t.Fatalf("reconciled state = %#v, want unkeyed export-root state", stored)
				}
			}
		})
	}
}

func TestUnauthorizedDesiredMountIsNotScheduledForRetry(t *testing.T) {
	server, _, _, _ := newReconcileTestServer(t)
	retries := make(chan reconcileRetry, 1)
	server.scheduleReconcileRetry(context.Background(), retries, desiredMount{Request: testMountRequest()}, errDesiredMountUnauthorized)
	select {
	case retry := <-retries:
		t.Fatalf("unexpected retry scheduled: %#v", retry)
	default:
	}
}

func TestMountUpgradesUnkeyedExportRootAuthorizationWithoutRemount(t *testing.T) {
	server, store, kubeClient, mounter := newReconcileTestServer(t)
	request := testMountRequest()
	request.SourceSubPath = ""
	if _, err := server.mount(context.Background(), "valid-token", "", request); err != nil {
		t.Fatalf("initial unkeyed export-root mount: %v", err)
	}

	key := "0123456789abcdef0123456789abcdef"
	server.exportRoot = testExportRootAuthorizer(t, key)
	normalized := request
	normalized.ContainerName = "main"
	unkeyed, exists := store.Get(desiredMountKeyFor(normalized))
	if !exists {
		t.Fatal("initial unkeyed desired mount not found")
	}
	kubeClient.pod.Status.ContainerStatuses[0].ContainerID = "containerd://new-container"
	blockedErr := server.reconcileMount(context.Background(), unkeyed)
	if !errors.Is(blockedErr, errDesiredMountUnauthorized) {
		t.Fatalf("reconcile unkeyed state under keyed policy error = %v, want unauthorized", blockedErr)
	}
	server.handleReconcileResult(unkeyed, blockedErr)
	if _, exists := store.Get(unkeyed.key()); !exists {
		t.Fatal("unkeyed desired mount was deleted before it could be upgraded")
	}
	kubeClient.pod.Status.ContainerStatuses[0].ContainerID = "containerd://old-container"

	if _, err := server.mount(context.Background(), "valid-token", key, request); err != nil {
		t.Fatalf("upgrade export-root authorization: %v", err)
	}
	if len(mounter.mountCalls) != 1 {
		t.Fatalf("mount calls = %d, want one initial mount", len(mounter.mountCalls))
	}

	stored, exists := store.Get(desiredMountKeyFor(normalized))
	if !exists || !stored.ExportRootAuthorized || stored.ExportRootKeyFingerprint != server.exportRoot.fingerprint {
		t.Fatalf("upgraded state = %#v, want current keyed export-root authorization", stored)
	}

	kubeClient.pod.Status.ContainerStatuses[0].ContainerID = "containerd://new-container"
	if err := server.reconcileMount(context.Background(), stored); err != nil {
		t.Fatalf("reconcile upgraded export-root mount: %v", err)
	}
	if len(mounter.mountCalls) != 2 {
		t.Fatalf("mount calls after container restart = %d, want 2", len(mounter.mountCalls))
	}
}

func TestReconcileRejectsLiveExportRootClassificationChange(t *testing.T) {
	server, store, kubeClient, mounter := newReconcileTestServer(t)
	desired := desiredMount{
		Request:              testMountRequest(),
		ContainerID:          "containerd://old-container",
		ExportRootAuthorized: true,
	}
	desired.Request.SourceSubPath = ""
	if err := store.Put(desired); err != nil {
		t.Fatalf("put export-root desired mount: %v", err)
	}
	kubeClient.pv.Spec.CSI.VolumeAttributes["subDir"] = "tenants/a"
	kubeClient.pod.Status.ContainerStatuses[0].ContainerID = "containerd://new-container"

	err := server.reconcileMount(context.Background(), desired)
	if !errors.Is(err, errDesiredMountUnauthorized) {
		t.Fatalf("reconcile changed root classification error = %v, want unauthorized", err)
	}
	if len(mounter.mountCalls) != 0 {
		t.Fatalf("mount calls = %d, want 0", len(mounter.mountCalls))
	}
	server.handleReconcileResult(desired, err)
	if _, exists := store.Get(desired.key()); !exists {
		t.Fatal("blocked desired mount was deleted")
	}
}

func TestMountDowngradesExportRootAuthorizationWhenKeyDisabledWithoutRemount(t *testing.T) {
	server, store, kubeClient, mounter := newReconcileTestServer(t)
	request := testMountRequest()
	request.SourceSubPath = ""
	key := "0123456789abcdef0123456789abcdef"
	server.exportRoot = testExportRootAuthorizer(t, key)
	if _, err := server.mount(context.Background(), "valid-token", key, request); err != nil {
		t.Fatalf("initial keyed export-root mount: %v", err)
	}

	server.exportRoot = exportRootAuthorizer{}
	if _, err := server.mount(context.Background(), "valid-token", "", request); err != nil {
		t.Fatalf("downgrade export-root authorization: %v", err)
	}
	if len(mounter.mountCalls) != 1 {
		t.Fatalf("mount calls = %d, want one initial mount", len(mounter.mountCalls))
	}

	normalized := request
	normalized.ContainerName = "main"
	stored, exists := store.Get(desiredMountKeyFor(normalized))
	if !exists || !stored.ExportRootAuthorized || stored.ExportRootKeyFingerprint != "" {
		t.Fatalf("downgraded state = %#v, want unkeyed export-root authorization", stored)
	}

	kubeClient.pod.Status.ContainerStatuses[0].ContainerID = "containerd://new-container"
	if err := server.reconcileMount(context.Background(), stored); err != nil {
		t.Fatalf("reconcile unkeyed export-root mount: %v", err)
	}
	if len(mounter.mountCalls) != 2 {
		t.Fatalf("mount calls after container restart = %d, want 2", len(mounter.mountCalls))
	}
}

func TestMountRejectsChangedIntentAtExistingTarget(t *testing.T) {
	t.Run("request changes", func(t *testing.T) {
		server, _, _, mounter := newReconcileTestServer(t)
		request := testMountRequest()
		if _, err := server.mount(context.Background(), "valid-token", "", request); err != nil {
			t.Fatalf("initial mount: %v", err)
		}
		request.SourceSubPath = "users/bob"
		if _, err := server.mount(context.Background(), "valid-token", "", request); !errors.Is(err, errBadRequest) {
			t.Fatalf("changed mount request error = %v, want bad request", err)
		}
		if len(mounter.mountCalls) != 1 {
			t.Fatalf("mount calls = %d, want one initial mount", len(mounter.mountCalls))
		}
	})

	t.Run("PV subdir changes root classification", func(t *testing.T) {
		server, _, kubeClient, mounter := newReconcileTestServer(t)
		request := testMountRequest()
		request.SourceSubPath = ""
		if _, err := server.mount(context.Background(), "valid-token", "", request); err != nil {
			t.Fatalf("initial export-root mount: %v", err)
		}
		kubeClient.pv.Spec.CSI.VolumeAttributes["subDir"] = "tenants/a"
		if _, err := server.mount(context.Background(), "valid-token", "", request); !errors.Is(err, errBadRequest) {
			t.Fatalf("changed root classification error = %v, want bad request", err)
		}
		if len(mounter.mountCalls) != 1 {
			t.Fatalf("mount calls = %d, want one initial mount", len(mounter.mountCalls))
		}
	})
}

func TestMountRefreshesExportRootFingerprintWithoutRemount(t *testing.T) {
	server, store, _, mounter := newReconcileTestServer(t)
	request := testMountRequest()
	request.SourceSubPath = ""

	oldKey := "0123456789abcdef0123456789abcdef"
	server.exportRoot = testExportRootAuthorizer(t, oldKey)
	if _, err := server.mount(context.Background(), "valid-token", oldKey, request); err != nil {
		t.Fatalf("initial export-root mount: %v", err)
	}

	newKey := "fedcba9876543210fedcba9876543210"
	server.exportRoot = testExportRootAuthorizer(t, newKey)
	if _, err := server.mount(context.Background(), "valid-token", newKey, request); err != nil {
		t.Fatalf("refresh export-root authorization: %v", err)
	}
	if len(mounter.mountCalls) != 1 {
		t.Fatalf("mount calls = %d, want one initial mount", len(mounter.mountCalls))
	}

	normalized := request
	normalized.ContainerName = "main"
	stored, exists := store.Get(desiredMountKeyFor(normalized))
	if !exists {
		t.Fatal("desired mount disappeared after key refresh")
	}
	if stored.ExportRootKeyFingerprint != server.exportRoot.fingerprint {
		t.Fatalf("stored fingerprint = %q, want current fingerprint", stored.ExportRootKeyFingerprint)
	}
}

func TestUnmountDeletesDesiredMountAndPreventsReconcile(t *testing.T) {
	server, store, kubeClient, mounter := newReconcileTestServer(t)
	desired := desiredMount{
		Request:     testMountRequest(),
		ContainerID: "containerd://old-container",
	}
	if err := store.Put(desired); err != nil {
		t.Fatalf("put desired mount: %v", err)
	}
	mounter.mounted[mounterKey(desired.ContainerID, desired.Request.TargetPath)] = true
	kubeClient.pv.Metadata.Annotations = map[string]string{allowNamespaceAnnotation: "revoked-ns"}

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

func TestUnmountWithoutDesiredStateDoesNotUnmountUnknownMount(t *testing.T) {
	server, _, _, mounter := newReconcileTestServer(t)
	request := testMountRequest()
	mounter.mounted[mounterKey("containerd://old-container", request.TargetPath)] = true

	result, err := server.unmount(context.Background(), "valid-token", api.UnmountRequest{
		APIVersion:    request.APIVersion,
		DriverName:    request.DriverName,
		Namespace:     request.Namespace,
		PodName:       request.PodName,
		PodUID:        request.PodUID,
		PVName:        request.PVName,
		TargetPath:    request.TargetPath,
		ContainerName: request.ContainerName,
	})
	if err != nil {
		t.Fatalf("unmount returned error: %v", err)
	}
	if !result.Unmounted {
		t.Fatal("Unmounted = false, want idempotent success")
	}
	if len(mounter.unmountCalls) != 0 {
		t.Fatalf("unmount calls = %d, want 0", len(mounter.unmountCalls))
	}
}

func TestUnmountStaleContainerStateDoesNotTouchNewContainerMount(t *testing.T) {
	server, store, kubeClient, mounter := newReconcileTestServer(t)
	desired := desiredMount{
		Request:     testMountRequest(),
		ContainerID: "containerd://old-container",
	}
	if err := store.Put(desired); err != nil {
		t.Fatalf("put desired mount: %v", err)
	}
	kubeClient.pod.Status.ContainerStatuses[0].ContainerID = "containerd://new-container"
	mounter.mounted[mounterKey("containerd://new-container", desired.Request.TargetPath)] = true

	result, err := server.unmount(context.Background(), "valid-token", api.UnmountRequest{
		APIVersion:    desired.Request.APIVersion,
		DriverName:    desired.Request.DriverName,
		Namespace:     desired.Request.Namespace,
		PodName:       desired.Request.PodName,
		PodUID:        desired.Request.PodUID,
		PVName:        desired.Request.PVName,
		TargetPath:    desired.Request.TargetPath,
		ContainerName: desired.Request.ContainerName,
	})
	if err != nil {
		t.Fatalf("unmount stale state: %v", err)
	}
	if !result.Unmounted {
		t.Fatal("Unmounted = false, want idempotent success")
	}
	if len(mounter.unmountCalls) != 0 {
		t.Fatalf("unmount calls = %d, want 0", len(mounter.unmountCalls))
	}
	if !mounter.mounted[mounterKey("containerd://new-container", desired.Request.TargetPath)] {
		t.Fatal("new container mount was removed")
	}
	if _, exists := store.Get(desired.key()); exists {
		t.Fatal("stale desired mount still exists")
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
	data, err := os.ReadFile(filepath.Join(dir, mountStateFilename(desired.key())))
	if err != nil {
		t.Fatalf("read current mount state: %v", err)
	}
	var state mountStateFile
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode current mount state: %v", err)
	}
	if state.Version != mountStateVersion {
		t.Fatalf("mount state version = %d, want %d", state.Version, mountStateVersion)
	}
}

func TestFileMountStateStoreLoadsLegacyStateWithoutRootAuthorization(t *testing.T) {
	dir := t.TempDir()
	desired := desiredMount{
		Request:                  testMountRequest(),
		ContainerID:              "containerd://container-a",
		ExportRootAuthorized:     true,
		ExportRootKeyFingerprint: strings.Repeat("f", 64),
	}
	payload, err := json.Marshal(mountStateFile{Version: legacyMountStateVersion, Mount: desired})
	if err != nil {
		t.Fatalf("encode legacy mount state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, mountStateFilename(desired.key())), payload, 0o600); err != nil {
		t.Fatalf("write legacy mount state: %v", err)
	}

	store, err := newFileMountStateStore(dir)
	if err != nil {
		t.Fatalf("load legacy mount state: %v", err)
	}
	got, exists := store.Get(desired.key())
	if !exists {
		t.Fatal("legacy desired mount was not loaded")
	}
	if got.ExportRootAuthorized {
		t.Fatal("legacy desired mount unexpectedly has export root authorization")
	}
	if got.ExportRootKeyFingerprint != "" {
		t.Fatal("legacy desired mount unexpectedly has an export root key fingerprint")
	}
	if !got.legacyRootClassificationUnknown {
		t.Fatal("legacy desired mount root classification is not marked unknown")
	}
}

func testExportRootAuthorizer(t *testing.T, key string) exportRootAuthorizer {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "export-root-key")
	if err := os.WriteFile(keyFile, []byte(key), 0o600); err != nil {
		t.Fatalf("write export root key: %v", err)
	}
	authorizer, err := loadExportRootAuthorizer(keyFile)
	if err != nil {
		t.Fatalf("load export root key: %v", err)
	}
	return authorizer
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
				Extra: map[string][]string{
					tokenPodNameExtraKey: {request.PodName},
					tokenPodUIDExtraKey:  {request.PodUID},
				},
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
	}
	mounter := &reconcileTestMounter{mounted: make(map[string]bool)}
	store, err := newFileMountStateStore(filepath.Join(t.TempDir(), "mount-state"))
	if err != nil {
		t.Fatalf("newFileMountStateStore: %v", err)
	}
	server, err := newServerWithState(config.WrapperConfig{
		DriverName:     request.DriverName,
		TokenAudience:  "test-audience",
		RequestTimeout: time.Second,
		NodeName:       "node-a",
	}, kubeClient, mounter, store, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newServerWithState: %v", err)
	}
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
