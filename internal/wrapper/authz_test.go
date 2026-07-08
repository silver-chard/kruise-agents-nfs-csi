package wrapper

import (
	"testing"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
)

func TestAuthorizePVForPodAllowsNamespaceScopedDynamicMount(t *testing.T) {
	pod := &kube.Pod{Metadata: kube.ObjectMeta{Name: "sandbox-pod", Namespace: "sandbox-ns", UID: "pod-uid"}}
	pv := &kube.PersistentVolume{
		Metadata: kube.ObjectMeta{Name: "pv-a"},
		Spec: kube.PersistentVolumeSpec{
			ClaimRef: &kube.ObjectReference{Namespace: "sandbox-ns", Name: "pvc-a", UID: "pvc-uid"},
			CSI:      &kube.CSIPersistentVolumeSource{Driver: "csi.nfs.zhida"},
		},
	}
	pvc := &kube.PersistentVolumeClaimResource{
		Metadata: kube.ObjectMeta{Name: "pvc-a", Namespace: "sandbox-ns", UID: "pvc-uid"},
	}

	if err := authorizePVForPod("csi.nfs.zhida", pod, pv, pvc); err != nil {
		t.Fatalf("authorizePVForPod returned error: %v", err)
	}
}

func TestAuthorizePVForPodRejectsDifferentClaimNamespace(t *testing.T) {
	pod := &kube.Pod{Metadata: kube.ObjectMeta{Name: "sandbox-pod", Namespace: "sandbox-ns", UID: "pod-uid"}}
	pv := &kube.PersistentVolume{
		Metadata: kube.ObjectMeta{Name: "pv-a"},
		Spec: kube.PersistentVolumeSpec{
			ClaimRef: &kube.ObjectReference{Namespace: "other-ns", Name: "pvc-a"},
			CSI:      &kube.CSIPersistentVolumeSource{Driver: "csi.nfs.zhida"},
		},
	}

	if err := authorizePVForPod("csi.nfs.zhida", pod, pv, nil); err == nil {
		t.Fatalf("authorizePVForPod succeeded, want error")
	}
}

func TestAuthorizePVForPodRejectsDifferentDriver(t *testing.T) {
	pod := &kube.Pod{Metadata: kube.ObjectMeta{Name: "sandbox-pod", Namespace: "sandbox-ns", UID: "pod-uid"}}
	pv := &kube.PersistentVolume{
		Metadata: kube.ObjectMeta{Name: "pv-a"},
		Spec: kube.PersistentVolumeSpec{
			ClaimRef: &kube.ObjectReference{Namespace: "sandbox-ns", Name: "pvc-a", UID: "pvc-uid"},
			CSI:      &kube.CSIPersistentVolumeSource{Driver: "other.csi.driver"},
		},
	}
	pvc := &kube.PersistentVolumeClaimResource{
		Metadata: kube.ObjectMeta{Name: "pvc-a", Namespace: "sandbox-ns", UID: "pvc-uid"},
	}

	if err := authorizePVForPod("csi.nfs.zhida", pod, pv, pvc); err == nil {
		t.Fatalf("authorizePVForPod succeeded, want error")
	}
}

func TestSelectContainerStatusUsesSandboxMainContainerEnv(t *testing.T) {
	pod := &kube.Pod{
		Spec: kube.PodSpec{
			Containers: []kube.Container{
				{Name: "agent-runtime"},
				{Name: "sandbox-workspace", Env: []kube.EnvVar{{Name: "SANDBOX_MAIN_CONTAINER", Value: "true"}}},
			},
		},
		Status: kube.PodStatus{
			ContainerStatuses: []kube.ContainerStatus{
				{Name: "agent-runtime", ContainerID: "containerd://sidecar"},
				{Name: "sandbox-workspace", ContainerID: "containerd://main"},
			},
		},
	}

	status, err := selectContainerStatus(pod, "")
	if err != nil {
		t.Fatalf("selectContainerStatus returned error: %v", err)
	}
	if status.Name != "sandbox-workspace" {
		t.Fatalf("selected container = %q, want sandbox-workspace", status.Name)
	}
}

func TestBuildMountPlanCarriesSourceSubPath(t *testing.T) {
	pod := &kube.Pod{Metadata: kube.ObjectMeta{Name: "sandbox-pod", Namespace: "sandbox-ns", UID: "pod-uid"}}
	pv := &kube.PersistentVolume{
		Metadata: kube.ObjectMeta{Name: "pv-a"},
		Spec: kube.PersistentVolumeSpec{
			CSI: &kube.CSIPersistentVolumeSource{
				VolumeHandle: "handle-a",
				VolumeAttributes: map[string]string{
					"server": "nfs.example.internal",
					"share":  "/exports/workloads",
				},
			},
		},
	}
	request := api.MountRequest{
		PVName:        "pv-a",
		SourceSubPath: "users/alice",
		TargetPath:    "/workspace/data",
		ContainerName: "main",
	}

	plan, err := buildMountPlan(request, pod, pv, "containerd://container-a")
	if err != nil {
		t.Fatalf("buildMountPlan returned error: %v", err)
	}
	if plan.SourceSubPath != "users/alice" {
		t.Fatalf("source sub path = %q, want users/alice", plan.SourceSubPath)
	}
}
