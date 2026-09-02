package wrapper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silver-chard/kruise-agents-nfs-csi/internal/node"
)

func TestExportRootAuthorizer(t *testing.T) {
	key := strings.Repeat("a", minimumExportRootKeyLength)
	keyFile := filepath.Join(t.TempDir(), "export-root-key")
	if err := os.WriteFile(keyFile, []byte(key+"\n"), 0o600); err != nil {
		t.Fatalf("write export root key: %v", err)
	}
	authorizer, err := loadExportRootAuthorizer(keyFile)
	if err != nil {
		t.Fatalf("loadExportRootAuthorizer: %v", err)
	}

	tests := []struct {
		name        string
		plan        node.MountPlan
		providedKey string
		wantRoot    bool
		wantErr     bool
	}{
		{name: "export root with key", providedKey: key, wantRoot: true},
		{name: "export root without key", wantErr: true},
		{name: "export root wrong key", providedKey: strings.Repeat("b", minimumExportRootKeyLength), wantErr: true},
		{name: "request subpath", plan: node.MountPlan{SourceSubPath: "users/a"}},
		{name: "pv subdir root", plan: node.MountPlan{PV: node.PersistentVolume{SubDir: "tenants/a"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fingerprint, err := authorizer.authorize(test.plan, test.providedKey)
			if test.wantErr {
				if err == nil {
					t.Fatal("authorize succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("authorize returned error: %v", err)
			}
			if gotRoot := fingerprint != ""; gotRoot != test.wantRoot {
				t.Fatalf("root authorization = %t, want %t", gotRoot, test.wantRoot)
			}
			if test.wantRoot && !authorizer.authorizesFingerprint(fingerprint) {
				t.Fatal("current authorizer rejected its own fingerprint")
			}
		})
	}
	if authorizer.authorizesFingerprint(strings.Repeat("f", 64)) {
		t.Fatal("authorizer accepted a different fingerprint")
	}
}

func TestExportRootAuthorizerWithoutConfiguredKeyAllowsExportRoot(t *testing.T) {
	authorizer, err := loadExportRootAuthorizer("")
	if err != nil {
		t.Fatalf("loadExportRootAuthorizer: %v", err)
	}
	for _, providedKey := range []string{"", strings.Repeat("unused", 8)} {
		fingerprint, err := authorizer.authorize(node.MountPlan{}, providedKey)
		if err != nil {
			t.Fatalf("authorize export root with provided key %q: %v", providedKey, err)
		}
		if fingerprint != "" {
			t.Fatalf("fingerprint = %q, want empty without a configured key", fingerprint)
		}
	}
	if authorizer.authorizesFingerprint(strings.Repeat("f", 64)) {
		t.Fatal("unconfigured authorizer accepted a fingerprint")
	}
}

func TestLoadExportRootAuthorizerRejectsInvalidKeyFile(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "empty"},
		{name: "too short", data: "too-short"},
		{name: "too long", data: strings.Repeat("a", maximumExportRootKeyLength+1)},
		{name: "non visible ASCII", data: strings.Repeat("a", minimumExportRootKeyLength-1) + "\x7f"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			keyFile := filepath.Join(t.TempDir(), "export-root-key")
			if err := os.WriteFile(keyFile, []byte(test.data), 0o600); err != nil {
				t.Fatalf("write export root key: %v", err)
			}
			if _, err := loadExportRootAuthorizer(keyFile); err == nil {
				t.Fatal("loadExportRootAuthorizer succeeded, want error")
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		if _, err := loadExportRootAuthorizer(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatal("loadExportRootAuthorizer succeeded, want error")
		}
	})
}
