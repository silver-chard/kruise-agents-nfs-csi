package security

import "testing"

func TestValidateTargetPathAllowsNormalAbsolutePath(t *testing.T) {
	got, err := ValidateTargetPath("/workspace/../workspace/data")
	if err != nil {
		t.Fatalf("ValidateTargetPath returned error: %v", err)
	}
	if got != "/workspace/data" {
		t.Fatalf("cleaned path = %q, want /workspace/data", got)
	}
}

func TestValidateTargetPathRejectsDangerousPaths(t *testing.T) {
	tests := []string{
		"/",
		"/proc",
		"/proc/1/root",
		"/sys/fs/cgroup",
		"/dev/null",
		"/run/secrets/kubernetes.io/serviceaccount",
		"/var/run/secrets/kubernetes.io/serviceaccount",
		"/var/lib/kubelet/pods",
		"/etc/passwd",
		"relative/path",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			if _, err := ValidateTargetPath(tt); err == nil {
				t.Fatalf("ValidateTargetPath(%q) succeeded, want error", tt)
			}
		})
	}
}
