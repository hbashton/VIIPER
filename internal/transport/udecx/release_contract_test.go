package udecx

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

func TestNativeReleaseBuildIdentityIsExplicitAndSourceBound(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	read := func(parts ...string) string {
		t.Helper()
		contents, err := os.ReadFile(filepath.Join(append([]string{root}, parts...)...))
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Join(parts...), err)
		}
		return string(contents)
	}

	project := read("native", "udecx", "driver", "ViiperUde.vcxproj")
	for _, required := range []string{
		"$(VIIPER_NATIVE_SOURCE_REVISION)",
		"GenerateViiperUdeBuildIdentity",
		`BeforeTargets="ClCompile"`,
		"Get-ViiperUdeBuildIdentity.ps1",
		`-OutputHeaderPath &quot;$(IntDir)ViiperUdeBuildIdentity.g.h&quot;`,
		`<AdditionalIncludeDirectories>$(IntDir);`,
		"fails closed without an explicit source revision",
	} {
		if !strings.Contains(project, required) {
			t.Fatalf("driver build omits fail-closed identity contract %q", required)
		}
	}

	for _, workflow := range []string{"build_base.yml", "native-ude.yml"} {
		contents := read(".github", "workflows", workflow)
		if !strings.Contains(contents, "VIIPER_NATIVE_SOURCE_REVISION: ${{ github.sha }}") {
			t.Fatalf("%s does not inject the exact workflow source SHA", workflow)
		}
	}

	justfile := read("justfile")
	if !strings.Contains(justfile, "Release builds require explicit VIIPER_NATIVE_SOURCE_REVISION.") ||
		!strings.Contains(justfile, "internal/transport/udecx.nativeSourceRevision=") {
		t.Fatal("release broker build can silently omit its source-bound native identity")
	}

	ioctl := read("native", "udecx", "driver", "Ioctl.c")
	if !strings.Contains(ioctl, "ViiperUdeBuildIdentity.g.h") ||
		!strings.Contains(ioctl, "output->BuildIdentity") {
		t.Fatal("kernel negotiation does not return the generated loaded-image identity")
	}

	for _, script := range []string{
		"New-ViiperUdeAttestationPackage.ps1",
		"Test-ViiperUdeSignedPackage.ps1",
		"Test-ViiperUdeReleaseBundle.ps1",
	} {
		contents := read("native", "udecx", "tools", script)
		for _, required := range []string{"Get-ViiperUdeBuildIdentity.ps1", "driverBuildIdentity"} {
			if !strings.Contains(contents, required) {
				t.Fatalf("%s omits package identity binding %q", script, required)
			}
		}
	}
}
