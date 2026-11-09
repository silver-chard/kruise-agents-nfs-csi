package wrapper

import (
	"context"
	"testing"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
)

func TestClaimBindingPolicy(t *testing.T) {
	const ns = "sandbox-ns"
	for _, tt := range []struct {
		name                                 string
		refNamespace, refUID, pvcUID, volume string
		policy                               map[string]string
		wantRead, wantOK                     bool
	}{
		{name: "existing binding", refNamespace: ns, refUID: "uid", pvcUID: "uid", volume: "pv-a", wantRead: true, wantOK: true},
		{name: "cross namespace", refNamespace: "other", refUID: "uid"},
		{name: "unbound PV"},
		{name: "missing claim UID", refNamespace: ns},
		{name: "recreated PVC", refNamespace: ns, refUID: "old", pvcUID: "new", volume: "pv-a", wantRead: true},
		{name: "PVC bound to another PV", refNamespace: ns, refUID: "uid", pvcUID: "uid", volume: "other", wantRead: true},
		{name: "explicit namespace", policy: map[string]string{allowNamespaceAnnotation: ns}, wantOK: true},
		{name: "namespace rejected", policy: map[string]string{allowNamespaceAnnotation: "other"}},
		{name: "service account cannot allow other namespace", refNamespace: "other", refUID: "uid", policy: map[string]string{allowServiceAccountAnnotation: "sandbox-sa"}},
		{name: "service account and bound claim", refNamespace: ns, refUID: "uid", pvcUID: "uid", volume: "pv-a", policy: map[string]string{allowServiceAccountAnnotation: "sandbox-sa"}, wantRead: true, wantOK: true},
		{name: "service account rejected", policy: map[string]string{allowNamespaceAnnotation: ns, allowServiceAccountAnnotation: "other"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _, k, _ := newReconcileTestServer(t)
			s.cfg.RequireBoundPVC = true
			k.pv.Metadata.Annotations = tt.policy
			k.pv.Spec.ClaimRef = nil
			if tt.refNamespace != "" {
				k.pv.Spec.ClaimRef = &kube.ObjectReference{Name: "claim", Namespace: tt.refNamespace, UID: tt.refUID}
			}
			k.claim = &kube.PersistentVolumeClaimResource{Metadata: kube.ObjectMeta{Name: "claim", Namespace: ns, UID: tt.pvcUID}}
			k.claim.Spec.VolumeName = tt.volume
			_, _, err := s.mountPlanForPod(context.Background(), testMountRequest(), k.pod)
			if (err == nil) != tt.wantOK || k.claimRead != tt.wantRead {
				t.Fatalf("error=%v, read=%t, want success=%t read=%t", err, k.claimRead, tt.wantOK, tt.wantRead)
			}
		})
	}
}
