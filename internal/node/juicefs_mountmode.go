//go:build linux

package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func juiceFSPathSegment(s string) bool {
	if s == "" || len(s) > 253 || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func juiceFSMountPodTarget(root, uid, pv string) string {
	return filepath.Join(root, "pods", uid, "volumes", "kubernetes.io~csi", pv, "mount")
}

// ValidateJuiceFSConfig rejects mode/root changes with persisted sources. Even
// an empty consumer list may be a pending unpublish, not permission to migrate.
func ValidateJuiceFSConfig(mode, root, state string) error {
	if mode != "" && mode != "process" && mode != "mountpod" {
		return fmt.Errorf("unknown mount mode %q", mode)
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || strings.Contains(root, "/pods") {
		return fmt.Errorf("publish root must be a clean absolute operator-owned path, not a pods subtree")
	}
	if !filepath.IsAbs(state) || filepath.Clean(state) != state || state == "/" {
		return fmt.Errorf("invalid JuiceFS state root")
	}
	entries, err := os.ReadDir(state)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(state, entry.Name()))
		if err != nil {
			return err
		}
		var l struct {
			PodUID string `json:"pod_uid"`
			PVName string `json:"pv_name"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(b, &l); err != nil {
			return fmt.Errorf("invalid existing JuiceFS lease %s", entry.Name())
		}
		if !juiceFSPathSegment(l.PodUID) || !juiceFSPathSegment(l.PVName) {
			return fmt.Errorf("invalid existing JuiceFS lease identity")
		}
		plan := MountPlan{PodUID: l.PodUID, PV: PersistentVolume{Name: l.PVName}}
		expected := filepath.Join(root, juiceFSKey(plan))
		if mode == "mountpod" {
			expected = juiceFSMountPodTarget(root, l.PodUID, l.PVName)
		}
		if l.Target != expected {
			return fmt.Errorf("existing JuiceFS lease %s belongs to a different mode/root; release old consumers before switching", entry.Name())
		}
	}
	return nil
}
