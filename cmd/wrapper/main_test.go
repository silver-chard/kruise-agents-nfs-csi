package main

import (
	"flag"
	"io"
	"os"
	"testing"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/config"
)

func TestBindWrapperFlagsSubPathConfig(t *testing.T) {
	cfg := config.WrapperConfig{
		CreatedSubPathMode: config.DefaultCreatedSubPathMode,
	}
	flagSet := flag.NewFlagSet("wrapper", flag.ContinueOnError)
	flagSet.SetOutput(io.Discard)
	bindWrapperFlags(flagSet, &cfg)

	if err := flagSet.Parse([]string{
		"--create-missing-subpaths=true",
		"--created-subpath-mode=2770",
		"--export-root-key-file=/run/secrets/export-root-key",
	}); err != nil {
		t.Fatalf("parse wrapper flags: %v", err)
	}
	if !cfg.CreateMissingSubPaths {
		t.Fatal("CreateMissingSubPaths = false, want true")
	}
	wantMode := os.FileMode(0o770) | os.ModeSetgid
	if cfg.CreatedSubPathMode != wantMode {
		t.Fatalf("CreatedSubPathMode = %s, want 2770", config.FormatFileMode(cfg.CreatedSubPathMode))
	}
	if cfg.ExportRootKeyFile != "/run/secrets/export-root-key" {
		t.Fatalf("ExportRootKeyFile = %q, want configured path", cfg.ExportRootKeyFile)
	}
}

func TestBindWrapperFlagsRejectsInvalidCreatedSubPathMode(t *testing.T) {
	for _, value := range []string{"0000", "0780"} {
		t.Run(value, func(t *testing.T) {
			cfg := config.WrapperConfig{
				CreatedSubPathMode: config.DefaultCreatedSubPathMode,
			}
			flagSet := flag.NewFlagSet("wrapper", flag.ContinueOnError)
			flagSet.SetOutput(io.Discard)
			bindWrapperFlags(flagSet, &cfg)

			if err := flagSet.Parse([]string{"--created-subpath-mode=" + value}); err == nil {
				t.Fatal("parse wrapper flags returned nil error, want invalid mode error")
			}
			if cfg.CreatedSubPathMode != config.DefaultCreatedSubPathMode {
				t.Fatalf("CreatedSubPathMode changed to %s after invalid input", config.FormatFileMode(cfg.CreatedSubPathMode))
			}
		})
	}
}
