package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadWrapperConfigSubPathDefaults(t *testing.T) {
	t.Setenv("WRAPPER_CREATE_MISSING_SUBPATHS", "")
	t.Setenv("WRAPPER_CREATED_SUBPATH_MODE", "")

	cfg, err := LoadWrapperConfig()
	if err != nil {
		t.Fatalf("LoadWrapperConfig returned error: %v", err)
	}
	if cfg.CreateMissingSubPaths {
		t.Fatal("CreateMissingSubPaths = true, want false")
	}
	if cfg.CreatedSubPathMode != DefaultCreatedSubPathMode {
		t.Fatalf("CreatedSubPathMode = %s, want %s", FormatFileMode(cfg.CreatedSubPathMode), FormatFileMode(DefaultCreatedSubPathMode))
	}
}

func TestLoadWrapperConfigSubPathEnv(t *testing.T) {
	t.Setenv("WRAPPER_CREATE_MISSING_SUBPATHS", "true")
	t.Setenv("WRAPPER_CREATED_SUBPATH_MODE", "2770")
	t.Setenv("WRAPPER_EXPORT_ROOT_KEY_FILE", "/run/secrets/export-root-key")

	cfg, err := LoadWrapperConfig()
	if err != nil {
		t.Fatalf("LoadWrapperConfig returned error: %v", err)
	}
	if !cfg.CreateMissingSubPaths {
		t.Fatal("CreateMissingSubPaths = false, want true")
	}
	wantMode := os.FileMode(0o770) | os.ModeSetgid
	if cfg.CreatedSubPathMode != wantMode {
		t.Fatalf("CreatedSubPathMode = %s, want 2770", FormatFileMode(cfg.CreatedSubPathMode))
	}
	if cfg.ExportRootKeyFile != "/run/secrets/export-root-key" {
		t.Fatalf("ExportRootKeyFile = %q, want configured path", cfg.ExportRootKeyFile)
	}
}

func TestLoadMounterConfigExportRootKeyFile(t *testing.T) {
	t.Setenv("EXPORT_ROOT_KEY_FILE", "/run/secrets/export-root-key")
	if got := LoadMounterConfig().ExportRootKeyFile; got != "/run/secrets/export-root-key" {
		t.Fatalf("ExportRootKeyFile = %q, want configured path", got)
	}
}

func TestLoadWrapperConfigRejectsInvalidCreatedSubPathMode(t *testing.T) {
	for _, value := range []string{"0000", "0780"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("WRAPPER_CREATED_SUBPATH_MODE", value)

			_, err := LoadWrapperConfig()
			if err == nil {
				t.Fatal("LoadWrapperConfig returned nil error, want invalid mode error")
			}
			if !strings.Contains(err.Error(), "WRAPPER_CREATED_SUBPATH_MODE") {
				t.Fatalf("error = %q, want environment variable name", err)
			}
		})
	}
}

func TestParseFileMode(t *testing.T) {
	tests := []struct {
		value string
		want  os.FileMode
	}{
		{value: "0001", want: 0o001},
		{value: "0770", want: 0o770},
		{value: "4770", want: 0o770 | os.ModeSetuid},
		{value: "2770", want: 0o770 | os.ModeSetgid},
		{value: "1770", want: 0o770 | os.ModeSticky},
		{value: "7777", want: 0o777 | os.ModeSetuid | os.ModeSetgid | os.ModeSticky},
	}

	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got, err := ParseFileMode(test.value)
			if err != nil {
				t.Fatalf("ParseFileMode returned error: %v", err)
			}
			if got != test.want {
				t.Fatalf("mode = %#o, want %#o", got, test.want)
			}
			if formatted := FormatFileMode(got); formatted != test.value {
				t.Fatalf("FormatFileMode = %q, want %q", formatted, test.value)
			}
		})
	}
}

func TestParseFileModeRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "-1", "0000", "0780", "mode", "10000"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseFileMode(value); err == nil {
				t.Fatalf("ParseFileMode(%q) returned nil error", value)
			}
		})
	}
}
