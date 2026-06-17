package wrapper

import (
	"errors"
	"fmt"
	"strings"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
)

var errBadRequest = errors.New("bad request")

func validateRequestShape(driverName string, request *api.MountRequest) error {
	if request.APIVersion != api.Version {
		return fmt.Errorf("%w: api_version must be %s", errBadRequest, api.Version)
	}
	if request.DriverName != driverName {
		return fmt.Errorf("%w: driver_name must be %s", errBadRequest, driverName)
	}
	if request.Namespace == "" {
		return fmt.Errorf("%w: namespace is required", errBadRequest)
	}
	if request.PodName == "" {
		return fmt.Errorf("%w: pod_name is required", errBadRequest)
	}
	if request.PodUID == "" {
		return fmt.Errorf("%w: pod_uid is required", errBadRequest)
	}
	if request.PVName == "" {
		return fmt.Errorf("%w: pv_name is required", errBadRequest)
	}
	if request.TargetPath == "" {
		return fmt.Errorf("%w: target_path is required", errBadRequest)
	}
	return nil
}

func authorizeToken(status *kube.TokenReviewStatus, namespace, expectedAudience string) error {
	if status == nil {
		return fmt.Errorf("token review returned empty status")
	}
	if !status.Authenticated {
		if status.Error != "" {
			return fmt.Errorf("token is not authenticated: %s", status.Error)
		}
		return fmt.Errorf("token is not authenticated")
	}
	if !strings.HasPrefix(status.User.Username, "system:serviceaccount:"+namespace+":") {
		return fmt.Errorf("token service account does not belong to namespace %s", namespace)
	}
	if expectedAudience != "" && !containsString(status.Audiences, expectedAudience) {
		return fmt.Errorf("token audience does not include %s", expectedAudience)
	}
	return nil
}

func authorizePod(tokenStatus *kube.TokenReviewStatus, pod *kube.Pod, request api.MountRequest) error {
	if pod.Metadata.Namespace != request.Namespace || pod.Metadata.Name != request.PodName {
		return fmt.Errorf("pod identity mismatch for %s/%s", request.Namespace, request.PodName)
	}
	if pod.Metadata.UID != request.PodUID {
		return fmt.Errorf("pod uid mismatch for %s/%s", request.Namespace, request.PodName)
	}
	expectedUser := "system:serviceaccount:" + request.Namespace + ":" + pod.Spec.ServiceAccountName
	if tokenStatus.User.Username != expectedUser {
		return fmt.Errorf("token service account %s does not match pod service account %s", tokenStatus.User.Username, expectedUser)
	}
	if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
		return fmt.Errorf("pod %s/%s is not running: phase=%s", request.Namespace, request.PodName, pod.Status.Phase)
	}
	return nil
}

func validatePVClaimRefForPod(pod *kube.Pod, pv *kube.PersistentVolume) error {
	if pv.Spec.ClaimRef == nil {
		return fmt.Errorf("pv %s has no claimRef", pv.Metadata.Name)
	}
	if pv.Spec.ClaimRef.Namespace != pod.Metadata.Namespace {
		return fmt.Errorf("pv %s claim namespace %s does not match pod namespace %s", pv.Metadata.Name, pv.Spec.ClaimRef.Namespace, pod.Metadata.Namespace)
	}
	if pv.Spec.ClaimRef.Name == "" {
		return fmt.Errorf("pv %s claimRef has empty name", pv.Metadata.Name)
	}
	return nil
}

func authorizePVForPod(driverName string, pod *kube.Pod, pv *kube.PersistentVolume, pvc *kube.PersistentVolumeClaimResource) error {
	if pv.Spec.CSI == nil {
		return fmt.Errorf("pv %s is not a CSI pv", pv.Metadata.Name)
	}
	if pv.Spec.CSI.Driver != driverName {
		return fmt.Errorf("pv %s uses driver %s, expected %s", pv.Metadata.Name, pv.Spec.CSI.Driver, driverName)
	}
	if err := validatePVClaimRefForPod(pod, pv); err != nil {
		return err
	}
	if pvc == nil {
		return fmt.Errorf("pvc %s/%s is required for pv %s", pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name, pv.Metadata.Name)
	}
	if pvc.Metadata.Namespace != pv.Spec.ClaimRef.Namespace || pvc.Metadata.Name != pv.Spec.ClaimRef.Name {
		return fmt.Errorf("pvc identity mismatch for pv %s claimRef %s/%s", pv.Metadata.Name, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
	}
	if pv.Spec.ClaimRef.UID != "" && pvc.Metadata.UID != "" && pvc.Metadata.UID != pv.Spec.ClaimRef.UID {
		return fmt.Errorf("pvc %s/%s uid %s does not match pv %s claim uid %s", pvc.Metadata.Namespace, pvc.Metadata.Name, pvc.Metadata.UID, pv.Metadata.Name, pv.Spec.ClaimRef.UID)
	}
	return nil
}

func selectContainerStatus(pod *kube.Pod, requestedName string) (kube.ContainerStatus, error) {
	if requestedName != "" {
		return containerStatusByName(pod, requestedName)
	}
	if name := sandboxMainContainerName(pod); name != "" {
		return containerStatusByName(pod, name)
	}
	if len(pod.Status.ContainerStatuses) != 1 {
		return kube.ContainerStatus{}, fmt.Errorf("container_name is required when pod has %d containers", len(pod.Status.ContainerStatuses))
	}
	status := pod.Status.ContainerStatuses[0]
	if status.ContainerID == "" {
		return kube.ContainerStatus{}, fmt.Errorf("container %s has empty container id", status.Name)
	}
	return status, nil
}

func containerStatusByName(pod *kube.Pod, name string) (kube.ContainerStatus, error) {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == name {
			if status.ContainerID == "" {
				return kube.ContainerStatus{}, fmt.Errorf("container %s has empty container id", name)
			}
			return status, nil
		}
	}
	return kube.ContainerStatus{}, fmt.Errorf("container %s was not found in pod status", name)
}

func sandboxMainContainerName(pod *kube.Pod) string {
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "SANDBOX_MAIN_CONTAINER" && strings.EqualFold(env.Value, "true") {
				return container.Name
			}
		}
	}
	return ""
}

func buildMountPlan(request api.MountRequest, pod *kube.Pod, pv *kube.PersistentVolume, containerID string) (node.MountPlan, error) {
	attrs := pv.Spec.CSI.VolumeAttributes
	server := firstNonEmpty(attrs["server"], attrs["mountOptions.server"])
	share := firstNonEmpty(attrs["share"], attrs["mountOptions.share"])
	subDir := firstNonEmpty(attrs["subDir"], attrs["subdir"])
	if server == "" || share == "" {
		return node.MountPlan{}, fmt.Errorf("pv %s is missing nfs server/share volume attributes", pv.Metadata.Name)
	}
	return node.MountPlan{
		PV: node.PersistentVolume{
			Name:         pv.Metadata.Name,
			VolumeHandle: pv.Spec.CSI.VolumeHandle,
			Server:       server,
			Share:        share,
			SubDir:       subDir,
			MountOptions: pv.Spec.MountOptions,
		},
		PodUID:        pod.Metadata.UID,
		ContainerName: request.ContainerName,
		ContainerID:   containerID,
		TargetPath:    request.TargetPath,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
