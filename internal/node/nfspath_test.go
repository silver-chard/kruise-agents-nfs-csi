package node

import "testing"

func TestNormalizeNFSSubDir(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "empty"},
		{name: "slash", value: "/"},
		{name: "dot", value: "."},
		{name: "absolute child", value: "/teams/a", want: "teams/a"},
		{name: "relative child", value: "./teams//a", want: "teams/a"},
		{name: "parent", value: "teams/../a", wantErr: true},
		{name: "nul", value: "teams/\x00/a", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeNFSSubDir(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("NormalizeNFSSubDir(%q) succeeded, want error", test.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeNFSSubDir(%q): %v", test.value, err)
			}
			if got != test.want {
				t.Fatalf("NormalizeNFSSubDir(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestIsNFSExportRoot(t *testing.T) {
	tests := []struct {
		name       string
		pvSubDir   string
		requestSub string
		want       bool
	}{
		{name: "export root", want: true},
		{name: "request subpath", requestSub: "users/a"},
		{name: "pv subdir root", pvSubDir: "tenants/a"},
		{name: "nested under pv subdir", pvSubDir: "tenants/a", requestSub: "data"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := MountPlan{PV: PersistentVolume{SubDir: test.pvSubDir}, SourceSubPath: test.requestSub}
			if got := IsNFSExportRoot(plan); got != test.want {
				t.Fatalf("IsNFSExportRoot() = %t, want %t", got, test.want)
			}
		})
	}
}
