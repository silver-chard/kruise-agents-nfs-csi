package wrapper

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
)

var errDesiredMountStale = errors.New("desired mount is stale")

const podWatchQueueSize = 1024
const reconcileRetryQueueSize = 4096
const reconcileRetryWorkers = 4

var reconcileRetryDelays = [...]time.Duration{
	200 * time.Millisecond,
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	5 * time.Second,
}

type reconcileRetry struct {
	key     desiredMountKey
	attempt int
}

func (s *Server) RunReconciler(ctx context.Context) {
	if s.cfg.NodeName == "" {
		s.logger.Printf("warning: desired mount reconciliation is disabled because wrapper node name is empty")
		return
	}

	events := make(chan kube.PodWatchEvent, podWatchQueueSize)
	retries := make(chan reconcileRetry, reconcileRetryQueueSize)
	for range reconcileRetryWorkers {
		go s.processReconcileRetries(ctx, retries)
	}
	go s.processPodWatchEvents(ctx, events, retries)

	err := s.kube.RunPodInformer(
		ctx,
		s.cfg.NodeName,
		func(pods []kube.Pod) {
			s.reconcilePodSnapshot(ctx, pods, retries)
		},
		func(event kube.PodWatchEvent) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		},
	)
	if err != nil && ctx.Err() == nil {
		s.logger.Printf("warning: pod informer on node %s stopped: %v", s.cfg.NodeName, err)
	}
}

func (s *Server) processPodWatchEvents(ctx context.Context, events <-chan kube.PodWatchEvent, retries chan<- reconcileRetry) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-events:
			s.reconcilePodEvent(ctx, event, retries)
		}
	}
}

func (s *Server) reconcilePodSnapshot(ctx context.Context, pods []kube.Pod, retries chan<- reconcileRetry) {
	podsByUID := make(map[string]*kube.Pod, len(pods))
	for i := range pods {
		pod := &pods[i]
		podsByUID[pod.Metadata.Namespace+"\x00"+pod.Metadata.Name+"\x00"+pod.Metadata.UID] = pod
	}
	for _, desired := range s.state.List() {
		pod := podsByUID[desired.Request.Namespace+"\x00"+desired.Request.PodName+"\x00"+desired.Request.PodUID]
		if pod == nil {
			requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
			livePod, err := s.kube.GetPod(requestCtx, desired.Request.Namespace, desired.Request.PodName)
			if err == nil && livePod.Spec.NodeName != "" && livePod.Spec.NodeName != s.cfg.NodeName {
				err = fmt.Errorf("%w: pod %s/%s moved away from node %s", errDesiredMountStale, desired.Request.Namespace, desired.Request.PodName, s.cfg.NodeName)
			} else if err == nil {
				err = s.reconcileMountForPod(requestCtx, desired, livePod)
			} else if kube.IsNotFound(err) {
				err = fmt.Errorf("%w: pod %s/%s no longer exists", errDesiredMountStale, desired.Request.Namespace, desired.Request.PodName)
			} else {
				err = fmt.Errorf("get pod %s/%s missing from node snapshot: %w", desired.Request.Namespace, desired.Request.PodName, err)
			}
			cancel()
			s.handleReconcileResult(desired, err)
			s.scheduleReconcileRetry(ctx, retries, desired, err)
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		err := s.reconcileMountForPod(requestCtx, desired, pod)
		cancel()
		s.handleReconcileResult(desired, err)
		s.scheduleReconcileRetry(ctx, retries, desired, err)
	}
}

func (s *Server) reconcilePodEvent(ctx context.Context, event kube.PodWatchEvent, retries ...chan<- reconcileRetry) {
	podKey := desiredPodKey{
		Namespace: event.Pod.Metadata.Namespace,
		Name:      event.Pod.Metadata.Name,
		UID:       event.Pod.Metadata.UID,
	}
	for _, desired := range s.state.ListForPod(podKey) {
		if event.Type == "DELETED" {
			s.handleReconcileResult(desired, fmt.Errorf("%w: pod %s/%s was deleted", errDesiredMountStale, desired.Request.Namespace, desired.Request.PodName))
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		err := s.reconcileMountForPod(requestCtx, desired, &event.Pod)
		cancel()
		s.handleReconcileResult(desired, err)
		if len(retries) != 0 {
			s.scheduleReconcileRetry(ctx, retries[0], desired, err)
		}
	}
}

func (s *Server) scheduleReconcileRetry(ctx context.Context, retries chan<- reconcileRetry, desired desiredMount, err error) {
	if err == nil || errors.Is(err, errDesiredMountStale) || ctx.Err() != nil {
		return
	}
	key := desired.key()
	if _, loaded := s.retrying.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	select {
	case retries <- reconcileRetry{key: key}:
	case <-ctx.Done():
		s.retrying.Delete(key)
	default:
		s.retrying.Delete(key)
		s.logger.Printf("warning: desired mount retry queue is full for pod uid %s container %s at %s", key.PodUID, key.ContainerName, key.TargetPath)
	}
}

func (s *Server) processReconcileRetries(ctx context.Context, retries chan reconcileRetry) {
	for {
		select {
		case <-ctx.Done():
			return
		case retry := <-retries:
			if !waitForRetry(ctx, reconcileRetryDelays[retry.attempt]) {
				return
			}
			desired, exists := s.state.Get(retry.key)
			if !exists {
				s.retrying.Delete(retry.key)
				continue
			}
			requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
			err := s.reconcileMount(requestCtx, desired)
			cancel()
			s.handleReconcileResult(desired, err)
			if err == nil || errors.Is(err, errDesiredMountStale) || retry.attempt+1 >= len(reconcileRetryDelays) {
				s.retrying.Delete(retry.key)
				continue
			}
			retry.attempt++
			select {
			case retries <- retry:
			case <-ctx.Done():
				return
			default:
				s.retrying.Delete(retry.key)
				s.logger.Printf("warning: desired mount retry queue is full for pod uid %s container %s at %s", retry.key.PodUID, retry.key.ContainerName, retry.key.TargetPath)
			}
		}
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Server) reconcileAll(ctx context.Context) {
	type podResult struct {
		pod *kube.Pod
		err error
	}
	pods := make(map[string]podResult)
	for _, desired := range s.state.List() {
		if err := ctx.Err(); err != nil {
			return
		}

		podKey := desired.Request.Namespace + "\x00" + desired.Request.PodName + "\x00" + desired.Request.PodUID
		podState, cached := pods[podKey]
		if !cached {
			requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
			pod, err := s.kube.GetPod(requestCtx, desired.Request.Namespace, desired.Request.PodName)
			cancel()
			if err != nil {
				if kube.IsNotFound(err) {
					err = fmt.Errorf("%w: pod %s/%s no longer exists", errDesiredMountStale, desired.Request.Namespace, desired.Request.PodName)
				} else {
					err = fmt.Errorf("get pod %s/%s: %w", desired.Request.Namespace, desired.Request.PodName, err)
				}
			}
			podState = podResult{pod: pod, err: err}
			pods[podKey] = podState
		}

		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		err := podState.err
		if err == nil {
			err = s.reconcileMountForPod(requestCtx, desired, podState.pod)
		}
		cancel()
		s.handleReconcileResult(desired, err)
	}
}

func (s *Server) handleReconcileResult(desired desiredMount, err error) {
	key := desired.key()
	if err == nil {
		s.reconcileErrors.Delete(key)
		return
	}
	if errors.Is(err, errDesiredMountStale) {
		lock := s.targetLock(key)
		lock.Lock()
		current, exists := s.state.Get(key)
		var deleteErr error
		if exists && current == desired {
			deleteErr = s.state.Delete(key)
		}
		lock.Unlock()
		if deleteErr == nil && exists && current == desired {
			s.reconcileErrors.Delete(key)
			s.logger.Printf("removed stale desired mount for pod %s/%s container %s at %s: %v", desired.Request.Namespace, desired.Request.PodName, desired.Request.ContainerName, desired.Request.TargetPath, err)
			return
		}
		if !exists || current != desired {
			s.reconcileErrors.Delete(key)
			return
		}
		err = errors.Join(err, fmt.Errorf("delete stale desired mount: %w", deleteErr))
	}
	s.logReconcileError(key, desired, err)
}

func (s *Server) reconcileMount(ctx context.Context, snapshot desiredMount) error {
	pod, err := s.kube.GetPod(ctx, snapshot.Request.Namespace, snapshot.Request.PodName)
	if err != nil {
		if kube.IsNotFound(err) {
			return fmt.Errorf("%w: pod %s/%s no longer exists", errDesiredMountStale, snapshot.Request.Namespace, snapshot.Request.PodName)
		}
		return fmt.Errorf("get pod %s/%s: %w", snapshot.Request.Namespace, snapshot.Request.PodName, err)
	}
	return s.reconcileMountForPod(ctx, snapshot, pod)
}

func (s *Server) reconcileMountForPod(ctx context.Context, snapshot desiredMount, pod *kube.Pod) error {
	key := snapshot.key()
	lock := s.targetLock(key)
	lock.Lock()
	defer lock.Unlock()

	current, exists := s.state.Get(key)
	if !exists || current != snapshot {
		return nil
	}
	request, err := s.normalizeMountRequest(current.Request, true)
	if err != nil {
		return err
	}
	if err := validatePodIdentity(pod, request); err != nil {
		return fmt.Errorf("%w: %v", errDesiredMountStale, err)
	}
	if err := validatePodNode(pod, s.cfg.NodeName); err != nil {
		return fmt.Errorf("%w: %v", errDesiredMountStale, err)
	}
	containerStatus, err := selectContainerStatus(pod, request.ContainerName)
	if err != nil {
		return fmt.Errorf("container status for desired mount: %w", err)
	}
	request.ContainerName = containerStatus.Name
	current.Request = request
	if current.ContainerID != "" && current.ContainerID == containerStatus.ContainerID {
		return nil
	}

	request, plan, err := s.mountPlanForPod(ctx, request, pod)
	if err != nil {
		return err
	}
	current.Request = request
	if node.IsNFSExportRoot(plan) &&
		(!current.ExportRootAuthorized || !s.exportRoot.authorizesFingerprint(current.ExportRootKeyFingerprint)) {
		return fmt.Errorf("%w: desired mount has no authorization from the current NFS export root key", errDesiredMountStale)
	}
	if !node.IsNFSExportRoot(plan) {
		current.ExportRootAuthorized = false
		current.ExportRootKeyFingerprint = ""
	}

	if current.ContainerID == "" || current.ContainerID == plan.ContainerID {
		mounted, err := s.mounter.IsMounted(ctx, plan)
		if err != nil {
			return err
		}
		if mounted {
			if current.ContainerID != plan.ContainerID {
				current.ContainerID = plan.ContainerID
				if err := s.state.Put(current); err != nil {
					return fmt.Errorf("record reconciled container: %w", err)
				}
			}
			return nil
		}
	}

	if err := s.mounter.Mount(ctx, plan); err != nil {
		return err
	}
	previousContainerID := current.ContainerID
	current.ContainerID = plan.ContainerID
	if err := s.state.Put(current); err != nil {
		return fmt.Errorf("record reconciled container: %w", err)
	}
	s.logger.Printf("reconciled pv %s for restarted pod %s/%s container %s at %s (container %s -> %s)", request.PVName, request.Namespace, request.PodName, request.ContainerName, request.TargetPath, safeContainerID(previousContainerID), safeContainerID(plan.ContainerID))
	return nil
}

func (s *Server) logReconcileError(key desiredMountKey, desired desiredMount, err error) {
	message := err.Error()
	if previous, loaded := s.reconcileErrors.LoadOrStore(key, message); loaded && previous == message {
		return
	}
	s.reconcileErrors.Store(key, message)
	s.logger.Printf("warning: reconcile desired mount for pod %s/%s container %s at %s: %v", desired.Request.Namespace, desired.Request.PodName, desired.Request.ContainerName, desired.Request.TargetPath, err)
}

func safeContainerID(containerID string) string {
	if containerID == "" {
		return "pending"
	}
	if len(containerID) <= 20 {
		return containerID
	}
	return containerID[:20] + "..."
}
