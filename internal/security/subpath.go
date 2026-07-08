package security

import (
	"fmt"
	"path"
	"strings"
)

// ValidateSourceSubPath validates a directory path relative to the PV root.
func ValidateSourceSubPath(subPath string) (string, error) {
	if subPath == "" {
		return "", nil
	}
	if strings.ContainsRune(subPath, '\x00') {
		return "", fmt.Errorf("source_sub_path contains NUL byte")
	}
	if path.IsAbs(subPath) {
		return "", fmt.Errorf("source_sub_path %q must be relative", subPath)
	}
	for _, segment := range strings.Split(subPath, "/") {
		if segment == ".." {
			return "", fmt.Errorf("source_sub_path %q must not contain ..", subPath)
		}
	}

	cleaned := path.Clean(subPath)
	if cleaned == "." {
		return "", nil
	}
	return cleaned, nil
}
