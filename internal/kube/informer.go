package kube

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/tools/cache"
)

func (c *Client) RunPodInformer(ctx context.Context, nodeName string, synced func([]Pod), handle func(PodWatchEvent)) error {
	if c.podREST == nil {
		return fmt.Errorf("kubernetes pod REST client is not configured")
	}
	selector := fields.OneTermEqualSelector("spec.nodeName", nodeName).String()
	listWatch := cache.NewFilteredListWatchFromClient(
		c.podREST,
		"pods",
		metav1.NamespaceAll,
		func(options *metav1.ListOptions) {
			options.FieldSelector = selector
		},
	)
	informer := cache.NewSharedIndexInformer(listWatch, &corev1.Pod{}, 0, cache.Indexers{})
	if err := informer.SetTransform(trimPodForInformerCache); err != nil {
		return fmt.Errorf("configure pod informer cache transform: %w", err)
	}
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(object any) {
			if pod, ok := podFromInformerObject(object); ok {
				handle(PodWatchEvent{Type: "ADDED", Pod: podSnapshot(pod)})
			}
		},
		UpdateFunc: func(oldObject, newObject any) {
			oldPod, oldOK := podFromInformerObject(oldObject)
			newPod, newOK := podFromInformerObject(newObject)
			if !oldOK || !newOK || sameContainerState(oldPod, newPod) {
				return
			}
			handle(PodWatchEvent{Type: "MODIFIED", Pod: podSnapshot(newPod)})
		},
		DeleteFunc: func(object any) {
			if pod, ok := podFromInformerObject(object); ok {
				handle(PodWatchEvent{Type: "DELETED", Pod: podSnapshot(pod)})
			}
		},
	})
	if err != nil {
		return fmt.Errorf("register pod informer handler: %w", err)
	}

	go informer.Run(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("pod informer cache did not sync")
	}

	objects := informer.GetStore().List()
	pods := make([]Pod, 0, len(objects))
	for _, object := range objects {
		if pod, ok := podFromInformerObject(object); ok {
			pods = append(pods, podSnapshot(pod))
		}
	}
	synced(pods)
	<-ctx.Done()
	return nil
}

func trimPodForInformerCache(object any) (any, error) {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return object, nil
	}
	trimmed := &corev1.Pod{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			UID:             pod.UID,
			ResourceVersion: pod.ResourceVersion,
		},
		Spec: corev1.PodSpec{
			NodeName:           pod.Spec.NodeName,
			ServiceAccountName: pod.Spec.ServiceAccountName,
		},
		Status: corev1.PodStatus{Phase: pod.Status.Phase},
	}
	for _, container := range pod.Spec.Containers {
		cachedContainer := corev1.Container{Name: container.Name}
		for _, env := range container.Env {
			if env.Name == "SANDBOX_MAIN_CONTAINER" {
				cachedContainer.Env = append(cachedContainer.Env, corev1.EnvVar{Name: env.Name, Value: env.Value})
			}
		}
		trimmed.Spec.Containers = append(trimmed.Spec.Containers, cachedContainer)
	}
	for _, status := range pod.Status.ContainerStatuses {
		trimmed.Status.ContainerStatuses = append(trimmed.Status.ContainerStatuses, corev1.ContainerStatus{
			Name:        status.Name,
			ContainerID: status.ContainerID,
		})
	}
	return trimmed, nil
}

func podFromInformerObject(object any) (*corev1.Pod, bool) {
	switch value := object.(type) {
	case *corev1.Pod:
		return value, true
	case cache.DeletedFinalStateUnknown:
		pod, ok := value.Obj.(*corev1.Pod)
		return pod, ok
	case *cache.DeletedFinalStateUnknown:
		pod, ok := value.Obj.(*corev1.Pod)
		return pod, ok
	default:
		return nil, false
	}
}

func sameContainerState(left, right *corev1.Pod) bool {
	if left.Status.Phase != right.Status.Phase || len(left.Status.ContainerStatuses) != len(right.Status.ContainerStatuses) {
		return false
	}
	leftIDs := make(map[string]string, len(left.Status.ContainerStatuses))
	for _, status := range left.Status.ContainerStatuses {
		leftIDs[status.Name] = status.ContainerID
	}
	for _, status := range right.Status.ContainerStatuses {
		leftID, exists := leftIDs[status.Name]
		if !exists || leftID != status.ContainerID {
			return false
		}
	}
	return true
}

func podSnapshot(pod *corev1.Pod) Pod {
	snapshot := Pod{
		Metadata: ObjectMeta{
			Name:        pod.Name,
			Namespace:   pod.Namespace,
			UID:         string(pod.UID),
			Labels:      copyStringMap(pod.Labels),
			Annotations: copyStringMap(pod.Annotations),
		},
		Spec: PodSpec{
			ServiceAccountName: pod.Spec.ServiceAccountName,
			NodeName:           pod.Spec.NodeName,
		},
		Status: PodStatus{Phase: string(pod.Status.Phase)},
	}
	for _, owner := range pod.OwnerReferences {
		snapshot.Metadata.OwnerReferences = append(snapshot.Metadata.OwnerReferences, OwnerReference{
			APIVersion: owner.APIVersion,
			Kind:       owner.Kind,
			Name:       owner.Name,
			UID:        string(owner.UID),
		})
	}
	for _, container := range pod.Spec.Containers {
		converted := Container{Name: container.Name}
		for _, env := range container.Env {
			converted.Env = append(converted.Env, EnvVar{Name: env.Name, Value: env.Value})
		}
		snapshot.Spec.Containers = append(snapshot.Spec.Containers, converted)
	}
	for _, volume := range pod.Spec.Volumes {
		converted := Volume{Name: volume.Name}
		if volume.PersistentVolumeClaim != nil {
			converted.PersistentVolumeClaim = &PersistentVolumeClaim{
				ClaimName: volume.PersistentVolumeClaim.ClaimName,
				ReadOnly:  volume.PersistentVolumeClaim.ReadOnly,
			}
		}
		snapshot.Spec.Volumes = append(snapshot.Spec.Volumes, converted)
	}
	for _, status := range pod.Status.ContainerStatuses {
		snapshot.Status.ContainerStatuses = append(snapshot.Status.ContainerStatuses, ContainerStatus{
			Name:        status.Name,
			ContainerID: status.ContainerID,
			Ready:       status.Ready,
		})
	}
	return snapshot
}

func copyStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
