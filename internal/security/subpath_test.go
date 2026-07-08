package security

import "testing"

func TestValidateSourceSubPathAllowsRelativeDirectoryPath(t *testing.T) {
	got, err := ValidateSourceSubPath("./users//alice/workspace")
	if err != nil {
		t.Fatalf("ValidateSourceSubPath returned error: %v", err)
	}
	if got != "users/alice/workspace" {
		t.Fatalf("cleaned source_sub_path = %q, want users/alice/workspace", got)
	}
}

func TestValidateSourceSubPathAllowsEmptyPath(t *testing.T) {
	got, err := ValidateSourceSubPath("")
	if err != nil {
		t.Fatalf("ValidateSourceSubPath returned error: %v", err)
	}
	if got != "" {
		t.Fatalf("cleaned source_sub_path = %q, want empty", got)
	}
}

func TestValidateSourceSubPathRejectsUnsafePaths(t *testing.T) {
	tests := []string{
		"/absolute/path",
		"../escape",
		"safe/../../escape",
		"safe/../escape",
		"safe/\x00/path",
	}

	for _, tt := range tests {
		t.Run(tt, func(t *testing.T) {
			if _, err := ValidateSourceSubPath(tt); err == nil {
				t.Fatalf("ValidateSourceSubPath(%q) succeeded, want error", tt)
			}
		})
	}
}
