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

const (
	allowNamespaceAnnotation      = "kary.dev/allow-namespace"
	allowServiceAccountAnnotation = "kary.dev/allow-serviceaccount"
	tokenPodNameExtraKey          = "authentication.kubernetes.io/pod-name"
	tokenPodUIDExtraKey           = "authentication.kubernetes.io/pod-uid"
)

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
	if err := validatePodIdentity(pod, request); err != nil {
		return err
	}
	expectedUser := "system:serviceaccount:" + request.Namespace + ":" + pod.Spec.ServiceAccountName
	if tokenStatus.User.Username != expectedUser {
		return fmt.Errorf("token service account %s does not match pod service account %s", tokenStatus.User.Username, expectedUser)
	}
	if err := requireTokenPodExtra(tokenStatus.User.Extra, tokenPodNameExtraKey, request.PodName); err != nil {
		return err
	}
	if err := requireTokenPodExtra(tokenStatus.User.Extra, tokenPodUIDExtraKey, request.PodUID); err != nil {
		return err
	}
	return nil
}

func requireTokenPodExtra(extra map[string][]string, key, expected string) error {
	values := extra[key]
	if len(values) != 1 || values[0] == "" {
		return fmt.Errorf("token must contain exactly one non-empty %s value", key)
	}
	if values[0] != expected {
		return fmt.Errorf("token %s does not match target pod", key)
	}
	return nil
}

func validatePodIdentity(pod *kube.Pod, request api.MountRequest) error {
	if pod.Metadata.Namespace != request.Namespace || pod.Metadata.Name != request.PodName {
		return fmt.Errorf("pod identity mismatch for %s/%s", request.Namespace, request.PodName)
	}
	if pod.Metadata.UID != request.PodUID {
		return fmt.Errorf("pod uid mismatch for %s/%s", request.Namespace, request.PodName)
	}
	if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
		return fmt.Errorf("pod %s/%s is not running: phase=%s", request.Namespace, request.PodName, pod.Status.Phase)
	}
	return nil
}

func validatePodNode(pod *kube.Pod, nodeName string) error {
	if nodeName != "" && pod.Spec.NodeName != nodeName {
		return fmt.Errorf("pod %s/%s is scheduled on node %s, wrapper serves node %s", pod.Metadata.Namespace, pod.Metadata.Name, pod.Spec.NodeName, nodeName)
	}
	return nil
}

func authorizePVForPod(driverName string, pod *kube.Pod, pv *kube.PersistentVolume) error {
	if pv.Spec.CSI == nil {
		return fmt.Errorf("pv %s is not a CSI pv", pv.Metadata.Name)
	}
	if pv.Spec.CSI.Driver != driverName {
		return fmt.Errorf("pv %s uses driver %s, expected %s", pv.Metadata.Name, pv.Spec.CSI.Driver, driverName)
	}
	if err := authorizePVAnnotation(pv, allowNamespaceAnnotation, pod.Metadata.Namespace); err != nil {
		return err
	}
	if err := authorizePVAnnotation(pv, allowServiceAccountAnnotation, pod.Spec.ServiceAccountName); err != nil {
		return err
	}
	return nil
}

func authorizePVAnnotation(pv *kube.PersistentVolume, key, identity string) error {
	raw, configured := pv.Metadata.Annotations[key]
	if !configured {
		return nil
	}
	if raw == "" {
		return fmt.Errorf("pv %s annotation %s must contain a comma-separated allowlist", pv.Metadata.Name, key)
	}

	allowed := false
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" || entry == "*" {
			return fmt.Errorf("pv %s annotation %s contains invalid entry", pv.Metadata.Name, key)
		}
		if entry == identity {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("pv %s annotation %s does not allow the target pod", pv.Metadata.Name, key)
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
	subDir, err := node.NormalizeNFSSubDir(firstNonEmpty(attrs["subDir"], attrs["subdir"]))
	if err != nil {
		return node.MountPlan{}, fmt.Errorf("pv %s has invalid nfs subDir: %w", pv.Metadata.Name, err)
	}
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
		SourceSubPath: request.SourceSubPath,
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
