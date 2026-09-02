package node

import (
	"fmt"
	"path"
	"strings"
)

// NormalizeNFSSubDir validates and normalizes a PV CSI subDir relative to the
// configured NFS share. Empty, slash-only, and dot-only values identify the
// share root.
func NormalizeNFSSubDir(value string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("nfs subDir contains NUL byte")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return "", fmt.Errorf("nfs subDir %q must not contain ..", value)
		}
	}

	cleaned := path.Clean("/" + strings.TrimLeft(value, "/"))
	if cleaned == "/" {
		return "", nil
	}
	return strings.TrimPrefix(cleaned, "/"), nil
}

// IsNFSExportRoot reports whether a plan selects the configured NFS share
// root instead of a PV or request subdirectory below it.
func IsNFSExportRoot(plan MountPlan) bool {
	return plan.PV.SubDir == "" && plan.SourceSubPath == ""
}
