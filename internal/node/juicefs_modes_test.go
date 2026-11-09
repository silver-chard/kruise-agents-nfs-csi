//go:build linux

package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestJuiceFSSourcePolicy(t *testing.T) {
	for _, test := range []struct {
		name     string
		pv       PersistentVolume
		readOnly bool
	}{
		{"writable default", PersistentVolume{}, false},
		{"operator csi readonly", PersistentVolume{ReadOnly: true}, true},
		{"ro overrides rw", PersistentVolume{MountOptions: []string{"rw,ro"}}, true},
		{"read-only true", PersistentVolume{MountOptions: []string{"read-only=true"}}, true},
		{"read-only false", PersistentVolume{MountOptions: []string{"read-only=false"}}, false},
		{"unknown boolean fails closed", PersistentVolume{MountOptions: []string{"read-only=unknown"}}, true},
		{"attribute policy", PersistentVolume{Attributes: map[string]string{"mountOptions": "subdir=/tenant,read-only"}}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := JuiceFSSourceReadOnly(test.pv); got != test.readOnly {
				t.Fatalf("readonly=%t, want %t", got, test.readOnly)
			}
		})
	}
}

func TestJuiceFSPublishSeparatesSourceAndConsumerModes(t *testing.T) {
	plan := MountPlan{ReadOnly: true, PV: PersistentVolume{VolumeHandle: "volume", MountOptions: []string{"rw", "subdir=/tenant"}, Attributes: map[string]string{"mountOptions": "cache-size=256,subdir=/attribute-root,read-only=false"}}}
	for _, sourceRO := range []bool{true, false} {
		r := juiceFSPublishRequest(plan, "/publish", sourceRO)
		if r.Readonly != sourceRO {
			t.Fatal("consumer mode replaced source mode")
		}
		want := csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
		flag := "rw"
		if sourceRO {
			want = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
			flag = "ro"
		}
		if r.VolumeCapability.AccessMode.Mode != want {
			t.Fatal("CSI access mode mismatch")
		}
		flags := r.VolumeCapability.GetMount().MountFlags
		if strings.Join(flags, ",") != "cache-size=256,subdir=/attribute-root,subdir=/tenant,"+flag {
			t.Fatalf("unexpected flags: %v", flags)
		}
		if _, ok := r.VolumeContext["mountOptions"]; ok {
			t.Fatal("duplicate mode flags left in volume context")
		}
	}
	if plan.PV.Attributes["mountOptions"] != "cache-size=256,subdir=/attribute-root,read-only=false" {
		t.Fatal("PV attributes mutated")
	}
}

func TestJuiceFSLeaseMixedConsumersAndModeConflicts(t *testing.T) {
	for _, firstRO := range []bool{true, false} {
		plan := MountPlan{PV: PersistentVolume{Name: "pv"}, PodUID: "pod", ContainerName: "main", TargetPath: "/first", SourceSubPath: "user/a", ReadOnly: firstRO}
		lease := &juiceFSLease{Version: 2, Consumers: map[string]string{juiceFSConsumer(plan): plan.SourceSubPath}, ConsumerReadOnly: map[string]bool{juiceFSConsumer(plan): firstRO}}
		other := plan
		other.TargetPath = "/second"
		other.ReadOnly = !firstRO
		if err := validateJuiceFSLeaseAccess(other, lease); err != nil {
			t.Fatalf("RW source cannot mix targets: %v", err)
		}
		if juiceFSKey(plan) != juiceFSKey(other) {
			t.Fatal("access mode split the source identity")
		}
		other.TargetPath = plan.TargetPath
		if err := validateJuiceFSLeaseAccess(other, lease); err == nil {
			t.Fatal("existing target mode changed without unmount")
		}
	}
}

func TestJuiceFSLegacyLeaseRemainsReadOnly(t *testing.T) {
	dir := t.TempDir()
	m := &linuxMounter{cfg: Config{JuiceFSStateDir: dir, JuiceFSPublishRoot: filepath.Join(dir, "publish")}}
	plan := MountPlan{PV: PersistentVolume{Name: "pv", VolumeHandle: "handle"}, PodUID: "pod", ContainerName: "main", TargetPath: "/old", ReadOnly: true}
	legacy := &juiceFSLease{Version: 1, PodUID: plan.PodUID, PVName: plan.PV.Name, VolumeHandle: plan.PV.VolumeHandle, Target: m.juiceFSStagePath(plan), Consumers: map[string]string{juiceFSConsumer(plan): "user/a"}}
	if err := m.writeJuiceFSLease(plan, legacy); err != nil {
		t.Fatal(err)
	}
	l, err := m.readJuiceFSLease(plan)
	if err != nil {
		t.Fatal(err)
	}
	if l.Version != 2 || !l.SourceReadOnly || !l.ConsumerReadOnly[juiceFSConsumer(plan)] {
		t.Fatal("legacy lease silently became writable")
	}
	if err := validateJuiceFSLeaseAccess(plan, l); err != nil {
		t.Fatal(err)
	}
	plan.TargetPath = "/new"
	plan.ReadOnly = false
	if err := validateJuiceFSLeaseAccess(plan, l); err == nil {
		t.Fatal("active legacy source promoted to writable")
	}
	if err := m.writeJuiceFSLease(plan, l); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, juiceFSKey(plan)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"version":2`) {
		t.Fatal("legacy migration not persisted")
	}
}

func TestJuiceFSLeaseRejectsMissingModes(t *testing.T) {
	dir := t.TempDir()
	m := &linuxMounter{cfg: Config{JuiceFSStateDir: dir, JuiceFSPublishRoot: filepath.Join(dir, "publish")}}
	plan := MountPlan{PV: PersistentVolume{Name: "pv"}, PodUID: "pod", ContainerName: "main", TargetPath: "/data"}
	l := &juiceFSLease{Version: 2, PodUID: plan.PodUID, PVName: plan.PV.Name, Target: m.juiceFSStagePath(plan), Consumers: map[string]string{juiceFSConsumer(plan): "user/a"}, ConsumerReadOnly: map[string]bool{}}
	if err := m.writeJuiceFSLease(plan, l); err != nil {
		t.Fatal(err)
	}
	if _, err := m.readJuiceFSLease(plan); err == nil {
		t.Fatal("v2 lease missing explicit target mode accepted")
	}
}
