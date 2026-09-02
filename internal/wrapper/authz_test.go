package wrapper

import (
	"testing"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
)

func TestAuthorizePVForPodAnnotationPolicy(t *testing.T) {
	pod := &kube.Pod{
		Metadata: kube.ObjectMeta{Name: "sandbox-pod", Namespace: "sandbox-ns", UID: "pod-uid"},
		Spec:     kube.PodSpec{ServiceAccountName: "sandbox-sa"},
	}
	tests := []struct {
		name        string
		annotations map[string]string
		claimNS     string
		wantErr     bool
	}{
		{name: "annotations absent allow bound PV from another namespace", claimNS: "other-ns"},
		{name: "namespace match", annotations: map[string]string{allowNamespaceAnnotation: "other-ns, sandbox-ns"}},
		{name: "namespace mismatch", annotations: map[string]string{allowNamespaceAnnotation: "other-ns"}, wantErr: true},
		{name: "service account match", annotations: map[string]string{allowServiceAccountAnnotation: "default, sandbox-sa"}},
		{name: "service account mismatch", annotations: map[string]string{allowServiceAccountAnnotation: "default"}, wantErr: true},
		{name: "both match", annotations: map[string]string{allowNamespaceAnnotation: "sandbox-ns", allowServiceAccountAnnotation: "sandbox-sa"}},
		{name: "one of both mismatches", annotations: map[string]string{allowNamespaceAnnotation: "sandbox-ns", allowServiceAccountAnnotation: "other-sa"}, wantErr: true},
		{name: "empty value", annotations: map[string]string{allowNamespaceAnnotation: ""}, wantErr: true},
		{name: "empty item", annotations: map[string]string{allowNamespaceAnnotation: "sandbox-ns,"}, wantErr: true},
		{name: "wildcard", annotations: map[string]string{allowNamespaceAnnotation: "*"}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pv := &kube.PersistentVolume{
				Metadata: kube.ObjectMeta{Name: "pv-a", Annotations: test.annotations},
				Spec: kube.PersistentVolumeSpec{
					ClaimRef: &kube.ObjectReference{Namespace: test.claimNS, Name: "pvc-a", UID: "pvc-uid"},
					CSI:      &kube.CSIPersistentVolumeSource{Driver: "csi.nfs.zhida"},
				},
			}
			err := authorizePVForPod("csi.nfs.zhida", pod, pv)
			if test.wantErr && err == nil {
				t.Fatal("authorizePVForPod succeeded, want error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("authorizePVForPod returned error: %v", err)
			}
		})
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
	if err := authorizePVForPod("csi.nfs.zhida", pod, pv); err == nil {
		t.Fatalf("authorizePVForPod succeeded, want error")
	}
}

func TestAuthorizePVForPodAllowsPVWithoutClaimRef(t *testing.T) {
	pod := &kube.Pod{
		Metadata: kube.ObjectMeta{Namespace: "sandbox-ns"},
		Spec:     kube.PodSpec{ServiceAccountName: "sandbox-sa"},
	}
	pv := &kube.PersistentVolume{
		Metadata: kube.ObjectMeta{Name: "shared-pv"},
		Spec: kube.PersistentVolumeSpec{
			CSI: &kube.CSIPersistentVolumeSource{Driver: "csi.nfs.zhida"},
		},
	}
	if err := authorizePVForPod("csi.nfs.zhida", pod, pv); err != nil {
		t.Fatalf("authorizePVForPod returned error for PV without claimRef: %v", err)
	}
}

func TestAuthorizePodRequiresBoundTokenIdentity(t *testing.T) {
	request := api.MountRequest{Namespace: "sandbox-ns", PodName: "pod-a", PodUID: "pod-uid"}
	pod := &kube.Pod{
		Metadata: kube.ObjectMeta{Name: request.PodName, Namespace: request.Namespace, UID: request.PodUID},
		Spec:     kube.PodSpec{ServiceAccountName: "sandbox-sa"},
		Status:   kube.PodStatus{Phase: "Running"},
	}
	valid := &kube.TokenReviewStatus{User: kube.TokenReviewUser{
		Username: "system:serviceaccount:sandbox-ns:sandbox-sa",
		Extra: map[string][]string{
			tokenPodNameExtraKey: {request.PodName},
			tokenPodUIDExtraKey:  {request.PodUID},
		},
	}}
	if err := authorizePod(valid, pod, request); err != nil {
		t.Fatalf("authorizePod returned error: %v", err)
	}

	tests := []struct {
		name  string
		extra map[string][]string
	}{
		{name: "missing", extra: nil},
		{name: "different pod", extra: map[string][]string{tokenPodNameExtraKey: {"pod-b"}, tokenPodUIDExtraKey: {"other-uid"}}},
		{name: "multiple uid values", extra: map[string][]string{tokenPodNameExtraKey: {request.PodName}, tokenPodUIDExtraKey: {request.PodUID, "other-uid"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := *valid
			status.User = valid.User
			status.User.Extra = test.extra
			if err := authorizePod(&status, pod, request); err == nil {
				t.Fatal("authorizePod succeeded, want error")
			}
		})
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
					"subDir": "/tenants//alice",
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
	if plan.PV.SubDir != "tenants/alice" {
		t.Fatalf("normalized PV subDir = %q, want tenants/alice", plan.PV.SubDir)
	}
}

func TestBuildMountPlanRejectsUnsafePVSubDir(t *testing.T) {
	pod := &kube.Pod{Metadata: kube.ObjectMeta{UID: "pod-uid"}}
	pv := &kube.PersistentVolume{
		Metadata: kube.ObjectMeta{Name: "pv-a"},
		Spec: kube.PersistentVolumeSpec{CSI: &kube.CSIPersistentVolumeSource{
			VolumeAttributes: map[string]string{
				"server": "nfs.example.internal",
				"share":  "/exports/workloads",
				"subDir": "tenants/../escape",
			},
		}},
	}
	if _, err := buildMountPlan(api.MountRequest{PVName: "pv-a"}, pod, pv, "containerd://container-a"); err == nil {
		t.Fatal("buildMountPlan succeeded, want unsafe subDir error")
	}
}
