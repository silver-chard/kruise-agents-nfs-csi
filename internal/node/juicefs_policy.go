package node

import "strings"

// JuiceFSSourceReadOnly describes the operator-owned volume, independently of
// the Manager's per-target grant. A writable source may serve read-only targets;
// a read-only source must never be promoted by a writable target request.
func JuiceFSSourceReadOnly(pv PersistentVolume) bool {
	if pv.ReadOnly {
		return true
	}
	options := append([]string(nil), pv.MountOptions...)
	options = append(options, pv.Attributes["mountOptions"])
	for _, option := range options {
		for _, flag := range strings.Split(option, ",") {
			key, value, hasValue := strings.Cut(strings.TrimSpace(flag), "=")
			if key == "ro" {
				return true
			}
			if key == "read-only" && (!hasValue || (value != "false" && value != "0")) {
				return true
			}
		}
	}
	return false
}
