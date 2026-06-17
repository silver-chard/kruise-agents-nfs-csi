package security

import (
	"fmt"
	"path"
	"strings"
)

var deniedExactTargetPaths = map[string]struct{}{
	"/":     {},
	"/proc": {},
	"/sys":  {},
	"/dev":  {},
}

var deniedTargetPrefixes = []string{
	"/proc/",
	"/sys/",
	"/dev/",
	"/run/secrets/",
	"/var/run/secrets/",
	"/etc/kubernetes/",
	"/etc/ssl/private/",
	"/root/.kube/",
	"/var/lib/kubelet/",
	"/var/lib/kruise-agents-nfs-csi/",
}

var deniedTargetFiles = map[string]struct{}{
	"/etc/passwd": {},
	"/etc/shadow": {},
	"/etc/group":  {},
}

func ValidateTargetPath(targetPath string) (string, error) {
	if strings.ContainsRune(targetPath, '\x00') {
		return "", fmt.Errorf("target path contains NUL byte")
	}
	if !strings.HasPrefix(targetPath, "/") {
		return "", fmt.Errorf("target path %q must be absolute", targetPath)
	}

	cleaned := path.Clean(targetPath)
	if _, denied := deniedExactTargetPaths[cleaned]; denied {
		return "", fmt.Errorf("target path %s is not allowed", cleaned)
	}
	if _, denied := deniedTargetFiles[cleaned]; denied {
		return "", fmt.Errorf("target path %s is not allowed", cleaned)
	}
	for _, prefix := range deniedTargetPrefixes {
		if strings.HasPrefix(cleaned+"/", prefix) || strings.HasPrefix(cleaned, prefix) {
			return "", fmt.Errorf("target path %s is not allowed", cleaned)
		}
	}

	return cleaned, nil
}
