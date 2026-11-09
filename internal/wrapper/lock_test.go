package wrapper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
)

func TestMountAndUnmountLockWaitAreNodeErrors(t *testing.T) {
	for _, operation := range []string{"mount", "unmount"} {
		t.Run(operation, func(t *testing.T) {
			server, _, _, mounter := newReconcileTestServer(t)
			request := testMountRequest()
			unlock, err := server.targetLocks.Lock(context.Background(), desiredMountKeyFor(request))
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if operation == "mount" {
				_, err = server.mount(ctx, "valid-token", "", request)
			} else {
				_, err = server.unmount(ctx, "valid-token", api.UnmountRequest{
					APIVersion: request.APIVersion, DriverName: request.DriverName,
					Namespace: request.Namespace, PodName: request.PodName, PodUID: request.PodUID,
					PVName: request.PVName, TargetPath: request.TargetPath, ContainerName: request.ContainerName,
				})
			}
			if !errors.Is(err, context.DeadlineExceeded) || requestErrorStatus(err) != http.StatusInternalServerError {
				t.Fatalf("lock timeout = %v, HTTP %d, want node operation timeout", err, requestErrorStatus(err))
			}
			if len(mounter.mountCalls) != 0 || len(mounter.unmountCalls) != 0 {
				t.Fatal("cancelled waiter performed a node operation")
			}
		})
	}
}

func TestReconcileLockWaitHonorsContext(t *testing.T) {
	server, store, kubeClient, mounter := newReconcileTestServer(t)
	desired := desiredMount{Request: testMountRequest()}
	if err := store.Put(desired); err != nil {
		t.Fatal(err)
	}
	unlock, err := server.targetLocks.Lock(context.Background(), desired.key())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := server.reconcileMountForPod(ctx, desired, kubeClient.pod); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconcile error = %v, want deadline", err)
	}
	if len(mounter.mountCalls) != 0 {
		t.Fatal("cancelled lock waiter started a mount")
	}
}

func TestStaleCleanupStopsWaitingOnShutdown(t *testing.T) {
	server, store, _, _ := newReconcileTestServer(t)
	desired := desiredMount{Request: testMountRequest()}
	if err := store.Put(desired); err != nil {
		t.Fatal(err)
	}
	unlock, err := server.targetLocks.Lock(context.Background(), desired.key())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.handleReconcileResult(ctx, desired, fmt.Errorf("%w: deleted", errDesiredMountStale))
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stale cleanup ignored shutdown while waiting for target")
	}
	if _, exists := store.Get(desired.key()); !exists {
		t.Fatal("cancelled cleanup changed desired state")
	}
}
