package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
)

func TestJuiceFSUnmountDetachesUnhealthyTarget(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		for _, healthErr := range []error{nil, errors.New("transport endpoint is not connected")} {
			t.Run(fmt.Sprintf("readOnly=%t/healthError=%t", readOnly, healthErr != nil), func(t *testing.T) {
				server, store, _, mounter, desired := newJuiceFSUnmountTestServer(t, readOnly)
				// IsMounted would report either a mode mismatch or a dead FUSE
				// source. Neither outcome may bypass detaching the actual target.
				mounter.healthErr = healthErr
				result, err := server.unmount(context.Background(), "valid-token", juiceFSUnmountRequest(desired.Request))
				if err != nil || result == nil || !result.Unmounted {
					t.Fatalf("unmount unhealthy target = %+v, %v", result, err)
				}
				if len(mounter.unmountCalls) != 1 {
					t.Fatalf("unmount calls = %d, want 1", len(mounter.unmountCalls))
				}
				assertJuiceFSUnmountPlan(t, mounter.unmountCalls[0], desired)
				if mounter.healthCalls != 0 || len(mounter.releaseCalls) != 0 {
					t.Fatalf("health checks = %d, source-only releases = %d; want neither", mounter.healthCalls, len(mounter.releaseCalls))
				}
				if _, exists := store.Get(desired.key()); exists {
					t.Fatal("desired state remains after successful unmount")
				}
				if mounter.mounted[mounterKey(desired.ContainerID, desired.Request.TargetPath)] {
					t.Fatal("unhealthy target remains mounted")
				}
			})
		}
	}
}

func TestJuiceFSUnmountBusyPreservesDesiredState(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("readOnly=%t", readOnly), func(t *testing.T) {
			server, store, _, mounter, desired := newJuiceFSUnmountTestServer(t, readOnly)
			mounter.unmountErr = syscall.EBUSY
			request := juiceFSUnmountRequest(desired.Request)
			result, err := server.unmount(context.Background(), "valid-token", request)
			if result != nil || !errors.Is(err, syscall.EBUSY) || !errors.Is(err, errNodeOperation) {
				t.Fatalf("busy unmount = %+v, %v; want node operation and EBUSY", result, err)
			}
			if len(mounter.unmountCalls) != 1 {
				t.Fatalf("unmount calls = %d, want 1", len(mounter.unmountCalls))
			}
			assertJuiceFSUnmountPlan(t, mounter.unmountCalls[0], desired)
			if got, exists := store.Get(desired.key()); !exists || got != desired {
				t.Fatalf("state after busy unmount = %+v, exists=%t; want original intent", got, exists)
			}
			// The rollback must survive a wrapper restart, not only restore the
			// current process's in-memory desired state.
			reloaded, err := newFileMountStateStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			if got, exists := reloaded.Get(desired.key()); !exists || got != desired {
				t.Fatalf("persisted state after busy unmount = %+v, exists=%t", got, exists)
			}
			if len(mounter.releaseCalls) != 0 || !mounter.mounted[mounterKey(desired.ContainerID, desired.Request.TargetPath)] {
				t.Fatal("busy target or its source was released")
			}
			mounter.unmountErr = nil
			if _, err := server.unmount(context.Background(), "valid-token", request); err != nil {
				t.Fatalf("retry unmount: %v", err)
			}
			if len(mounter.unmountCalls) != 2 {
				t.Fatalf("unmount calls after retry = %d, want 2", len(mounter.unmountCalls))
			}
			if _, exists := store.Get(desired.key()); exists {
				t.Fatal("desired state remains after successful retry")
			}
		})
	}
}

func TestJuiceFSReconcileRevokedWritableGrantPreservesMode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		downgrade  bool
		unmountErr error
	}{
		{name: "removed grant"},
		{name: "downgraded grant", downgrade: true},
		{name: "busy revoked target", downgrade: true, unmountErr: syscall.EBUSY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, store, kubeClient, mounter, desired := newJuiceFSUnmountTestServer(t, false)
			setJuiceFSGrantForTest(t, kubeClient.pod, desired.Request)
			if err := authorizeJuiceFSMount(kubeClient.pod, desired.Request); err != nil {
				t.Fatalf("initial writable grant is invalid: %v", err)
			}
			if tc.downgrade {
				readonlyRequest := desired.Request
				readonlyRequest.ReadOnly = true
				setJuiceFSGrantForTest(t, kubeClient.pod, readonlyRequest)
			} else {
				delete(kubeClient.pod.Metadata.Annotations, JuiceFSAllowedMountsAnnotation)
			}
			mounter.unmountErr = tc.unmountErr
			err := server.reconcileMount(context.Background(), desired)
			if len(mounter.unmountCalls) != 1 {
				t.Fatalf("revocation unmount calls = %d, want 1", len(mounter.unmountCalls))
			}
			assertJuiceFSUnmountPlan(t, mounter.unmountCalls[0], desired)
			if tc.unmountErr != nil {
				if !errors.Is(err, tc.unmountErr) || errors.Is(err, errDesiredMountStale) {
					t.Fatalf("busy revoked target = %v; want retryable unmount error", err)
				}
			} else if !errors.Is(err, errDesiredMountStale) || !errors.Is(err, errDesiredMountSourceReleased) {
				t.Fatalf("revoked target = %v; want stale state with source already released", err)
			}
			server.handleReconcileResult(desired, err)
			got, exists := store.Get(desired.key())
			if tc.unmountErr != nil {
				if !exists || got != desired || !shouldRetryReconcile(err) {
					t.Fatalf("busy revocation did not retain retryable original state: %+v, exists=%t", got, exists)
				}
			} else if exists {
				t.Fatal("revoked desired state remains after successful detach")
			}
			if len(mounter.releaseCalls) != 0 || len(mounter.mountCalls) != 0 || mounter.healthCalls != 0 {
				t.Fatalf("revocation made extra calls: release=%d mount=%d health=%d", len(mounter.releaseCalls), len(mounter.mountCalls), mounter.healthCalls)
			}
		})
	}
}

func TestJuiceFSMountCleanupPendingKeepsRevocableState(t *testing.T) {
	for _, reconcile := range []bool{false, true} {
		name := "initial mount"
		if reconcile {
			name = "reconcile restarted container"
		}
		t.Run(name, func(t *testing.T) {
			server, store, kubeClient, mounter, desired := newJuiceFSUnmountTestServer(t, false)
			delete(mounter.mounted, mounterKey(desired.ContainerID, desired.Request.TargetPath))
			server.cfg.JuiceFSSecretRefs = "kube-system/drive-secret"
			server.kube = &juiceFSUnmountSecretTestKube{reconcileTestKube: kubeClient}
			kubeClient.pv.Spec.CSI = &kube.CSIPersistentVolumeSource{
				Driver: node.JuiceFSDriver, VolumeHandle: "drive-handle",
				VolumeAttributes:     map[string]string{"name": "drive"},
				NodePublishSecretRef: &kube.SecretReference{Namespace: "kube-system", Name: "drive-secret"},
			}
			setJuiceFSGrantForTest(t, kubeClient.pod, desired.Request)
			mounter.mountErr = errors.Join(node.ErrMountCleanupPending, syscall.EBUSY)
			want := desired
			var mountErr error
			if reconcile {
				want.ContainerID = "containerd://restarted-container"
				kubeClient.pod.Status.ContainerStatuses[0].ContainerID = want.ContainerID
				mountErr = server.reconcileMount(context.Background(), desired)
				server.handleReconcileResult(desired, mountErr)
			} else {
				if err := store.Delete(desired.key()); err != nil {
					t.Fatal(err)
				}
				result, err := server.mount(context.Background(), "valid-token", "", desired.Request)
				mountErr = err
				if result != nil || !errors.Is(err, errNodeOperation) {
					t.Fatalf("incomplete mount = %+v, %v; want node operation error", result, err)
				}
			}
			if !errors.Is(mountErr, node.ErrMountCleanupPending) || !errors.Is(mountErr, syscall.EBUSY) {
				t.Fatalf("mount error = %v; want cleanup pending and EBUSY", mountErr)
			}
			if len(mounter.mountCalls) != 1 || mounter.mountCalls[0].ContainerID != want.ContainerID {
				t.Fatalf("mount calls = %+v; want attempted mount in %s", mounter.mountCalls, want.ContainerID)
			}
			if got, exists := store.Get(want.key()); !exists || got != want {
				t.Fatalf("pending mount state = %+v, exists=%t; want %+v", got, exists, want)
			}
			// Reload the persisted state before revocation: a wrapper restart
			// must not lose ownership of the still-attached writable target.
			reloaded, err := newFileMountStateStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			persisted, exists := reloaded.Get(want.key())
			if !exists || persisted != want {
				t.Fatalf("persisted pending mount = %+v, exists=%t; want %+v", persisted, exists, want)
			}
			if len(mounter.releaseCalls) != 0 || len(mounter.unmountCalls) != 0 {
				t.Fatal("pending target was forgotten or its source released before revocation")
			}
			server.state = reloaded
			delete(kubeClient.pod.Metadata.Annotations, JuiceFSAllowedMountsAnnotation)
			err = server.reconcileMount(context.Background(), persisted)
			if !errors.Is(err, errDesiredMountStale) || !errors.Is(err, errDesiredMountSourceReleased) {
				t.Fatalf("revoke pending target = %v; want completed detach", err)
			}
			if len(mounter.unmountCalls) != 1 {
				t.Fatalf("revocation unmount calls = %d, want 1", len(mounter.unmountCalls))
			}
			assertJuiceFSUnmountPlan(t, mounter.unmountCalls[0], want)
			server.handleReconcileResult(persisted, err)
			if _, exists := reloaded.Get(want.key()); exists {
				t.Fatal("pending state remains after revocation detached the target")
			}
			if mounter.mounted[mounterKey(want.ContainerID, want.Request.TargetPath)] || len(mounter.releaseCalls) != 0 || len(mounter.mountCalls) != 1 {
				t.Fatal("revocation left a target attached, released its source twice, or remounted it")
			}
		})
	}
}

type juiceFSUnmountTestMounter struct {
	*reconcileTestMounter
	healthErr   error
	healthCalls int
	mountErr    error
	unmountErr  error
}

func (m *juiceFSUnmountTestMounter) Mount(ctx context.Context, plan node.MountPlan) error {
	// A cleanup-pending failure leaves the target attached even though Mount
	// reports failure. Record it so later revocation must actually detach it.
	if err := m.reconcileTestMounter.Mount(ctx, plan); err != nil {
		return err
	}
	return m.mountErr
}

func (m *juiceFSUnmountTestMounter) IsMounted(context.Context, node.MountPlan) (bool, error) {
	m.healthCalls++
	return false, m.healthErr
}

func (m *juiceFSUnmountTestMounter) Unmount(ctx context.Context, plan node.MountPlan) error {
	if m.unmountErr != nil {
		m.unmountCalls = append(m.unmountCalls, plan)
		return m.unmountErr
	}
	return m.reconcileTestMounter.Unmount(ctx, plan)
}

func newJuiceFSUnmountTestServer(t *testing.T, readOnly bool) (*Server, *fileMountStateStore, *reconcileTestKube, *juiceFSUnmountTestMounter, desiredMount) {
	t.Helper()
	server, store, kubeClient, baseMounter := newReconcileTestServer(t)
	server.cfg.JuiceFSSocket = "/unused/juicefs-csi.sock"
	desired := desiredMount{Request: testMountRequest(), ContainerID: "containerd://old-container"}
	desired.Request.DriverName = node.JuiceFSDriver
	desired.Request.ReadOnly = readOnly
	if err := store.Put(desired); err != nil {
		t.Fatal(err)
	}
	baseMounter.mounted[mounterKey(desired.ContainerID, desired.Request.TargetPath)] = true
	mounter := &juiceFSUnmountTestMounter{reconcileTestMounter: baseMounter}
	server.mounter = mounter
	return server, store, kubeClient, mounter, desired
}

func juiceFSUnmountRequest(request api.MountRequest) api.UnmountRequest {
	return api.UnmountRequest{
		APIVersion: request.APIVersion, DriverName: request.DriverName,
		Namespace: request.Namespace, PodName: request.PodName, PodUID: request.PodUID,
		PVName: request.PVName, TargetPath: request.TargetPath, ContainerName: request.ContainerName,
	}
}

func assertJuiceFSUnmountPlan(t *testing.T, plan node.MountPlan, desired desiredMount) {
	t.Helper()
	if plan.PV.Driver != node.JuiceFSDriver || plan.PV.Name != desired.Request.PVName ||
		plan.PodUID != desired.Request.PodUID || plan.ContainerName != desired.Request.ContainerName ||
		plan.ContainerID != desired.ContainerID || plan.SourceSubPath != desired.Request.SourceSubPath ||
		plan.TargetPath != desired.Request.TargetPath || plan.ReadOnly != desired.Request.ReadOnly {
		t.Fatalf("unmount plan = %+v, want persisted identity and access mode from %+v", plan, desired)
	}
}

func setJuiceFSGrantForTest(t *testing.T, pod *kube.Pod, request api.MountRequest) {
	t.Helper()
	grant, err := json.Marshal([]map[string]any{{
		"pv_name": request.PVName, "source_sub_path": request.SourceSubPath,
		"target_path": request.TargetPath, "container_name": request.ContainerName, "read_only": request.ReadOnly,
	}})
	if err != nil {
		t.Fatal(err)
	}
	pod.Metadata.Annotations = map[string]string{JuiceFSAllowedMountsAnnotation: string(grant)}
}

type juiceFSUnmountSecretTestKube struct {
	*reconcileTestKube
}

func (*juiceFSUnmountSecretTestKube) GetSecret(_ context.Context, namespace, name string) (*kube.Secret, error) {
	if namespace != "kube-system" || name != "drive-secret" {
		return nil, fmt.Errorf("unexpected secret %s/%s", namespace, name)
	}
	return &kube.Secret{Data: map[string][]byte{"name": []byte("drive")}}, nil
}
