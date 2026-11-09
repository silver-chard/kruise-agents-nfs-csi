package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
)

const JuiceFSAllowedMountsAnnotation = "drive.juicefs.com/allowed-mounts"

// This annotation is an authorization statement written only by the trusted
// Manager, not a mount request channel. Sandbox service accounts must not be
// allowed to patch their Pods, PVs, or this annotation.
func authorizeJuiceFSMount(pod *kube.Pod, request api.MountRequest) error {
	type grant struct {
		PVName        string `json:"pv_name"`
		SourceSubPath string `json:"source_sub_path"`
		TargetPath    string `json:"target_path"`
		ContainerName string `json:"container_name"`
		ReadOnly      *bool  `json:"read_only"`
	}
	var grants []grant
	if err := json.Unmarshal([]byte(pod.Metadata.Annotations[JuiceFSAllowedMountsAnnotation]), &grants); err != nil {
		return fmt.Errorf("missing or invalid trusted JuiceFS mount authorization")
	}
	for _, g := range grants {
		// Missing/null is not an implicit writable grant. Manager must explicitly
		// authorize the requested mode as well as the exact source and target.
		if g.ReadOnly != nil && *g.ReadOnly == request.ReadOnly &&
			g.PVName == request.PVName && g.SourceSubPath == request.SourceSubPath &&
			g.TargetPath == request.TargetPath && g.ContainerName == request.ContainerName {
			return nil
		}
	}
	return fmt.Errorf("JuiceFS source/target is not authorized for this Pod")
}

func (s *Server) loadJuiceFSSecrets(ctx context.Context, pv *kube.PersistentVolume, plan *node.MountPlan) error {
	ref := pv.Spec.CSI.NodePublishSecretRef
	if ref == nil {
		return fmt.Errorf("JuiceFS PV must reference an allowed nodePublishSecretRef")
	}
	allowed := false
	for _, v := range strings.Split(s.cfg.JuiceFSSecretRefs, ",") {
		if strings.TrimSpace(v) == ref.Namespace+"/"+ref.Name {
			allowed = true
		}
	}
	if !allowed {
		return fmt.Errorf("JuiceFS PV nodePublishSecretRef is not allowed by wrapper configuration")
	}
	reader, ok := s.kube.(interface {
		GetSecret(context.Context, string, string) (*kube.Secret, error)
	})
	if !ok {
		return fmt.Errorf("Kubernetes client does not support Secret reads")
	}
	secret, err := reader.GetSecret(ctx, ref.Namespace, ref.Name)
	if err != nil {
		return fmt.Errorf("cannot load allowed JuiceFS credentials")
	}
	for k, v := range secret.Data {
		plan.PV.Secrets[k] = string(v)
	}
	return nil
}

func (s *Server) releaseSource(ctx context.Context, request api.MountRequest) error {
	releaser, ok := s.mounter.(interface {
		Release(context.Context, node.MountPlan) error
	})
	if !ok {
		return nil
	}
	return releaser.Release(ctx, node.MountPlan{PV: node.PersistentVolume{Name: request.PVName, Driver: request.DriverName}, PodUID: request.PodUID, ContainerName: request.ContainerName, TargetPath: request.TargetPath})
}
