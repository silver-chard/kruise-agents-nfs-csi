package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/config"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/security"
)

type Kubernetes interface {
	ReviewToken(ctx context.Context, token string, audiences []string) (*kube.TokenReviewStatus, error)
	GetPod(ctx context.Context, namespace, name string) (*kube.Pod, error)
	RunPodInformer(ctx context.Context, nodeName string, synced func([]kube.Pod), handle func(kube.PodWatchEvent)) error
	GetPersistentVolume(ctx context.Context, name string) (*kube.PersistentVolume, error)
	GetPersistentVolumeClaim(ctx context.Context, namespace, name string) (*kube.PersistentVolumeClaimResource, error)
}

type Server struct {
	cfg             config.WrapperConfig
	kube            Kubernetes
	mounter         node.Mounter
	state           mountStateStore
	logger          *log.Logger
	mux             *http.ServeMux
	targetLocks     [targetLockStripes]sync.Mutex
	reconcileErrors sync.Map
	retrying        sync.Map
}

const targetLockStripes = 128

var errNodeOperation = errors.New("node operation failed")

func NewServer(cfg config.WrapperConfig, kubeClient Kubernetes, mounter node.Mounter, logger *log.Logger) (*Server, error) {
	state, err := newFileMountStateStore(cfg.MountStateDir)
	if err != nil {
		return nil, err
	}
	return newServerWithState(cfg, kubeClient, mounter, state, logger), nil
}

func newServerWithState(cfg config.WrapperConfig, kubeClient Kubernetes, mounter node.Mounter, state mountStateStore, logger *log.Logger) *Server {
	server := &Server{
		cfg:     cfg,
		kube:    kubeClient,
		mounter: mounter,
		state:   state,
		logger:  logger,
		mux:     http.NewServeMux(),
	}
	server.mux.HandleFunc("/healthz", server.handleHealthz)
	server.mux.HandleFunc("/v1/mount", server.handleMount)
	server.mux.HandleFunc("/v1/unmount", server.handleUnmount)
	return server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeData(w, http.StatusOK, map[string]string{"status": "ok", "driver_name": s.cfg.DriverName})
}

func (s *Server) handleMount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	token, err := bearerToken(r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var request api.MountRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json request: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	result, err := s.mount(ctx, token, request)
	if err != nil {
		writeError(w, requestErrorStatus(err), err.Error())
		return
	}
	writeData(w, http.StatusOK, result)
}

func (s *Server) handleUnmount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	token, err := bearerToken(r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var request api.UnmountRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json request: "+err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	result, err := s.unmount(ctx, token, request)
	if err != nil {
		writeError(w, requestErrorStatus(err), err.Error())
		return
	}
	writeData(w, http.StatusOK, result)
}

func (s *Server) mount(ctx context.Context, token string, request api.MountRequest) (*api.MountResult, error) {
	request, plan, err := s.authorizedMountPlan(ctx, token, request, true)
	if err != nil {
		return nil, err
	}
	key := desiredMountKeyFor(request)
	lock := s.targetLock(key)
	lock.Lock()
	defer lock.Unlock()

	previous, existed := s.state.Get(key)
	desired := desiredMount{Request: request}
	if existed && sameMountIntent(previous, desired) && (previous.ContainerID == "" || previous.ContainerID == plan.ContainerID) {
		mounted, err := s.mounter.IsMounted(ctx, plan)
		if err != nil {
			return nil, errors.Join(errNodeOperation, err)
		}
		if mounted {
			return mountResult(s.cfg.DriverName, request), nil
		}
	}

	if err := s.state.Put(desired); err != nil {
		return nil, fmt.Errorf("persist desired mount: %w", err)
	}
	if err := s.mounter.Mount(ctx, plan); err != nil {
		return nil, errors.Join(errNodeOperation, err, s.restoreDesiredMount(key, previous, existed))
	}
	desired.ContainerID = plan.ContainerID
	if err := s.state.Put(desired); err != nil {
		s.logger.Printf("warning: record mounted container for pod %s/%s container %s at %s: %v", request.Namespace, request.PodName, request.ContainerName, request.TargetPath, err)
	}

	s.logger.Printf("mounted pv %s for pod %s/%s container %s at %s", request.PVName, request.Namespace, request.PodName, request.ContainerName, request.TargetPath)
	return mountResult(s.cfg.DriverName, request), nil
}

func mountResult(driverName string, request api.MountRequest) *api.MountResult {
	return &api.MountResult{
		Mounted:       true,
		DriverName:    driverName,
		PVName:        request.PVName,
		SourceSubPath: request.SourceSubPath,
		TargetPath:    request.TargetPath,
		ContainerName: request.ContainerName,
	}
}

func (s *Server) unmount(ctx context.Context, token string, request api.UnmountRequest) (*api.UnmountResult, error) {
	mountRequest := api.MountRequest{
		APIVersion:    request.APIVersion,
		DriverName:    request.DriverName,
		Namespace:     request.Namespace,
		PodName:       request.PodName,
		PodUID:        request.PodUID,
		PVName:        request.PVName,
		TargetPath:    request.TargetPath,
		ContainerName: request.ContainerName,
	}
	mountRequest, plan, err := s.authorizedMountPlan(ctx, token, mountRequest, false)
	if err != nil {
		return nil, err
	}
	key := desiredMountKeyFor(mountRequest)
	lock := s.targetLock(key)
	lock.Lock()
	defer lock.Unlock()

	previous, existed := s.state.Get(key)
	if existed && previous.Request.PVName != mountRequest.PVName {
		return nil, fmt.Errorf("%w: target path %s is registered for pv %s", errBadRequest, mountRequest.TargetPath, previous.Request.PVName)
	}
	if err := s.state.Delete(key); err != nil {
		return nil, fmt.Errorf("delete desired mount: %w", err)
	}
	mounted, err := s.mounter.IsMounted(ctx, plan)
	if err != nil {
		return nil, errors.Join(errNodeOperation, err, s.restoreDesiredMount(key, previous, existed))
	}
	if mounted {
		if err := s.mounter.Unmount(ctx, plan); err != nil {
			return nil, errors.Join(errNodeOperation, err, s.restoreDesiredMount(key, previous, existed))
		}
	}

	s.logger.Printf("unmounted pv %s for pod %s/%s container %s at %s", mountRequest.PVName, mountRequest.Namespace, mountRequest.PodName, mountRequest.ContainerName, mountRequest.TargetPath)
	return &api.UnmountResult{
		Unmounted:     true,
		DriverName:    s.cfg.DriverName,
		PVName:        mountRequest.PVName,
		TargetPath:    mountRequest.TargetPath,
		ContainerName: mountRequest.ContainerName,
	}, nil
}

func (s *Server) restoreDesiredMount(key desiredMountKey, previous desiredMount, existed bool) error {
	if existed {
		if err := s.state.Put(previous); err != nil {
			return fmt.Errorf("restore desired mount after failed operation: %w", err)
		}
		return nil
	}
	if err := s.state.Delete(key); err != nil {
		return fmt.Errorf("remove pending desired mount after failed operation: %w", err)
	}
	return nil
}

func (s *Server) targetLock(key desiredMountKey) *sync.Mutex {
	hash := uint32(2166136261)
	for _, value := range []string{key.PodUID, key.ContainerName, key.TargetPath} {
		for i := 0; i < len(value); i++ {
			hash ^= uint32(value[i])
			hash *= 16777619
		}
	}
	return &s.targetLocks[hash%targetLockStripes]
}

func (s *Server) authorizedMountPlan(ctx context.Context, token string, request api.MountRequest, validateSubPath bool) (api.MountRequest, node.MountPlan, error) {
	request, err := s.normalizeMountRequest(request, validateSubPath)
	if err != nil {
		return request, node.MountPlan{}, err
	}

	tokenStatus, err := s.kube.ReviewToken(ctx, token, []string{s.cfg.TokenAudience})
	if err != nil {
		return request, node.MountPlan{}, fmt.Errorf("token review failed: %w", err)
	}
	if err := authorizeToken(tokenStatus, request.Namespace, s.cfg.TokenAudience); err != nil {
		return request, node.MountPlan{}, err
	}
	return s.mountPlanForRequest(ctx, request, tokenStatus)
}

func (s *Server) normalizeMountRequest(request api.MountRequest, validateSubPath bool) (api.MountRequest, error) {
	if err := validateRequestShape(s.cfg.DriverName, &request); err != nil {
		return request, err
	}

	cleanTarget, err := security.ValidateTargetPath(request.TargetPath)
	if err != nil {
		return request, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	request.TargetPath = cleanTarget
	if validateSubPath {
		cleanSubPath, err := security.ValidateSourceSubPath(request.SourceSubPath)
		if err != nil {
			return request, fmt.Errorf("%w: %v", errBadRequest, err)
		}
		request.SourceSubPath = cleanSubPath
	} else {
		request.SourceSubPath = ""
	}
	return request, nil
}

func (s *Server) mountPlanForRequest(ctx context.Context, request api.MountRequest, tokenStatus *kube.TokenReviewStatus) (api.MountRequest, node.MountPlan, error) {
	pod, err := s.kube.GetPod(ctx, request.Namespace, request.PodName)
	if err != nil {
		if tokenStatus == nil && kube.IsNotFound(err) {
			return request, node.MountPlan{}, fmt.Errorf("%w: pod %s/%s no longer exists", errDesiredMountStale, request.Namespace, request.PodName)
		}
		return request, node.MountPlan{}, fmt.Errorf("get pod %s/%s: %w", request.Namespace, request.PodName, err)
	}
	if tokenStatus == nil {
		if err := validatePodIdentity(pod, request); err != nil {
			return request, node.MountPlan{}, fmt.Errorf("%w: %v", errDesiredMountStale, err)
		}
	} else {
		if err := authorizePod(tokenStatus, pod, request); err != nil {
			return request, node.MountPlan{}, err
		}
	}
	if err := validatePodNode(pod, s.cfg.NodeName); err != nil {
		return request, node.MountPlan{}, err
	}
	return s.mountPlanForPod(ctx, request, pod)
}

func (s *Server) mountPlanForPod(ctx context.Context, request api.MountRequest, pod *kube.Pod) (api.MountRequest, node.MountPlan, error) {
	pv, err := s.kube.GetPersistentVolume(ctx, request.PVName)
	if err != nil {
		return request, node.MountPlan{}, fmt.Errorf("get pv %s: %w", request.PVName, err)
	}
	if err := validatePVClaimRefForPod(pod, pv); err != nil {
		return request, node.MountPlan{}, err
	}
	pvc, err := s.kube.GetPersistentVolumeClaim(ctx, pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name)
	if err != nil {
		return request, node.MountPlan{}, fmt.Errorf("get pvc %s/%s for pv %s: %w", pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name, pv.Metadata.Name, err)
	}

	if err := authorizePVForPod(s.cfg.DriverName, pod, pv, pvc); err != nil {
		return request, node.MountPlan{}, err
	}

	containerStatus, err := selectContainerStatus(pod, request.ContainerName)
	if err != nil {
		return request, node.MountPlan{}, fmt.Errorf("%w: %v", errBadRequest, err)
	}
	request.ContainerName = containerStatus.Name

	plan, err := buildMountPlan(request, pod, pv, containerStatus.ContainerID)
	if err != nil {
		return request, node.MountPlan{}, err
	}
	return request, plan, nil
}

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Response{Data: data})
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(api.Response{Error: message})
}

func requestErrorStatus(err error) int {
	switch {
	case errors.Is(err, errBadRequest), errors.Is(err, node.ErrBadSourceSubPath):
		return http.StatusBadRequest
	case errors.Is(err, node.ErrMountDisabled):
		return http.StatusServiceUnavailable
	case errors.Is(err, errNodeOperation):
		return http.StatusInternalServerError
	default:
		return http.StatusForbidden
	}
}

func bearerToken(header string) (string, error) {
	if header == "" {
		return "", fmt.Errorf("missing authorization header")
	}
	prefix := "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", fmt.Errorf("authorization header must use bearer token")
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	if token == "" {
		return "", fmt.Errorf("empty bearer token")
	}
	return token, nil
}
