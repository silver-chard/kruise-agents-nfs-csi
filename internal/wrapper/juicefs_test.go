package wrapper

import (
	"context"
	"fmt"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/config"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/kube"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
	"testing"
)

type multiVolumeSecretKube struct {
	secrets map[string]*kube.Secret
}

func (*multiVolumeSecretKube) ReviewToken(context.Context, string, []string) (*kube.TokenReviewStatus, error) {
	return nil, fmt.Errorf("not implemented")
}

func (*multiVolumeSecretKube) GetPod(context.Context, string, string) (*kube.Pod, error) {
	return nil, fmt.Errorf("not implemented")
}

func (*multiVolumeSecretKube) RunPodInformer(context.Context, string, func([]kube.Pod), func(kube.PodWatchEvent)) error {
	return fmt.Errorf("not implemented")
}

func (*multiVolumeSecretKube) GetPersistentVolume(context.Context, string) (*kube.PersistentVolume, error) {
	return nil, fmt.Errorf("not implemented")
}

func (k *multiVolumeSecretKube) GetSecret(_ context.Context, namespace, name string) (*kube.Secret, error) {
	secret, ok := k.secrets[namespace+"/"+name]
	if !ok {
		return nil, fmt.Errorf("secret not found")
	}
	return secret, nil
}

func TestJuiceFSAuthorizationExactAndReadOnly(t *testing.T) {
	pod := &kube.Pod{Metadata: kube.ObjectMeta{Annotations: map[string]string{JuiceFSAllowedMountsAnnotation: `[{"pv_name":"shared","source_sub_path":"users/alice","target_path":"/mnt/personal","container_name":"main","read_only":true}]`}}}
	request := api.MountRequest{PVName: "shared", SourceSubPath: "users/alice", TargetPath: "/mnt/personal", ContainerName: "main", ReadOnly: true}
	if err := authorizeJuiceFSMount(pod, request); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*api.MountRequest){"other-user": func(r *api.MountRequest) { r.SourceSubPath = "users/bob" }, "root": func(r *api.MountRequest) { r.SourceSubPath = "" }, "write": func(r *api.MountRequest) { r.ReadOnly = false }, "target": func(r *api.MountRequest) { r.TargetPath = "/etc" }, "container": func(r *api.MountRequest) { r.ContainerName = "sidecar" }}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			r := request
			change(&r)
			if authorizeJuiceFSMount(pod, r) == nil {
				t.Fatal("unauthorized mount accepted")
			}
		})
	}
	delete(pod.Metadata.Annotations, JuiceFSAllowedMountsAnnotation)
	if authorizeJuiceFSMount(pod, request) == nil {
		t.Fatal("missing grant accepted")
	}
}

func TestJuiceFSPlanDoesNotRequireNFSServer(t *testing.T) {
	pv := &kube.PersistentVolume{Metadata: kube.ObjectMeta{Name: "shared"}, Spec: kube.PersistentVolumeSpec{CSI: &kube.CSIPersistentVolumeSource{Driver: node.JuiceFSDriver, VolumeHandle: "handle", VolumeAttributes: map[string]string{"name": "drive"}}, MountOptions: []string{"subdir=/tenants/team"}}}
	pod := &kube.Pod{Metadata: kube.ObjectMeta{Name: "warm-pod", Namespace: "demo", UID: "uid"}}
	request := api.MountRequest{ReadOnly: true, SourceSubPath: "users/alice", ContainerName: "main", TargetPath: "/mnt/personal"}
	plan, err := buildMountPlan(request, pod, pv, "containerd://id")
	if err != nil {
		t.Fatal(err)
	}
	if plan.PV.Driver != node.JuiceFSDriver || plan.PV.VolumeHandle != "handle" || !plan.ReadOnly || plan.PodNamespace != "demo" {
		t.Fatalf("unexpected plan: %+v", plan)
	}
	if node.IsNFSExportRoot(plan) {
		t.Fatal("JuiceFS misclassified as NFS export root")
	}
	request.ReadOnly = false
	if writable, err := buildMountPlan(request, pod, pv, "id"); err != nil || writable.ReadOnly {
		t.Fatalf("writable PV did not produce writable plan: %+v, %v", writable, err)
	}
	pv.Spec.CSI.ReadOnly = true
	if _, err := buildMountPlan(request, pod, pv, "id"); err == nil {
		t.Fatal("read-only PV accepted a writable target")
	}
}

func TestJuiceFSAuthorizationRequiresExplicitMatchingMode(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		readOnly   bool
		allowed    bool
	}{
		{"explicit writable", `,"read_only":false`, false, true},
		{"explicit readonly", `,"read_only":true`, true, true},
		{"missing writable", ``, false, false},
		{"null writable", `,"read_only":null`, false, false},
		{"string writable", `,"read_only":"false"`, false, false},
		{"readonly cannot grant write", `,"read_only":true`, false, false},
		{"mode must match", `,"read_only":false`, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			annotation := `[{"pv_name":"shared","source_sub_path":"user/alice","target_path":"/data","container_name":"main"` + test.mode + `}]`
			pod := &kube.Pod{Metadata: kube.ObjectMeta{Annotations: map[string]string{JuiceFSAllowedMountsAnnotation: annotation}}}
			request := api.MountRequest{PVName: "shared", SourceSubPath: "user/alice", TargetPath: "/data", ContainerName: "main", ReadOnly: test.readOnly}
			if err := authorizeJuiceFSMount(pod, request); (err == nil) != test.allowed {
				t.Fatalf("allowed = %v, error = %v", test.allowed, err)
			}
		})
	}
}

func TestJuiceFSPlanRespectsPVReadonlyOptions(t *testing.T) {
	for _, test := range []struct {
		name       string
		csiRO      bool
		options    []string
		attributes map[string]string
	}{
		{"csi", true, nil, nil},
		{"mount ro", false, []string{"subdir=/tenant", "ro"}, nil},
		{"mount read-only", false, []string{"read-only"}, nil},
		{"attributes ro", false, nil, map[string]string{"mountOptions": "subdir=/tenant,ro"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			pv := &kube.PersistentVolume{Spec: kube.PersistentVolumeSpec{CSI: &kube.CSIPersistentVolumeSource{Driver: node.JuiceFSDriver, ReadOnly: test.csiRO, VolumeAttributes: test.attributes}, MountOptions: test.options}}
			if _, err := buildMountPlan(api.MountRequest{ReadOnly: false}, &kube.Pod{}, pv, "container"); err == nil {
				t.Fatal("PV read-only policy ignored")
			}
			if _, err := buildMountPlan(api.MountRequest{ReadOnly: true}, &kube.Pod{}, pv, "container"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadJuiceFSSecretsSelectsPerVolumeBackend(t *testing.T) {
	server := &Server{
		cfg: config.WrapperConfig{JuiceFSSecretRefs: "kube-system/volume-a,kube-system/volume-b"},
		kube: &multiVolumeSecretKube{secrets: map[string]*kube.Secret{
			"kube-system/volume-a": {Data: map[string][]byte{"metaurl": []byte("mysql://metadata-a/volume_a")}},
			"kube-system/volume-b": {Data: map[string][]byte{"metaurl": []byte("redis://metadata-b/2")}},
		}},
	}

	tests := []struct {
		name, secret, want string
	}{
		{name: "mysql volume", secret: "volume-a", want: "mysql://metadata-a/volume_a"},
		{name: "redis volume", secret: "volume-b", want: "redis://metadata-b/2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pv := &kube.PersistentVolume{Spec: kube.PersistentVolumeSpec{CSI: &kube.CSIPersistentVolumeSource{NodePublishSecretRef: &kube.SecretReference{Namespace: "kube-system", Name: test.secret}}}}
			plan := node.MountPlan{PV: node.PersistentVolume{Secrets: map[string]string{}}}
			if err := server.loadJuiceFSSecrets(context.Background(), pv, &plan); err != nil {
				t.Fatal(err)
			}
			if got := plan.PV.Secrets["metaurl"]; got != test.want {
				t.Fatalf("metaurl = %q, want %q", got, test.want)
			}
		})
	}
}
