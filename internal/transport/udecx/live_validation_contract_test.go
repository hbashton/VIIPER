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
	contract := string(script)
	for _, required := range []string{
		"[switch]$ReleaseGate",
		"$SignatureValidationMode -ne 'Production'",
		"-RequireDriverVerifier is required",
		"-MediaProbePath is required",
		"-InputProbePath is required",
		"-RestartRootDevice is required",
		"-DisposableTestMachine is required",
		"$Iterations -lt 3",
		"$MediaDurationSeconds -lt 180",
		"VIIPER_UDE_LIVE_MEDIA_SECONDS",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("native release gate omitted %q", required)
		}
	}
}

func TestNativeMediaProbeRejectsObservableDiscontinuity(t *testing.T) {
	path := filepath.Join("..", "..", "..", "native", "udecx", "tools",
		"ViiperUdeMediaProbe.cpp")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read native media probe: %v", err)
	}
	contract := string(source)
	for _, required := range []string{
		"AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY",
		"AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR",
		"positionRegressions",
		"qpcRegressions",
		"renderStats.underruns != 0",
		"ValidateFrameCount(\"render\"",
		"ValidateFrameCount(\"capture\"",
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
	contract := string(source)
	for _, required := range []string{
		"startLiveProbe(",
		"mediaCtx, mediaProbe, \"exercise\"",
		"publishInput(sequence)",
		"if feedbackController {\n\t\t\t\t\t\tverifyFeedback()",
		"2*mediaDuration+2*time.Minute",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("native concurrent media soak omitted %q", required)
		}
	}
}
