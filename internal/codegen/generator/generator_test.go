package generator

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverDevicePackagePathsSkipsInternalPackages(t *testing.T) {
	deviceBaseDir := t.TempDir()
	for _, name := range []string{"dualsense", "internal", "xbox360"} {
		if err := os.Mkdir(filepath.Join(deviceBaseDir, name), 0o755); err != nil {
			t.Fatalf("create test directory %q: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(deviceBaseDir, "report.go"), nil, 0o644); err != nil {
		t.Fatalf("create test file: %v", err)
	}

	got, err := discoverDevicePackagePaths(deviceBaseDir)
	if err != nil {
		t.Fatalf("discover device packages: %v", err)
	}

	want := []string{
		filepath.Join(deviceBaseDir, "dualsense"),
		filepath.Join(deviceBaseDir, "xbox360"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("discovered packages = %v, want %v", got, want)
	}
}
