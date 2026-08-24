package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

func TestSameContainerStateOnlyChangesForPhaseOrContainerID(t *testing.T) {
	oldPod := &corev1.Pod{
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "main",
				ContainerID: "containerd://old",
				Ready:       true,
			}},
		},
	}
	readinessOnly := oldPod.DeepCopy()
	readinessOnly.Status.ContainerStatuses[0].Ready = false
	if !sameContainerState(oldPod, readinessOnly) {
		t.Fatalf("readiness-only update should not enqueue mount reconciliation")
	}

	restarted := oldPod.DeepCopy()
	restarted.Status.ContainerStatuses[0].ContainerID = "containerd://new"
	if sameContainerState(oldPod, restarted) {
		t.Fatalf("container id change was not detected")
	}

	terminal := oldPod.DeepCopy()
	terminal.Status.Phase = corev1.PodFailed
	if sameContainerState(oldPod, terminal) {
		t.Fatalf("pod phase change was not detected")
	}
}

func TestPodSnapshotCarriesReconciliationIdentity(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-a",
			Namespace: "sandbox-ns",
			UID:       types.UID("pod-uid"),
		},
		Spec: corev1.PodSpec{
			NodeName:           "node-a",
			ServiceAccountName: "sandbox-sa",
			Containers: []corev1.Container{{
				Name: "main",
				Env:  []corev1.EnvVar{{Name: "SANDBOX_MAIN_CONTAINER", Value: "true"}},
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "main",
				ContainerID: "containerd://container-a",
			}},
		},
	}

	snapshot := podSnapshot(pod)
	if snapshot.Metadata.UID != "pod-uid" || snapshot.Spec.NodeName != "node-a" {
		t.Fatalf("snapshot identity = %#v, want pod uid and node", snapshot)
	}
	if snapshot.Status.ContainerStatuses[0].ContainerID != "containerd://container-a" {
		t.Fatalf("snapshot container status = %#v", snapshot.Status.ContainerStatuses)
	}
	if name := sandboxMainContainerNameForTest(snapshot); name != "main" {
		t.Fatalf("sandbox main container = %q, want main", name)
	}
}

func TestPodFromInformerObjectHandlesDeleteTombstone(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}}
	got, ok := podFromInformerObject(cache.DeletedFinalStateUnknown{Key: "ns/pod-a", Obj: pod})
	if !ok || got.Name != "pod-a" {
		t.Fatalf("podFromInformerObject = %#v, %v", got, ok)
	}
}

func TestTrimPodForInformerCacheDropsHeavyUnusedFields(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "pod-a",
			Namespace:   "sandbox-ns",
			UID:         types.UID("pod-uid"),
			Annotations: map[string]string{"large": "unused"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{
				Name:    "main",
				Image:   "large-image-reference",
				Command: []string{"unused"},
				Env: []corev1.EnvVar{
					{Name: "SANDBOX_MAIN_CONTAINER", Value: "true"},
					{Name: "UNUSED", Value: "large"},
				},
			}},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://id", Image: "unused"}}},
	}

	object, err := trimPodForInformerCache(pod)
	if err != nil {
		t.Fatalf("trimPodForInformerCache returned error: %v", err)
	}
	trimmed := object.(*corev1.Pod)
	if trimmed.Annotations != nil || trimmed.Spec.Containers[0].Image != "" || trimmed.Spec.Containers[0].Command != nil {
		t.Fatalf("trimmed pod retained unused fields: %#v", trimmed)
	}
	if len(trimmed.Spec.Containers[0].Env) != 1 || trimmed.Spec.Containers[0].Env[0].Name != "SANDBOX_MAIN_CONTAINER" {
		t.Fatalf("trimmed pod env = %#v, want only main-container marker", trimmed.Spec.Containers[0].Env)
	}
}

func sandboxMainContainerNameForTest(pod Pod) string {
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "SANDBOX_MAIN_CONTAINER" && env.Value == "true" {
				return container.Name
			}
		}
	}
	return ""
}
