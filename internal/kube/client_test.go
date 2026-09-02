package kube

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadBearerTokenFileObservesRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token-one\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if got, err := readBearerTokenFile(path); err != nil || got != "token-one" {
		t.Fatalf("first token = %q, err=%v", got, err)
	}
	if err := os.WriteFile(path, []byte("token-two\n"), 0o600); err != nil {
		t.Fatalf("rotate token file: %v", err)
	}
	if got, err := readBearerTokenFile(path); err != nil || got != "token-two" {
		t.Fatalf("rotated token = %q, err=%v", got, err)
	}
}

func TestReadBearerTokenFileRejectsEmptyToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	if _, err := readBearerTokenFile(path); err == nil {
		t.Fatal("readBearerTokenFile succeeded, want error")
	}
}
