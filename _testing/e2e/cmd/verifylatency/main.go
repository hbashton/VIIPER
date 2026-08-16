package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Alia5/VIIPER/_testing/e2e/latency"
)

func main() {
	var input, markersPath, tracePath, source, sdlRevision, sdlHash, manifestHash, driverHash, driverBuildIdentity, profileHash string
	var usbipRuntimeHash, packageValidationMode, localTestCertificateSHA string
	var orientation, cycleID string
	var samples, cycleIndex, cycleCount int
	flag.StringVar(&input, "input", "", "latency suite JSON")
	flag.StringVar(&markersPath, "markers", "", "decoded ETL TraceLogging marker JSON")
	flag.StringVar(&tracePath, "trace", "", "raw sequential ETL bound by marker JSON")
	flag.StringVar(&source, "source", "", "expected repository revision")
	flag.StringVar(&sdlRevision, "sdl-revision", "", "expected SDL revision")
	flag.StringVar(&sdlHash, "sdl-sha256", "", "expected loaded SDL SHA-256")
	flag.StringVar(&manifestHash, "manifest-sha256", "", "expected package manifest SHA-256")
	flag.StringVar(&driverHash, "driver-sha256", "", "expected installed driver SHA-256")
	flag.StringVar(&driverBuildIdentity, "driver-build-identity", "", "expected negotiated loaded-driver identity")
	flag.StringVar(&profileHash, "trace-profile-sha256", "", "expected WPRP SHA-256")
	flag.StringVar(&usbipRuntimeHash, "usbip-runtime-sha256", "", "expected exact USB/IP runtime provenance SHA-256")
	flag.StringVar(&packageValidationMode, "package-validation-mode", "", "expected production or local-test package gate")
	flag.StringVar(&localTestCertificateSHA, "local-test-certificate-sha256", "", "expected local-test certificate SHA-256")
	flag.StringVar(&orientation, "orientation", "", "expected balanced schedule orientation")
	flag.StringVar(&cycleID, "cycle-id", "", "expected balanced matrix cycle ID")
	flag.IntVar(&cycleIndex, "cycle-index", 0, "expected balanced matrix cycle index")
	flag.IntVar(&cycleCount, "cycle-count", 0, "expected balanced matrix cycle count")
	flag.IntVar(&samples, "samples", 0, "expected sample pairs per controller/transport")
	flag.Parse()
	if input == "" || markersPath == "" || tracePath == "" || source == "" || sdlRevision == "" || sdlHash == "" ||
		manifestHash == "" || driverHash == "" || driverBuildIdentity == "" || profileHash == "" || usbipRuntimeHash == "" ||
		packageValidationMode == "" || localTestCertificateSHA == "" || orientation == "" ||
		cycleID == "" || cycleIndex == 0 || cycleCount == 0 || samples == 0 {
		fail(errors.New("all verifier flags are required"))
	}
	expectedLocalTestCertificateSHA := strings.ToLower(localTestCertificateSHA)
	if strings.ToLower(packageValidationMode) == latency.PackageValidationProduction &&
		expectedLocalTestCertificateSHA == "none" {
		expectedLocalTestCertificateSHA = ""
	}
	file, err := os.Open(input)
	if err != nil {
		fail(err)
	}
	defer file.Close()
	suite, err := latency.ParseSuiteReport(file)
	if err != nil {
		fail(err)
	}
	if err = latency.RequireSuitePass(suite); err != nil {
		fail(err)
	}
	p := suite.Provenance
	if p.SourceRevision != strings.ToLower(source) ||
		p.SDLSourceRevision != strings.ToLower(sdlRevision) ||
		p.SDLBinarySHA256 != strings.ToLower(sdlHash) ||
		p.NativePackageManifestSHA256 != strings.ToLower(manifestHash) ||
		p.NativePackageValidationMode != strings.ToLower(packageValidationMode) ||
		p.NativeLocalTestCertificateSHA256 != expectedLocalTestCertificateSHA ||
		p.NativeDriverSHA256 != strings.ToLower(driverHash) ||
		p.NativeDriverBuildIdentity != strings.ToLower(driverBuildIdentity) ||
		p.USBIPRuntime.CaptureSHA256 != strings.ToLower(usbipRuntimeHash) ||
		p.TraceProfileSHA256 != strings.ToLower(profileHash) ||
		p.TraceProviderName != latency.TraceProviderName ||
		p.TraceProviderGUID != latency.TraceProviderGUID ||
		p.USBIPBaselineMode != latency.USBIPBaselineMode ||
		p.USBIPBaselineVersion != latency.USBIPBaselineVersion {
		fail(errors.New("suite provenance does not match the production invocation"))
	}
	for _, controllerCase := range suite.Cases {
		workload := controllerCase.Workload
		if workload.SamplePairs != samples ||
			workload.ScheduleOrientation != strings.ToLower(orientation) ||
			workload.CycleID != strings.ToLower(cycleID) ||
			workload.CycleIndex != cycleIndex || workload.CycleCount != cycleCount {
			fail(fmt.Errorf("%s workload is not bound to the requested sample and balanced-cycle identity",
				workload.ControllerType))
		}
	}
	markersFile, err := os.Open(markersPath)
	if err != nil {
		fail(err)
	}
	defer markersFile.Close()
	markerEvidence, err := latency.ParseTraceMarkerEvidence(markersFile)
	if err != nil {
		fail(err)
	}
	if err = latency.VerifyTraceMarkerSource(markerEvidence, tracePath); err != nil {
		fail(err)
	}
	if err = latency.VerifyTraceMarkers(suite, markerEvidence.Markers); err != nil {
		fail(err)
	}
	fmt.Printf("strictly verified %d controller cases\n", len(suite.Cases))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "latency evidence rejected:", err)
	os.Exit(1)
}
