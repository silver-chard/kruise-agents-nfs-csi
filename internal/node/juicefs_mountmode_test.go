//go:build linux

package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMountPodTargetAndModeGuard(t *testing.T) {
	d := t.TempDir()
	root := filepath.Join(d, "publish")
	state := filepath.Join(d, "state")
	plan := MountPlan{PodUID: "real-pod-uid", PV: PersistentVolume{Name: "test-pv"}}
	old := &linuxMounter{cfg: Config{JuiceFSPublishRoot: root, JuiceFSStateDir: state}}
	m := &linuxMounter{cfg: Config{JuiceFSPublishRoot: root, JuiceFSStateDir: state, JuiceFSMountMode: "mountpod"}}
	if got := m.juiceFSStagePath(plan); got != root+"/pods/real-pod-uid/volumes/kubernetes.io~csi/test-pv/mount" {
		t.Fatal(got)
	}
	if err := ValidateJuiceFSConfig("mountpod", root, state); err != nil {
		t.Fatal(err)
	}
	l := &juiceFSLease{Version: 2, PodUID: plan.PodUID, PVName: plan.PV.Name, Target: old.juiceFSStagePath(plan), Consumers: map[string]string{}, ConsumerReadOnly: map[string]bool{}}
	if err := old.writeJuiceFSLease(plan, l); err != nil {
		t.Fatal(err)
	}
	if err := ValidateJuiceFSConfig("mountpod", root, state); err == nil {
		t.Fatal("adopted process lease")
	}
	if err := ValidateJuiceFSConfig("process", root, state); err != nil {
		t.Fatal(err)
	}
	l.Target = m.juiceFSStagePath(plan)
	if err := m.writeJuiceFSLease(plan, l); err != nil {
		t.Fatal(err)
	}
	if err := ValidateJuiceFSConfig("mountpod", root, state); err != nil {
		t.Fatal(err)
	}
	if err := ValidateJuiceFSConfig("process", root, state); err == nil {
		t.Fatal("adopted mountpod lease")
	}
	os.RemoveAll(state)
	for _, bad := range []string{"", "/", "relative", "/a/../b", "/var/lib/kubelet/pods/uid"} {
		if ValidateJuiceFSConfig("mountpod", bad, state) == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, bad := range []string{"../escape", ".", "..", "", "a/b"} {
		if juiceFSPathSegment(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
