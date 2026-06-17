package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/api"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/config"
	"google.golang.org/protobuf/proto"
)

func TestParseRuntimeMountRequest(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "sandbox-ns")
	t.Setenv("POD_NAME", "sandbox-pod")
	t.Setenv("POD_UID", "pod-uid")

	raw, err := proto.Marshal(&csi.NodePublishVolumeRequest{
		VolumeId:   "pvc-1234-abcdef",
		TargetPath: "/workspace/data",
	})
	if err != nil {
		t.Fatalf("marshal csi request: %v", err)
	}

	_, request, err := parseRequest([]string{
		"mount",
		"--driver", "csi.nfs.zhida",
		"--config", base64.StdEncoding.EncodeToString(raw),
	}, config.MounterConfig{
		DriverName:  "csi.nfs.zhida",
		SocketPath:  "/socket.sock",
		TokenFile:   "/token",
		HTTPTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("parseRequest returned error: %v", err)
	}

	if request.DriverName != "csi.nfs.zhida" {
		t.Fatalf("driver name = %q, want csi.nfs.zhida", request.DriverName)
	}
	if request.PVName != "pvc-1234" {
		t.Fatalf("pv name = %q, want pvc-1234", request.PVName)
	}
	if request.TargetPath != "/workspace/data" {
		t.Fatalf("target path = %q, want /workspace/data", request.TargetPath)
	}
	if request.PodName != "sandbox-pod" || request.Namespace != "sandbox-ns" {
		t.Fatalf("pod identity = %s/%s, want sandbox-ns/sandbox-pod", request.Namespace, request.PodName)
	}
}

func TestCompletePodIdentityFromFiles(t *testing.T) {
	dir := t.TempDir()
	namespaceFile := filepath.Join(dir, "namespace")
	podNameFile := filepath.Join(dir, "pod_name")
	podUIDFile := filepath.Join(dir, "pod_uid")

	for path, value := range map[string]string{
		namespaceFile: "sandbox-ns\n",
		podNameFile:   "sandbox-pod\n",
		podUIDFile:    "pod-uid\n",
	} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	request := completePodIdentity(apiMountRequest(), config.MounterConfig{
		NamespaceFile: namespaceFile,
		PodNameFile:   podNameFile,
		PodUIDFile:    podUIDFile,
	})

	if request.Namespace != "sandbox-ns" || request.PodName != "sandbox-pod" || request.PodUID != "pod-uid" {
		t.Fatalf("pod identity = %s/%s/%s, want sandbox-ns/sandbox-pod/pod-uid", request.Namespace, request.PodName, request.PodUID)
	}
}

func apiMountRequest() api.MountRequest {
	return api.MountRequest{
		APIVersion: api.Version,
		DriverName: "csi.nfs.zhida",
		PVName:     "pv",
		TargetPath: "/data",
	}
}

func TestPVNameFromNodePublishRequestPrefersContext(t *testing.T) {
	got, err := pvNameFromNodePublishRequest(&csi.NodePublishVolumeRequest{
		VolumeId: "generated-id-abcdef",
		VolumeContext: map[string]string{
			"csi.storage.k8s.io/pv/name": "real-pv-name",
		},
	})
	if err != nil {
		t.Fatalf("pvNameFromNodePublishRequest returned error: %v", err)
	}
	if got != "real-pv-name" {
		t.Fatalf("pv name = %q, want real-pv-name", got)
	}
}

func TestDerivePVNameFromVolumeID(t *testing.T) {
	tests := map[string]string{
		"pv-one-abc123":      "pv-one",
		"pv-one-not-RANDOM":  "pv-one-not-RANDOM",
		"pv-with-six-12345":  "pv-with-six-12345",
		"pv-with-six-123456": "pv-with-six",
	}

	for volumeID, want := range tests {
		t.Run(volumeID, func(t *testing.T) {
			got, err := derivePVNameFromVolumeID(volumeID)
			if err != nil {
				t.Fatalf("derivePVNameFromVolumeID returned error: %v", err)
			}
			if got != want {
				t.Fatalf("derived pv name = %q, want %q", got, want)
			}
		})
	}
}
