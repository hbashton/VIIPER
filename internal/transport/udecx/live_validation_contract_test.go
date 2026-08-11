package udecx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeLiveReleaseGateRequiresCompleteEvidence(t *testing.T) {
	root := filepath.Join("..", "..", "..", "native", "udecx", "tools")
	script, err := os.ReadFile(filepath.Join(root, "Invoke-ViiperUdeLiveValidation.ps1"))
	if err != nil {
		t.Fatalf("read native live validator: %v", err)
	}
	contract := strings.ReplaceAll(string(script), "\r\n", "\n")
	for _, required := range []string{
		"[switch]$ReleaseGate",
		"$SignatureValidationMode -ne 'Production'",
		"-RequireDriverVerifier is required",
		"-MediaProbePath is required",
		"-InputProbePath is required",
		"-ProbeManifestPath is required",
		"-RestartRootDevice is required",
		"-DisposableTestMachine is required",
		"$Iterations -lt 3",
		"$MediaDurationSeconds -lt 180",
		"VIIPER_UDE_LIVE_MEDIA_SECONDS",
		"Confirm-SecureBootUEFI",
		"$build -lt 22000",
		"0x001209BB",
		"Driver Verifier must target only ViiperUde.sys",
		"Test-LiveProbeManifest",
		"sourceRevision",
		"Get-FileHash -LiteralPath $path -Algorithm SHA256",
		"-ProbeManifestPath is required whenever a source-bound live probe is used",
		"[ValidateSet('LocalTest', 'ControlledTest', 'Production')]",
		"$SignatureValidationMode -eq 'LocalTest'",
		"testsigning Yes",
		"-LocalTestCertificatePath $LocalTestCertificatePath",
		"rev-parse --verify HEAD",
		"status --porcelain=v1 --untracked-files=all",
		"submodule status --recursive",
		"$env:GOFLAGS = '-mod=readonly'",
		"$env:GOWORK = 'off'",
		"$env:GOENV = 'off'",
		"$env:GOTOOLCHAIN = 'local'",
		"$env:GOOS = 'windows'",
		"$env:GOARCH = 'amd64'",
		"$env:CGO_ENABLED = '0'",
		"$go.Source env GOMOD",
		"$nativeIdentityLdflags",
		"internal/transport/udecx.nativeSourceRevision=",
		"$ExpectedSourceRevision.ToLowerInvariant()",
		"Win32_PnPEntity",
		"@($_.HardwareID) -contains 'ROOT\\VIIPER\\UDE'",
		"$ownedRootDevices[0].PNPDeviceID",
		"Go reported success without executing required live test",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("native release gate omitted %q", required)
		}
	}
	if strings.Contains(contract, "DeviceID -like 'ROOT\\VIIPER\\UDE*'") {
		t.Fatal("native release gate confuses the INF hardware ID with the generated PnP instance ID")
	}
}

func TestNativeMediaProbeRejectsObservableDiscontinuity(t *testing.T) {
	path := filepath.Join("..", "..", "..", "native", "udecx", "tools",
		"ViiperUdeMediaProbe.cpp")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native media probe: %v", err)
	}
	contract := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, required := range []string{
		"AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY",
		"AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR",
		"positionRegressions",
		"qpcRegressions",
		"renderStats.underruns != 0",
		"ValidateFrameCount(\"render\"",
		"ValidateFrameCount(\"capture\"",
		"captureStats.nonSilentFrames < captureStats.frames / 2",
		"seconds > 300",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("native media probe omitted %q", required)
		}
	}
}

func TestNativeLiveSoakKeepsMediaInputAndFeedbackConcurrent(t *testing.T) {
	path := filepath.Join("..", "..", "server", "usb", "native_live_windows_test.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native live integration test: %v", err)
	}
	contract := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, required := range []string{
		"startLiveProbe(",
		"mediaCtx, mediaProbe, \"exercise\"",
		"armLiveNativeMediaWitness(dev)",
		"mediaWitness.startMicrophone(mediaCtx)",
		"mediaWitness.validate(mediaDuration)",
		"publishInput(sequence)",
		"if feedbackController {\n\t\t\t\t\t\tverifyFeedback()",
		"3*mediaDuration+2*time.Minute",
		"controller.name == \"DualSenseEdge\"",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("native concurrent media soak omitted %q", required)
		}
	}
}

func TestNativePerformanceTraceCapturesAttributableCriticalPath(t *testing.T) {
	path := filepath.Join("..", "..", "..", "native", "udecx", "tools",
		"Invoke-ViiperUdePerformanceValidation.ps1")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native performance validator: %v", err)
	}
	contract := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, required := range []string{
		"[string]$ProbeManifestPath",
		"ProbeManifestPath      = $ProbeManifestPath",
		"$profile = 'GeneralProfile.Verbose'",
		"GeneralProfile\\.Verbose\\.Memory",
		"@('DPC', 'Interrupt', 'WDFDPC', 'WDFInterrupt')",
		"@('CSwitch', 'ReadyThread', 'SampledProfile')",
		"Count -lt 2",
		"Dropped Event\\s*:\\s*(?<count>\\d+)",
		"$resolvedOutput.evidence.json",
		"analysisRequired = $true",
		"Performance acceptance still requires WPA analysis",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("native performance trace contract omitted %q", required)
		}
	}
}

func TestNativeWorkflowPublishesSourceBoundLiveProbes(t *testing.T) {
	path := filepath.Join("..", "..", "..", ".github", "workflows", "native-ude.yml")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native workflow: %v", err)
	}
	contract := strings.ReplaceAll(string(source), "\r\n", "\n")
	for _, required := range []string{
		"schemaVersion = 1",
		"sourceRevision = $env:GITHUB_SHA.ToLowerInvariant()",
		"'ViiperUdeMediaProbe.exe' = (Get-FileHash",
		"'ViiperUdeInputProbe.exe' = (Get-FileHash",
		"ViiperUdeLiveProbes.manifest.json",
		"ViiperUdeLiveProbes-windows-amd64-${{ github.sha }}",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("native workflow source-bound probes omitted %q", required)
		}
	}
}
