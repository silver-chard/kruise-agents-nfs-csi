package wrapper

import (
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/config"
	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
	"testing"
	"time"
)

func TestMountPodTimeoutDoesNotAffectNFS(t *testing.T) {
	s := &Server{cfg: config.WrapperConfig{JuiceFSMountMode: "mountpod", RequestTimeout: 30 * time.Second, JuiceFSRequestTimeout: 180 * time.Second}}
	if s.operationTimeout(node.JuiceFSDriver) != 180*time.Second {
		t.Fatal("cold start timeout missing")
	}
	if s.operationTimeout("csi.nfs.cnap.com") != 30*time.Second {
		t.Fatal("NFS timeout changed")
	}
	s.cfg.JuiceFSMountMode = "process"
	if s.operationTimeout(node.JuiceFSDriver) != 30*time.Second {
		t.Fatal("legacy timeout changed")
	}
}
