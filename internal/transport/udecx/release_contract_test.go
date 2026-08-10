package udecx

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestExpectedDriverPackageVersionMatchesProject(t *testing.T) {
	projectPath := filepath.Join("..", "..", "..", "native", "udecx", "driver", "ViiperUde.vcxproj")
	project, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatalf("read native driver project: %v", err)
	}
	matches := regexp.MustCompile(`<ViiperUdeDriverVersion>([^<]+)</ViiperUdeDriverVersion>`).FindAllSubmatch(project, -1)
	if len(matches) != 1 || len(matches[0]) != 2 {
		t.Fatal("native driver project has no single release version contract")
	}
	if got := string(matches[0][1]); got != DriverPackageVersion {
		t.Fatalf("native package version drift: Go=%q project=%q", DriverPackageVersion, got)
	}
}
